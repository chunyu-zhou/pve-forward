package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"time"

	"pveforward/internal/proto"
	"pveforward/internal/store"
	"pveforward/internal/webcfg"
)

// Web 转发：配置由控制端生成，内容（摘要）变化时下发到 OpenResty 所在位置。

// webRetry 为下发失败后，配置不变时的重试间隔。
const webRetry = 10 * time.Minute

// webErr 把旧版 Agent 不支持 Web 操作的错误转换为提示。
func webErr(err error) error {
	if err != nil && strings.Contains(err.Error(), `未知方法 "web"`) {
		return errors.New("节点上的 Agent 版本过旧，不支持 Web 转发：请在节点上重新执行安装命令升级 Agent")
	}
	return err
}

// WebTarget 返回 OpenResty 运行位置的连接参数（入口虚拟机需要其内网地址与 SSH 凭据）。
func (m *Manager) WebTarget(w store.NodeWeb) (proto.WebTarget, error) {
	if w.Mode != "vm" {
		return proto.WebTarget{Mode: "host"}, nil
	}
	v, err := m.db.VM(w.VMID)
	if err != nil || !v.Present {
		return proto.WebTarget{}, errors.New("入口虚拟机不存在")
	}
	if v.IPv4 == "" {
		return proto.WebTarget{}, fmt.Errorf("入口虚拟机 %d 还没有内网 IPv4 地址", v.VMID)
	}
	t := proto.WebTarget{Mode: "vm", VMIP: v.IPv4, SSHUser: w.SSHUser, SSHPort: w.SSHPort, SSHAuth: w.SSHAuth}
	if w.SSHAuth == "password" || w.SSHAuth == "key" {
		if t.SSHSecret, err = m.box.Open(w.SSHSecretEnc); err != nil {
			return t, err
		}
	}
	return t, nil
}

// webStatusURL 返回已部署的 OpenResty 统计接口地址；未部署时为空。
func (m *Manager) webStatusURL(w store.NodeWeb, st store.WebState) string {
	if !w.Enabled || st.Location == "" {
		return ""
	}
	ip := "127.0.0.1"
	if id, ok := strings.CutPrefix(st.Location, "vm:"); ok {
		var vid int64
		fmt.Sscan(id, &vid)
		v, err := m.db.VM(vid)
		if err != nil || v.IPv4 == "" {
			return ""
		}
		ip = v.IPv4
	}
	return fmt.Sprintf("http://%s:%d/pvef-stats", ip, webcfg.StatusPort)
}

// RenderWeb 生成节点的 Web 配置，并返回各站点无法生效的原因（站点 ID → 原因，正常为空）。
func (m *Manager) RenderWeb(n *store.Node, w store.NodeWeb, st store.WebState) (*webcfg.Rendered, map[int64]string, error) {
	cfg := webcfg.Config{HTTP: w.HTTP, HTTPS: w.HTTPS, Version: st.Version, StatusListen: "127.0.0.1", StatusAllow: "127.0.0.1", Certs: map[int64]webcfg.Cert{}}
	if w.Mode == "vm" {
		v, err := m.db.VM(w.VMID)
		if err != nil || v.IPv4 == "" {
			return nil, nil, errors.New("入口虚拟机没有内网 IPv4 地址")
		}
		gw, err := netip.ParsePrefix(n.Net.GW4)
		if err != nil {
			return nil, nil, errors.New("节点内网网关地址无效")
		}
		cfg.StatusListen, cfg.StatusAllow = v.IPv4, gw.Addr().String()
	}
	vms := map[int64]*store.VM{}
	if list, err := m.db.VMs(n.ID); err == nil {
		for _, v := range list {
			vms[v.ID] = v
		}
	} else {
		return nil, nil, err
	}
	users := map[int64]*store.User{}
	if list, err := m.db.Users(); err == nil {
		for _, u := range list {
			users[u.ID] = u
		}
	} else {
		return nil, nil, err
	}
	certs := map[int64]*store.Cert{}
	if list, err := m.db.Certs(); err == nil {
		for _, c := range list {
			certs[c.ID] = c
		}
	} else {
		return nil, nil, err
	}
	sites, err := m.db.Sites(0, n.ID)
	if err != nil {
		return nil, nil, err
	}
	reasons := map[int64]string{}
	for _, s := range sites {
		if !s.Enabled {
			continue
		}
		u, v := users[s.UserID], vms[s.VMID]
		https := w.IsHTTPS(s.Port)
		var cert *store.Cert
		if s.CertID != nil {
			cert = certs[*s.CertID]
		}
		var hosts []string
		for _, h := range s.Hosts {
			hosts = append(hosts, h.Host)
		}
		reason := ""
		switch {
		case u == nil || !u.Enabled:
			reason = "用户已禁用"
		case u.OverQuota():
			reason = "用户流量已超限"
		case v == nil || !v.Present:
			reason = "目标虚拟机已删除"
		case u.Role != "admin" && !v.Shared && (v.UserID == nil || *v.UserID != s.UserID):
			reason = "目标虚拟机已不属于该用户"
		case v.IPv4 == "":
			reason = "目标虚拟机没有 IPv4 地址"
		case !w.HasPort(s.Port):
			reason = fmt.Sprintf("入口端口 %d 已停用", s.Port)
		case len(hosts) == 0:
			reason = "没有域名"
		case https && cert == nil:
			reason = "证书不存在"
		}
		if reason == "" && https {
			for _, h := range hosts {
				if !webcfg.Covers(cert.Domains, h) {
					reason = "证书 " + cert.Name + " 不包含域名 " + h
					break
				}
			}
		}
		var key string
		if reason == "" && https {
			if key, err = m.box.Open(cert.KeyEnc); err != nil {
				reason = "证书私钥无法解密"
			}
		}
		reasons[s.ID] = reason
		if reason != "" {
			continue
		}
		site := webcfg.Site{ID: s.ID, Hosts: hosts, Port: s.Port, HTTPS: https, Scheme: s.Scheme, Target: v.IPv4, TargetPort: s.TargetPort, Opts: s.Opts}
		if https {
			site.CertID = cert.ID
			cfg.Certs[cert.ID] = webcfg.Cert{Cert: cert.CertPEM, Key: key}
		}
		cfg.Sites = append(cfg.Sites, site)
	}
	return webcfg.Render(cfg), reasons, nil
}

// saveSiteReasons 记录站点无法生效的原因（只更新变化的）。
func (m *Manager) saveSiteReasons(nodeID int64, reasons map[int64]string) {
	sites, err := m.db.Sites(0, nodeID)
	if err != nil {
		return
	}
	for _, s := range sites {
		if r := reasons[s.ID]; r != s.Error {
			m.db.Exec(`UPDATE sites SET error_msg=? WHERE id=?`, r, s.ID)
		}
	}
}

// syncWeb 在节点串行锁内调用：配置变化时下发。只处理已部署（由管理员“保存并部署”）且位置未变的节点。
func (m *Manager) syncWeb(ctx context.Context, drv Driver, n *store.Node, force bool) error {
	w, st, err := m.db.NodeWeb(n.ID)
	if err != nil || !w.Enabled || st.Location == "" || st.Location != w.Location() {
		return err
	}
	rd, reasons, err := m.RenderWeb(n, w, st)
	if err != nil {
		if st.Error != err.Error() {
			st.Error = err.Error()
			m.db.SetWebState(n.ID, st)
		}
		return err
	}
	m.saveSiteReasons(n.ID, reasons)
	if !force && (rd.Hash == st.Hash || (rd.Hash == st.FailHash && time.Since(time.Unix(st.FailAt, 0)) < webRetry)) {
		return nil
	}
	target, err := m.WebTarget(w)
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		var res *proto.WebResult
		res, err = drv.Web(cctx, &proto.WebReq{Op: "apply", Target: target, Dir: rd.Dir, Files: rd.Files})
		cancel()
		err = webErr(err)
		if err == nil && res != nil && res.Version != "" {
			st.Version = res.Version
		}
	}
	if err != nil {
		if st.Error != err.Error() {
			m.db.AddEvent("error", 0, n.ID, fmt.Sprintf("节点 %s 的 Web 转发配置下发失败：%v", n.Name, err))
		}
		st.FailHash, st.FailAt, st.Error = rd.Hash, time.Now().Unix(), err.Error()
		m.db.SetWebState(n.ID, st)
		return err
	}
	// 配置中的 HTTP/2 写法取决于版本：版本变化后重新生成
	st.Hash, st.FailHash, st.Error, st.AppliedAt = rd.Hash, "", "", time.Now().Unix()
	if again, _, err := m.RenderWeb(n, w, st); err == nil && again.Hash != rd.Hash {
		st.Hash = ""
	}
	return m.db.SetWebState(n.ID, st)
}

// ApplyWeb 立即生成并下发节点的 Web 配置（管理员部署后调用）。
func (m *Manager) ApplyWeb(ctx context.Context, nodeID int64) error {
	n, err := m.db.Node(nodeID)
	if err != nil {
		return err
	}
	return m.WithDriver(nodeID, func(d Driver) error { return m.syncWeb(ctx, d, n, true) })
}

// WebOp 在节点上执行一次 Web 操作（检测、安装、移除）。
func (m *Manager) WebOp(ctx context.Context, nodeID int64, req *proto.WebReq) (*proto.WebResult, error) {
	var res *proto.WebResult
	err := m.WithDriver(nodeID, func(d Driver) error {
		var err error
		res, err = d.Web(ctx, req)
		return webErr(err)
	})
	return res, err
}

// webState 补充期望状态：统计接口地址、入口虚拟机的端口转发、站点目标。
func (m *Manager) webState(n *store.Node, st *proto.DesiredState, vmByID map[int64]*store.VM) {
	w, ws, err := m.db.NodeWeb(n.ID)
	if err != nil || !w.Enabled {
		return
	}
	st.WebStatus = m.webStatusURL(w, ws)
	// 入口虚拟机模式：宿主机把 Web 端口转发到入口虚拟机（部署完成后才生效）
	if w.Mode == "vm" && ws.Location == w.Location() {
		if v := vmByID[w.VMID]; v != nil && v.Present && v.IPv4 != "" {
			used := map[int]bool{}
			for _, r := range st.Rules {
				used[r.Port] = true
			}
			for _, p := range w.Ports() {
				if !used[p] {
					st.Rules = append(st.Rules, proto.Rule{ID: -int64(p), Port: p, Proto: "tcp", IPVer: "v4", Target4: v.IPv4, TargetPort: p, Public: true})
				}
			}
		}
	}
	sites, err := m.db.Sites(0, n.ID)
	if err != nil {
		return
	}
	for _, s := range sites {
		if v := vmByID[s.VMID]; s.Enabled && s.Error == "" && v != nil && v.IPv4 != "" {
			st.Sites = append(st.Sites, proto.SiteTarget{ID: s.ID, Target: v.IPv4, Port: s.TargetPort})
		}
	}
}

// processWeb 处理采集到的 OpenResty 统计与站点健康状态。
func (m *Manager) processWeb(n *store.Node, r *runtime, st *proto.DesiredState, rep *proto.Report) {
	if rep.Web != nil && rep.Web.Up {
		r.mu.Lock()
		deltas := webDeltas(r.lastWeb, rep.Web.Stats)
		r.lastWeb = rep.Web.Stats
		r.mu.Unlock()
		if err := m.db.AddSiteTraffic(n.ID, deltas); err != nil {
			log.Printf("记录站点流量失败: %v", err)
		} else {
			m.checkQuota()
		}
	}
	// 目标不可达需连续 3 次失败才判定
	r.mu.Lock()
	newErr := map[int64]string{}
	for _, s := range st.Sites {
		msg := rep.SiteHealth[s.ID]
		if msg != "" {
			r.siteFails[s.ID]++
			if r.siteFails[s.ID] < 3 {
				msg = r.siteErr[s.ID]
			} else if !strings.HasSuffix(msg, "（连续 3 次）") {
				msg += "（连续 3 次）"
			}
		} else {
			delete(r.siteFails, s.ID)
		}
		newErr[s.ID] = msg
	}
	old := r.siteErr
	r.siteErr = newErr
	r.mu.Unlock()
	for id, msg := range newErr {
		if msg != "" && msg != old[id] {
			var uid int64
			var host string
			m.db.QueryRow(`SELECT s.user_id, COALESCE((SELECT host FROM site_hosts WHERE site_id=s.id ORDER BY pos LIMIT 1),'') FROM sites s WHERE s.id=?`, id).Scan(&uid, &host)
			m.db.AddEvent("error", uid, n.ID, fmt.Sprintf("Web 转发 %s %s", host, msg))
		}
	}
}

// webDeltas 计算两次统计之间的增量（OpenResty 重启后计数从 0 开始）。
func webDeltas(last, cur map[int64]proto.WebCounter) map[int64]proto.WebCounter {
	res := map[int64]proto.WebCounter{}
	sub := func(a, b uint64) uint64 {
		if a >= b {
			return a - b
		}
		return a
	}
	for id, c := range cur {
		l := last[id]
		res[id] = proto.WebCounter{Requests: sub(c.Requests, l.Requests), Up: sub(c.Up, l.Up), Down: sub(c.Down, l.Down)}
	}
	return res
}

// SiteError 返回站点当前的运行异常（目标不可达）。
func (m *Manager) SiteError(nodeID, siteID int64) string {
	r := m.rt(nodeID)
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.siteErr[siteID]
}
