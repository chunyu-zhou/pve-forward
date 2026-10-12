package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"pveforward/internal/manager"
	"pveforward/internal/proto"
	"pveforward/internal/store"
)

// ---------------- 资源监控 ----------------

type monNode struct {
	ID       int64               `json:"id"`
	Name     string              `json:"name"`
	Remark   string              `json:"remark"`
	Status   string              `json:"status"`
	LastSeen int64               `json:"lastSeen"`
	Metrics  *proto.NodeMetrics  `json:"metrics"`
	Disks    []proto.DiskMetrics `json:"disks"`
}

type monVM struct {
	ID       int64  `json:"id"`
	NodeID   int64  `json:"nodeId"`
	NodeName string `json:"nodeName"`
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Owner    string `json:"owner,omitempty"`
	Shared   bool   `json:"shared"`
	IPv4     string `json:"ipv4,omitempty"`
	IPv6     string `json:"ipv6,omitempty"`
	monVMMetrics
}

// Explicit allowlist: agent inventory fields (such as MAC) are not monitor data.
// Keep proto.VMInfo unchanged because the agent and VM inventory need them.
type monVMMetrics struct {
	CPU        float64 `json:"cpu"`
	Cores      int     `json:"cores"`
	MemUsed    uint64  `json:"memUsed"`
	MemTotal   uint64  `json:"memTotal"`
	DiskUsed   uint64  `json:"diskUsed"`
	DiskTotal  uint64  `json:"diskTotal"`
	Uptime     int64   `json:"uptime"`
	NetIn      float64 `json:"netIn"`
	NetOut     float64 `json:"netOut"`
	DiskR      float64 `json:"diskR"`
	DiskW      float64 `json:"diskW"`
	IOPS       float64 `json:"iops"`
	IOPressure float64 `json:"ioPressure"`
}

type monScope struct {
	nodes     map[int64]bool // nil 表示全部
	vmFilter  func(*store.VM) bool
	showOwner bool
	showIP    bool
	showNodes bool
}

func (s *Server) monitorData(sc monScope) map[string]any {
	nodes, _ := s.db.Nodes()
	users := map[int64]string{}
	if us, err := s.db.Users(); err == nil {
		for _, u := range us {
			users[u.ID] = u.Username
		}
	}
	statuses := map[int64]manager.Status{}
	outNodes := []monNode{}
	names := map[int64]string{}
	for _, n := range nodes {
		if !n.Initialized || (sc.nodes != nil && !sc.nodes[n.ID]) {
			continue
		}
		names[n.ID] = n.Name
		st := s.mgr.Status(n.ID)
		statuses[n.ID] = st
		if !sc.showNodes {
			continue
		}
		mn := monNode{ID: n.ID, Name: n.Name, Remark: n.Remark, Status: st.State, LastSeen: n.LastSeen, Disks: []proto.DiskMetrics{}}
		if st.Report != nil {
			m := st.Report.Node
			mn.Metrics = &m
			mn.Disks = st.Report.Disks
		}
		outNodes = append(outNodes, mn)
	}
	vms, _ := s.db.VMs(0)
	outVMs := []monVM{}
	for _, v := range vms {
		if _, ok := names[v.NodeID]; !ok || !v.Present || (sc.vmFilter != nil && !sc.vmFilter(v)) {
			continue
		}
		o := monVM{ID: v.ID, NodeID: v.NodeID, NodeName: names[v.NodeID], VMID: v.VMID, Name: v.Name, Type: v.Type, Status: v.Status, Shared: v.Shared}
		if sc.showOwner && v.UserID != nil {
			o.Owner = users[*v.UserID]
		}
		if sc.showIP {
			o.IPv4, o.IPv6 = v.IPv4, v.IPv6
		}
		st := statuses[v.NodeID]
		if rep := st.Report; rep != nil {
			for _, x := range rep.VMs {
				if x.VMID == v.VMID {
					// Configuration remains useful offline, but cached utilization is not live data.
					o.monVMMetrics = monVMMetrics{Cores: x.Cores, MemTotal: x.MemTotal, DiskTotal: x.DiskTotal}
					if st.State == "online" {
						o.CPU, o.MemUsed, o.DiskUsed, o.Uptime = x.CPU, x.MemUsed, x.DiskUsed, x.Uptime
						o.NetIn, o.NetOut, o.DiskR, o.DiskW = x.NetIn, x.NetOut, x.DiskR, x.DiskW
						o.IOPS, o.IOPressure = x.IOPS, x.IOPressure
						if x.Status != "" {
							o.Status = x.Status
						}
					}
					break
				}
			}
		}
		if st.State != "online" {
			o.Status = "offline"
		}
		outVMs = append(outVMs, o)
	}
	st := s.settings()
	return map[string]any{"nodes": outNodes, "vms": outVMs, "updated": time.Now().Unix(),
		"thresholds": map[string]int{"busyUtil": st.DiskBusyUtil, "satUtil": st.DiskSatUtil, "busyAwait": st.DiskBusyAwait, "satAwait": st.DiskSatAwait},
		"interval":   st.Interval}
}

func (s *Server) monitor(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sc := monScope{showOwner: true, showIP: true, showNodes: u.Role == "admin"}
	if u.Role != "admin" {
		sc.vmFilter = func(v *store.VM) bool { return v.UserID != nil && *v.UserID == u.ID }
	}
	writeJSON(w, s.monitorData(sc))
}

func (s *Server) rrd(w http.ResponseWriter, r *http.Request, nodeID int64, req proto.RRDReq) {
	var pts []proto.RRDPoint
	err := s.mgr.WithDriver(nodeID, func(d manager.Driver) error {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		var err error
		pts, err = d.RRD(ctx, req)
		return err
	})
	if err != nil {
		fail(w, bad("读取历史数据失败：%v", err))
		return
	}
	if pts == nil {
		pts = []proto.RRDPoint{}
	}
	writeJSON(w, pts)
}

// monitorVM applies the same inventory boundary as monitorData before an RRD call.
func (s *Server) monitorVM(id int64) (*store.VM, bool) {
	v, err := s.db.VM(id)
	if err != nil || !v.Present {
		return nil, false
	}
	n, err := s.db.Node(v.NodeID)
	if err != nil || !n.Initialized {
		return nil, false
	}
	return v, true
}

func (s *Server) monitorRRD(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	tf := r.URL.Query().Get("tf")
	if vid := queryInt(r, "vm"); vid > 0 {
		v, ok := s.monitorVM(vid)
		if !ok || (u.Role != "admin" && (v.UserID == nil || *v.UserID != u.ID)) {
			jsonErr(w, http.StatusNotFound, "虚拟机不存在")
			return
		}
		s.rrd(w, r, v.NodeID, proto.RRDReq{Type: v.Type, VMID: v.VMID, Timeframe: tf})
		return
	}
	if u.Role != "admin" {
		jsonErr(w, http.StatusForbidden, "需要管理员权限")
		return
	}
	s.rrd(w, r, queryInt(r, "node"), proto.RRDReq{Type: "node", Timeframe: tf})
}

// ---------------- 公开监控页 ----------------

type PublicCfg struct {
	Enabled    bool    `json:"enabled"`
	Slug       string  `json:"slug"`
	Access     string  `json:"access"` // open | password
	PwdHash    string  `json:"pwdHash,omitempty"`
	Title      string  `json:"title"`
	Desc       string  `json:"desc"`
	Refresh    int     `json:"refresh"`
	Nodes      []int64 `json:"nodes"`
	ShowNodes  bool    `json:"showNodes"`
	ShowVMs    bool    `json:"showVms"`
	ShowDetail bool    `json:"showDetail"`
	ShowIP     bool    `json:"showIp"`
	ShowOwner  bool    `json:"showOwner"`
	OnlyShared bool    `json:"onlyShared"`
}

func (s *Server) publicCfg() PublicCfg {
	c := PublicCfg{Access: "open", Title: s.settings().Site + " 状态", Desc: "节点与虚拟机实时资源状态", Refresh: 30,
		ShowNodes: true, ShowVMs: true, ShowDetail: true, Nodes: []int64{}}
	s.db.SettingJSON("public", &c)
	return c
}

// publicAuth 校验公开页访问；返回配置与是否允许。
func (s *Server) publicAuth(w http.ResponseWriter, r *http.Request) (PublicCfg, bool) {
	c := s.publicCfg()
	ip := clientIP(r)
	if s.limiter.Blocked("public:"+ip, 20, 10*time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "访问过于频繁")
		return c, false
	}
	if !s.setupDone() || !c.Enabled || c.Slug == "" || r.PathValue("slug") != c.Slug {
		jsonErr(w, http.StatusNotFound, "页面未开放")
		return c, false
	}
	if c.Access == "password" {
		pw := r.Header.Get("X-Access-Password")
		if pw == "" {
			w.Header().Set("X-Title", url.QueryEscape(c.Title))
			jsonErr(w, http.StatusUnauthorized, "需要访问密码")
			return c, false
		}
		if !checkPassword(c.PwdHash, pw) {
			s.limiter.Fail("public:" + ip)
			jsonErr(w, http.StatusUnauthorized, "访问密码不正确")
			return c, false
		}
	}
	return c, true
}

func (s *Server) publicVMFilter(c PublicCfg) func(*store.VM) bool {
	return func(v *store.VM) bool { return c.ShowVMs && (!c.OnlyShared || v.Shared) }
}

func (s *Server) publicStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := s.publicAuth(w, r)
	if !ok {
		return
	}
	nodes := map[int64]bool{}
	for _, id := range c.Nodes {
		nodes[id] = true
	}
	d := s.monitorData(monScope{nodes: nodes, vmFilter: s.publicVMFilter(c), showOwner: c.ShowOwner, showIP: c.ShowIP, showNodes: c.ShowNodes})
	if !c.ShowNodes {
		// 不展示节点卡片时仍需节点名用于分组
		var ns []map[string]any
		for _, id := range c.Nodes {
			if n, err := s.db.Node(id); err == nil {
				ns = append(ns, map[string]any{"id": n.ID, "name": n.Name, "status": s.mgr.Status(n.ID).State})
			}
		}
		d["nodeNames"] = ns
	}
	d["page"] = map[string]any{"title": c.Title, "desc": c.Desc, "refresh": c.Refresh, "showNodes": c.ShowNodes, "showVms": c.ShowVMs,
		"showDetail": c.ShowDetail, "showIp": c.ShowIP, "showOwner": c.ShowOwner}
	writeJSON(w, d)
}

func (s *Server) publicRRD(w http.ResponseWriter, r *http.Request) {
	c, ok := s.publicAuth(w, r)
	if !ok {
		return
	}
	v, visible := s.monitorVM(queryInt(r, "vm"))
	allowed := false
	for _, id := range c.Nodes {
		if visible && id == v.NodeID {
			allowed = true
		}
	}
	if !c.ShowDetail || !allowed || !s.publicVMFilter(c)(v) {
		jsonErr(w, http.StatusNotFound, "不可访问")
		return
	}
	s.rrd(w, r, v.NodeID, proto.RRDReq{Type: v.Type, VMID: v.VMID, Timeframe: r.URL.Query().Get("tf")})
}

// ---------------- 仪表盘 ----------------

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	admin := u.Role == "admin"
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()

	// 流量序列：近 24 小时按小时、近 7 天按天
	tf := r.URL.Query().Get("range")
	nodeID := queryInt(r, "node")
	var since int64
	step := int64(3600)
	n := 24
	if tf == "7d" {
		step, n = 86400, 7
		since = midnight - 6*86400
	} else {
		since = now.Truncate(time.Hour).Unix() - 23*3600
	}
	var q string
	args := []any{since}
	if admin {
		q = `SELECT hour, SUM(bytes_up), SUM(bytes_down) FROM node_traffic WHERE hour>=?`
		if nodeID > 0 {
			q += ` AND node_id=?`
			args = append(args, nodeID)
		}
	} else {
		q = `SELECT t.hour, SUM(t.bytes_up), SUM(t.bytes_down) FROM traffic t JOIN rules r ON r.id=t.rule_id WHERE t.hour>=? AND r.user_id=?`
		args = append(args, u.ID)
		if nodeID > 0 {
			q += ` AND r.node_id=?`
			args = append(args, nodeID)
		}
	}
	series := make([]map[string]int64, n)
	for i := range series {
		series[i] = map[string]int64{"t": since + int64(i)*step, "up": 0, "down": 0}
	}
	rows, err := s.db.Query(q+` GROUP BY 1`, args...)
	if err == nil {
		for rows.Next() {
			var h, up, down int64
			rows.Scan(&h, &up, &down)
			i := int((h - since) / step)
			if tf == "7d" {
				day := time.Unix(h, 0)
				i = int((time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, now.Location()).Unix() - since) / step)
			}
			if i >= 0 && i < n {
				series[i]["up"] += up
				series[i]["down"] += down
			}
		}
		rows.Close()
	}

	// 今日流量
	var todayUp, todayDown int64
	if admin {
		s.db.QueryRow(`SELECT COALESCE(SUM(bytes_up),0), COALESCE(SUM(bytes_down),0) FROM node_traffic WHERE hour>=?`, midnight).Scan(&todayUp, &todayDown)
	} else {
		s.db.QueryRow(`SELECT COALESCE(SUM(t.bytes_up),0), COALESCE(SUM(t.bytes_down),0) FROM traffic t JOIN rules r ON r.id=t.rule_id WHERE t.hour>=? AND r.user_id=?`, midnight, u.ID).Scan(&todayUp, &todayDown)
	}
	var totalUp, totalDown int64
	tq := `SELECT COALESCE(SUM(bytes_up),0), COALESCE(SUM(bytes_down),0) FROM rules`
	if admin {
		s.db.QueryRow(tq).Scan(&totalUp, &totalDown)
	} else {
		s.db.QueryRow(tq+` WHERE user_id=?`, u.ID).Scan(&totalUp, &totalDown)
	}

	// 排行：近 24 小时与前一个 24 小时比较
	cur := map[int64]int64{}
	prev := map[int64]int64{}
	rq := `SELECT t.rule_id, SUM(CASE WHEN t.hour>=? THEN t.bytes_up+t.bytes_down ELSE 0 END), SUM(CASE WHEN t.hour<? THEN t.bytes_up+t.bytes_down ELSE 0 END)
		FROM traffic t JOIN rules r ON r.id=t.rule_id WHERE t.hour>=?`
	d1 := now.Add(-24 * time.Hour).Unix()
	rargs := []any{d1, d1, now.Add(-48 * time.Hour).Unix()}
	if !admin {
		rq += ` AND r.user_id=?`
		rargs = append(rargs, u.ID)
	}
	if rows, err := s.db.Query(rq+` GROUP BY t.rule_id`, rargs...); err == nil {
		for rows.Next() {
			var id, c, p int64
			rows.Scan(&id, &c, &p)
			cur[id], prev[id] = c, p
		}
		rows.Close()
	}
	type rank struct {
		RuleID int64   `json:"ruleId"`
		Bytes  int64   `json:"bytes"`
		Change float64 `json:"change"`
	}
	var ranking []rank
	for id, c := range cur {
		ch := 0.0
		if prev[id] > 0 {
			ch = float64(c-prev[id]) / float64(prev[id]) * 100
		}
		ranking = append(ranking, rank{id, c, ch})
	}
	sort.Slice(ranking, func(i, j int) bool { return ranking[i].Bytes > ranking[j].Bytes })
	if len(ranking) > 20 {
		ranking = ranking[:20]
	}

	uid := int64(0)
	if !admin {
		uid = u.ID
	}
	events, _ := s.db.Events(uid, 12)
	if events == nil {
		events = []*store.Event{}
	}
	if ranking == nil {
		ranking = []rank{}
	}
	writeJSON(w, map[string]any{"series": series, "step": step, "todayUp": todayUp, "todayDown": todayDown,
		"totalUp": totalUp, "totalDown": totalDown, "ranking": ranking, "events": events})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	ev, err := s.db.Events(0, 200)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, ev)
}

// ---------------- 设置 ----------------

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	p := s.publicCfg()
	hasPwd := p.PwdHash != ""
	p.PwdHash = ""
	writeJSON(w, map[string]any{"settings": s.settings(), "public": p, "publicHasPassword": hasPwd, "pin": s.CertFingerprint(), "version": Version,
		"listen": s.listen, "tlsFixed": s.TLSMode})
}

var slugRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Settings  Settings  `json:"settings"`
		Public    PublicCfg `json:"public"`
		PublicPwd string    `json:"publicPwd"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	old := s.settings()
	st := req.Settings
	u, err := url.Parse(strings.TrimRight(st.URL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Path != "" || u.Port() != "" {
		jsonErr(w, http.StatusBadRequest, "访问地址格式应为 https://域名或IP（不含端口和路径）")
		return
	}
	st.URL = u.String()
	if st.Port < 1 || st.Port > 65535 {
		jsonErr(w, http.StatusBadRequest, "端口无效")
		return
	}
	if st.TLS != "self" && st.TLS != "acme" && st.TLS != "proxy" {
		jsonErr(w, http.StatusBadRequest, "证书方式无效")
		return
	}
	if st.Interval < 10 || st.Interval > 3600 {
		jsonErr(w, http.StatusBadRequest, "统计间隔需在 10 – 3600 秒之间")
		return
	}
	if st.Site = strings.TrimSpace(st.Site); st.Site == "" {
		st.Site = "PVE Forward"
	}
	if st.SessionDays <= 0 {
		st.SessionDays = 7
	}
	if np, err := parseNodePorts(st.DefaultPortRange, st.DefaultReservedPorts); err != nil {
		jsonErr(w, http.StatusBadRequest, "新节点默认端口："+err.Error())
		return
	} else {
		st.DefaultPortRange, st.DefaultReservedPorts = np.Range, np.Reserved
	}
	st.DefaultReserved = nil
	mirror, ok := validMirror(st.UpdateMirror)
	if !ok {
		jsonErr(w, http.StatusBadRequest, "下载加速前缀需为 https:// 开头的地址")
		return
	}
	st.UpdateMirror = mirror
	p := req.Public
	old2 := s.publicCfg()
	if p.Enabled && !slugRe.MatchString(p.Slug) {
		jsonErr(w, http.StatusBadRequest, "公开页路径后缀需为 4-64 位字母、数字、- 或 _")
		return
	}
	p.PwdHash = old2.PwdHash
	if req.PublicPwd != "" {
		p.PwdHash, _ = hashPassword(req.PublicPwd)
	}
	if p.Access == "password" && p.PwdHash == "" {
		jsonErr(w, http.StatusBadRequest, "请设置访问密码")
		return
	}
	if p.Access != "password" {
		p.Access = "open"
	}
	if p.Refresh < 5 {
		p.Refresh = 30
	}
	if p.Nodes == nil {
		p.Nodes = []int64{}
	}
	s.db.SetSettingJSON("settings", st)
	s.db.SetSettingJSON("public", p)
	restart := s.needRestart(old, st)
	if st.TLS == "self" && old.URL != st.URL {
		removeTLS(s.dataDir)
	}
	writeJSON(w, map[string]any{"ok": true, "restart": restart})
	if restart {
		s.scheduleRestart()
	}
}
