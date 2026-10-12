// Package manager 维护每个节点的连接，负责下发期望状态与周期性采集。
package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"pveforward/internal/proto"
	"pveforward/internal/secret"
	"pveforward/internal/sshx"
	"pveforward/internal/store"
)

func timeoutFor(method string) time.Duration {
	switch method {
	case "init_network":
		return 10 * time.Minute
	case "apply", "collect", "detect":
		return 90 * time.Second
	}
	return 60 * time.Second
}

// Status 节点运行状态
type Status struct {
	State  string        `json:"state"` // waiting | pending | online | offline
	Error  string        `json:"error,omitempty"`
	Report *proto.Report `json:"report,omitempty"`
	Detect *proto.Detect `json:"detect,omitempty"`
	Since  time.Time     `json:"since"`
}

type runtime struct {
	id      int64
	mu      sync.Mutex
	drv     Driver
	agent   *AgentConn
	online  bool
	everOK  bool
	lastErr string
	since   time.Time
	report  *proto.Report
	detect  *proto.Detect
	last    map[int64]proto.Counter // 上次读到的计数器
	fails   map[int64]int           // 规则连续探测失败次数
	ruleErr map[int64]string
	kick    chan struct{}
	cancel  context.CancelFunc
	busy    sync.Mutex // 串行化对节点的操作（采集 / 下发 / 初始化）
}

type Manager struct {
	db  *store.DB
	box *secret.Box

	mu        sync.Mutex
	nodes     map[int64]*runtime
	overQuota map[int64]bool

	Interval func() time.Duration
	root     context.Context

	liveMu sync.Mutex
	live   map[time.Duration]time.Time // 监控页请求的采集间隔 → 有效期
	wg     sync.WaitGroup              // 节点循环
}

func New(db *store.DB, box *secret.Box) *Manager {
	return &Manager{db: db, box: box, nodes: map[int64]*runtime{}, overQuota: map[int64]bool{}, live: map[time.Duration]time.Time{},
		Interval: func() time.Duration { return 60 * time.Second }}
}

// Start 为所有节点启动后台循环。
func (m *Manager) Start(ctx context.Context) error {
	m.root = ctx
	users, err := m.db.Users()
	if err != nil {
		return err
	}
	for _, u := range users {
		m.overQuota[u.ID] = u.OverQuota()
	}
	nodes, err := m.db.Nodes()
	if err != nil {
		return err
	}
	for _, n := range nodes {
		m.AddNode(n.ID)
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.db.Cleanup(ctx)
			}
		}
	}()
	return nil
}

func (m *Manager) rt(id int64) *runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nodes[id]
}

// AddNode 为节点启动循环（已存在则重启，用于连接参数变化后）。
func (m *Manager) AddNode(id int64) {
	m.RemoveNode(id)
	root := m.root
	if root == nil {
		root = context.Background()
	}
	cctx, cancel := context.WithCancel(root)
	r := &runtime{id: id, kick: make(chan struct{}, 1), cancel: cancel, last: map[int64]proto.Counter{},
		fails: map[int64]int{}, ruleErr: map[int64]string{}, since: time.Now()}
	m.mu.Lock()
	m.nodes[id] = r
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.loop(cctx, r)
	}()
}

// Wait 在 Start 的上下文取消后等待所有节点循环退出（最多 timeout），
// 返回是否全部退出。用于在线更新替换进程前，避免中断正在下发的规则或写入的流量。
func (m *Manager) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (m *Manager) RemoveNode(id int64) {
	m.mu.Lock()
	r := m.nodes[id]
	delete(m.nodes, id)
	m.mu.Unlock()
	if r != nil {
		r.cancel()
		r.mu.Lock()
		if r.drv != nil {
			r.drv.Close()
		}
		r.mu.Unlock()
	}
}

// Kick 让节点尽快执行一轮下发与采集。
// Live 由监控页在刷新时调用：在接下来一段时间内按 every 采集（不低于 3 秒），
// 页面关闭后自动恢复为系统设置中的采集间隔。
func (m *Manager) Live(every time.Duration) {
	every = max(every, 3*time.Second)
	before := m.interval()
	m.liveMu.Lock()
	m.live[every] = time.Now().Add(max(15*time.Second, 3*every))
	m.liveMu.Unlock()
	if m.interval() < before {
		// 间隔变短：立即唤醒各节点，不必等到下一次常规采集
		m.mu.Lock()
		ids := make([]int64, 0, len(m.nodes))
		for id := range m.nodes {
			ids = append(ids, id)
		}
		m.mu.Unlock()
		for _, id := range ids {
			m.Kick(id)
		}
	}
}

// interval 返回当前的采集间隔：系统设置的间隔，或监控页正在请求的更短间隔。
func (m *Manager) interval() time.Duration {
	iv := m.Interval()
	now := time.Now()
	m.liveMu.Lock()
	defer m.liveMu.Unlock()
	for every, until := range m.live {
		if now.After(until) {
			delete(m.live, every)
		} else if every < iv {
			iv = every
		}
	}
	return iv
}

func (m *Manager) Kick(id int64) {
	if r := m.rt(id); r != nil {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// Push 增加期望状态版本并触发下发。
func (m *Manager) Push(ids ...int64) {
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		m.db.BumpState(id)
		m.Kick(id)
	}
}

// PushUser 用户相关变化（启用、配额）影响其所有节点。
func (m *Manager) PushUser(uid int64) {
	rows, err := m.db.Query(`SELECT DISTINCT node_id FROM rules WHERE user_id=?`, uid)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	m.Push(ids...)
}

// AttachAgent 在 Agent 连接期间把它作为节点的驱动，阻塞直到连接断开。
func (m *Manager) AttachAgent(ctx context.Context, id int64, c *AgentConn) {
	r := m.rt(id)
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.agent != nil {
		r.agent.Close()
	}
	r.agent = c
	r.drv = &agentDriver{c}
	r.mu.Unlock()
	m.Kick(id)
	select {
	case <-c.Done():
	case <-ctx.Done():
	}
	r.mu.Lock()
	if r.agent == c {
		r.agent, r.drv = nil, nil
		if r.online {
			r.online, r.since = false, time.Now()
			r.lastErr = "Agent 连接断开"
		}
	}
	r.mu.Unlock()
}

// Driver 返回节点当前可用的驱动。
func (m *Manager) Driver(id int64) (Driver, error) {
	r := m.rt(id)
	if r == nil {
		return nil, errors.New("节点不存在")
	}
	if n, err := m.db.Node(id); err == nil {
		m.ensureSSH(r, n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drv == nil {
		if r.lastErr != "" {
			return nil, errors.New("节点未连接：" + r.lastErr)
		}
		return nil, errors.New("节点未连接")
	}
	return r.drv, nil
}

// WithDriver 在串行锁内使用驱动，避免与后台采集 / 下发并发执行。
func (m *Manager) WithDriver(id int64, f func(Driver) error) error {
	r := m.rt(id)
	if r == nil {
		return errors.New("节点不存在")
	}
	d, err := m.Driver(id)
	if err != nil {
		return err
	}
	r.busy.Lock()
	defer r.busy.Unlock()
	return f(d)
}

func (m *Manager) Status(id int64) Status {
	r := m.rt(id)
	if r == nil {
		return Status{State: "waiting"}
	}
	n, err := m.db.Node(id)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Status{Error: r.lastErr, Report: r.report, Detect: r.detect, Since: r.since}
	switch {
	case err == nil && !n.Initialized && r.drv != nil && r.everOK:
		s.State = "pending"
	case err == nil && !n.Initialized:
		s.State = "waiting"
	case r.online:
		s.State = "online"
	default:
		s.State = "offline"
	}
	return s
}

// RuleError 返回规则当前的异常原因。
func (m *Manager) RuleError(nodeID, ruleID int64) string {
	r := m.rt(nodeID)
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ruleErr[ruleID]
}

// SetDetect 保存检测结果（供界面展示）
func (m *Manager) SetDetect(id int64, d *proto.Detect) {
	if r := m.rt(id); r != nil {
		r.mu.Lock()
		r.detect = d
		r.mu.Unlock()
	}
}

// sshConfig 从数据库构造节点的 SSH 配置。
func (m *Manager) SSHConfig(n *store.Node) (sshx.Config, error) {
	c := sshx.Config{Host: n.SSHHost, Port: n.SSHPort, User: n.SSHUser, HostKey: n.HostKey}
	if n.SSHAuth == "cred" {
		if n.CredID == nil {
			return c, errors.New("节点未关联凭证")
		}
		cr, err := m.db.Credential(*n.CredID)
		if err != nil {
			return c, fmt.Errorf("凭证不存在: %w", err)
		}
		c.User = cr.Username
		s, err := m.box.Open(cr.SecretEnc)
		if err != nil {
			return c, err
		}
		p, _ := m.box.Open(cr.PassphraseEnc)
		if cr.Type == "key" {
			c.PrivateKey, c.Passphrase = s, p
		} else {
			c.Password = s
		}
		return c, nil
	}
	s, err := m.box.Open(n.SSHSecretEnc)
	if err != nil {
		return c, err
	}
	p, _ := m.box.Open(n.SSHPassEnc)
	if n.SSHAuth == "key" {
		c.PrivateKey, c.Passphrase = s, p
	} else {
		c.Password = s
	}
	return c, nil
}

func (m *Manager) loop(ctx context.Context, r *runtime) {
	backoff := 5 * time.Second
	for {
		wait := m.interval()
		if err := m.tick(ctx, r); err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = backoff
			if backoff < m.interval() {
				backoff *= 2
			}
		} else {
			backoff = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		case <-time.After(wait):
		}
	}
}

func (m *Manager) setErr(r *runtime, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		if !r.online {
			r.since = time.Now()
		}
		r.online, r.everOK, r.lastErr = true, true, ""
		return
	}
	if r.online {
		r.since = time.Now()
	}
	r.online, r.lastErr = false, err.Error()
}

// tick 执行一轮：确保连接 → 必要时下发 → 采集。
func (m *Manager) tick(ctx context.Context, r *runtime) error {
	n, err := m.db.Node(r.id)
	if err != nil {
		return err
	}
	wasOnline := m.isOnline(r)

	if err := m.ensureSSH(r, n); err != nil {
		m.setErr(r, err)
		return err
	}
	r.mu.Lock()
	drv := r.drv
	r.mu.Unlock()
	if drv == nil {
		err := errors.New("等待 Agent 连接")
		if n.Mode == "agent" && n.LastSeen > 0 {
			err = errors.New("Agent 未连接")
		}
		m.setErr(r, err)
		m.transition(n, wasOnline, false, err)
		return err
	}

	r.busy.Lock()
	defer r.busy.Unlock()

	var st *proto.DesiredState
	if n.Initialized {
		st, err = m.BuildState(n)
		if err != nil {
			m.setErr(r, err)
			return err
		}
		if n.AppliedVersion < n.StateVersion || !m.isOnline(r) {
			if err := m.apply(ctx, r, drv, n, st); err != nil {
				log.Printf("节点 %s 下发失败: %v", n.Name, err)
			}
		}
	}

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	rep, err := drv.Collect(cctx, st)
	cancel()
	if err != nil {
		m.setErr(r, err)
		m.resetDriverOnError(r, n, err)
		m.transition(n, wasOnline, false, err)
		return err
	}
	m.setErr(r, nil)
	m.db.Exec(`UPDATE nodes SET last_seen=? WHERE id=?`, time.Now().Unix(), n.ID)
	m.transition(n, wasOnline, true, nil)
	m.process(n, r, st, rep)
	return nil
}

// RunOnce 立即执行一轮下发与采集（同步），用于节点初始化完成后。
func (m *Manager) RunOnce(ctx context.Context, id int64) error {
	r := m.rt(id)
	if r == nil {
		return errors.New("节点不存在")
	}
	return m.tick(ctx, r)
}

// ensureSSH 为 SSH 节点创建驱动（尚未创建时）。
func (m *Manager) ensureSSH(r *runtime, n *store.Node) error {
	if n.Mode != "ssh" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drv != nil {
		return nil
	}
	if n.HostKey == "" {
		return errors.New("尚未信任主机指纹")
	}
	cfg, err := m.SSHConfig(n)
	if err != nil {
		return err
	}
	r.drv = NewSSHDriver(cfg)
	return nil
}

func (m *Manager) isOnline(r *runtime) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.online
}

func (m *Manager) resetDriverOnError(r *runtime, n *store.Node, err error) {
	if n.Mode != "ssh" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drv != nil {
		r.drv.Close()
		r.drv = nil
	}
}

func (m *Manager) transition(n *store.Node, was, now bool, err error) {
	if !n.Initialized || was == now {
		return
	}
	if now {
		m.db.AddEvent("info", 0, n.ID, fmt.Sprintf("节点 %s 已恢复在线", n.Name))
	} else {
		m.db.AddEvent("error", 0, n.ID, fmt.Sprintf("节点 %s 离线：%v", n.Name, err))
	}
}

func (m *Manager) apply(ctx context.Context, r *runtime, drv Driver, n *store.Node, st *proto.DesiredState) error {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := drv.Apply(cctx, st)
	if err != nil {
		m.db.Exec(`UPDATE nodes SET apply_error=? WHERE id=?`, err.Error(), n.ID)
		return err
	}
	// 替换前的计数器先入账，之后计数从 0 开始
	r.mu.Lock()
	deltas := counterDeltas(r.last, res.Before)
	r.last = map[int64]proto.Counter{}
	r.mu.Unlock()
	m.addTraffic(n.ID, deltas)
	m.db.Exec(`UPDATE nodes SET applied_version=?, apply_error='' WHERE id=?`, st.Version, n.ID)
	n.AppliedVersion = st.Version
	return nil
}

func counterDeltas(last, cur map[int64]proto.Counter) map[int64]proto.Counter {
	res := map[int64]proto.Counter{}
	for id, c := range cur {
		l := last[id]
		d := proto.Counter{Up: c.Up, Down: c.Down}
		if c.Up >= l.Up {
			d.Up = c.Up - l.Up
		}
		if c.Down >= l.Down {
			d.Down = c.Down - l.Down
		}
		res[id] = d
	}
	return res
}

func (m *Manager) addTraffic(nodeID int64, deltas map[int64]proto.Counter) {
	if err := m.db.AddTraffic(nodeID, deltas); err != nil {
		log.Printf("记录流量失败: %v", err)
		return
	}
	// 检查用户是否刚超出流量上限
	users, err := m.db.Users()
	if err != nil {
		return
	}
	for _, u := range users {
		over := u.OverQuota()
		m.mu.Lock()
		prev := m.overQuota[u.ID]
		m.overQuota[u.ID] = over
		m.mu.Unlock()
		if over && !prev {
			m.db.AddEvent("warn", u.ID, 0, fmt.Sprintf("%s 流量已达上限 %s，规则已自动停用", u.Username, humanBytes(u.QuotaBytes)))
			m.PushUser(u.ID)
		}
	}
}

// process 处理采集结果：同步虚拟机、累计流量、更新规则健康状态。
func (m *Manager) process(n *store.Node, r *runtime, st *proto.DesiredState, rep *proto.Report) {
	r.mu.Lock()
	r.report = rep
	r.mu.Unlock()

	if n.Initialized {
		changed, err := m.db.SyncVMs(n.ID, rep.VMs)
		if err != nil {
			log.Printf("同步虚拟机失败: %v", err)
		} else if changed {
			m.Push(n.ID)
		}
	}
	if st == nil {
		return
	}
	r.mu.Lock()
	deltas := counterDeltas(r.last, rep.Counters)
	r.last = rep.Counters
	r.mu.Unlock()
	m.addTraffic(n.ID, deltas)

	// 健康状态：目标不可达需连续 3 次失败才判定，端口冲突立即判定
	r.mu.Lock()
	newErr := map[int64]string{}
	for _, rule := range st.Rules {
		msg := rep.Health[rule.ID]
		if strings.HasPrefix(msg, "目标不可达") {
			r.fails[rule.ID]++
			if r.fails[rule.ID] < 3 {
				msg = r.ruleErr[rule.ID]
				if strings.HasPrefix(msg, "目标不可达") {
					// 维持原异常
				} else {
					msg = ""
				}
			} else {
				msg += "（连续 3 次）"
			}
		} else {
			delete(r.fails, rule.ID)
		}
		newErr[rule.ID] = msg
	}
	old := r.ruleErr
	r.ruleErr = newErr
	r.mu.Unlock()
	for id, msg := range newErr {
		if msg != old[id] {
			m.db.Exec(`UPDATE rules SET error_msg=? WHERE id=?`, msg, id)
			if msg != "" {
				var uid int64
				var port int
				m.db.QueryRow(`SELECT user_id, listen_port FROM rules WHERE id=?`, id).Scan(&uid, &port)
				m.db.AddEvent("error", uid, n.ID, fmt.Sprintf("%s:%d %s", n.Name, port, msg))
			}
		}
	}
}

// BuildState 根据数据库生成节点的期望状态，并记录无法生效的规则原因。
func (m *Manager) BuildState(n *store.Node) (*proto.DesiredState, error) {
	st := &proto.DesiredState{Version: n.StateVersion, Net: n.Net}
	vms, err := m.db.VMs(n.ID)
	if err != nil {
		return nil, err
	}
	vmByID := map[int64]*store.VM{}
	for _, v := range vms {
		vmByID[v.ID] = v
		if v.MAC != "" && v.IPv4 != "" && v.Present && (v.UserID != nil || v.Shared) {
			st.Hosts = append(st.Hosts, proto.DHCPHost{MAC: v.MAC, IP4: v.IPv4, IP6: v.IPv6, Name: fmt.Sprintf("vm%d", v.VMID)})
		}
	}
	users, err := m.db.Users()
	if err != nil {
		return nil, err
	}
	userByID := map[int64]*store.User{}
	for _, u := range users {
		userByID[u.ID] = u
	}
	rules, err := m.db.Rules(0, n.ID)
	if err != nil {
		return nil, err
	}
	for _, rl := range rules {
		reason := ""
		u := userByID[rl.UserID]
		v := vmByID[rl.VMID]
		switch {
		case !rl.Enabled:
		case u == nil || !u.Enabled:
			reason = "用户已禁用"
		case u.OverQuota():
			reason = "用户流量已超限"
		case v == nil || !v.Present:
			reason = "目标虚拟机已删除"
		case !v.Shared && (v.UserID == nil || *v.UserID != rl.UserID):
			reason = "目标虚拟机已不属于该用户"
		case rl.IPVer != "v6" && v.IPv4 == "":
			reason = "目标虚拟机没有 IPv4 地址"
		case rl.IPVer != "v4" && (v.IPv6 == "" || n.Net.CIDR6 == ""):
			reason = "目标虚拟机或节点没有 IPv6 地址"
		}
		if !rl.Enabled {
			continue
		}
		if reason != "" {
			if rl.Error != reason {
				m.db.Exec(`UPDATE rules SET error_msg=? WHERE id=?`, reason, rl.ID)
			}
			continue
		}
		if strings.HasPrefix(rl.Error, "用户") || strings.HasPrefix(rl.Error, "目标虚拟机") {
			m.db.Exec(`UPDATE rules SET error_msg='' WHERE id=?`, rl.ID)
		}
		st.Rules = append(st.Rules, proto.Rule{ID: rl.ID, Port: rl.Port, Proto: rl.Proto, IPVer: rl.IPVer,
			Target4: v.IPv4, Target6: v.IPv6, TargetPort: rl.TargetPort})
	}
	return st, nil
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
