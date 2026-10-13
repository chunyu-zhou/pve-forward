package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"pveforward/internal/dnsapi"
	"pveforward/internal/store"
	"pveforward/internal/webcfg"
)

// ---------------- Web 转发站点 ----------------

type siteOut struct {
	*store.Site
	Owner      string `json:"owner"`
	Public     bool   `json:"public"`
	NodeName   string `json:"nodeName"`
	NodeStatus string `json:"nodeStatus"`
	HTTPS      bool   `json:"https"`
	VMName     string `json:"vmName"`
	VMVMID     int    `json:"vmVmid"`
	VMShared   bool   `json:"vmShared"`
	Target4    string `json:"target4"`
	CertName   string `json:"certName"`
	CertDays   int    `json:"certDays"`
	Auth       bool   `json:"auth"`
	Req24      int64  `json:"req24"`
	Up24       int64  `json:"up24"`
	Down24     int64  `json:"down24"`
}

func (s *Server) siteRows(uid int64) ([]*siteOut, error) {
	sites, err := s.db.Sites(uid, 0)
	if err != nil {
		return nil, err
	}
	users := map[int64]*store.User{}
	if us, err := s.db.Users(); err == nil {
		for _, u := range us {
			users[u.ID] = u
		}
	}
	nodes := map[int64]*store.Node{}
	if ns, err := s.db.Nodes(); err == nil {
		for _, n := range ns {
			nodes[n.ID] = n
		}
	}
	vms := map[int64]*store.VM{}
	if vs, err := s.db.VMs(0); err == nil {
		for _, v := range vs {
			vms[v.ID] = v
		}
	}
	certs := map[int64]*store.Cert{}
	if cs, err := s.db.Certs(); err == nil {
		for _, c := range cs {
			certs[c.ID] = c
		}
	}
	t24 := map[int64][3]int64{}
	if rows, err := s.db.Query(`SELECT site_id, SUM(requests), SUM(bytes_up), SUM(bytes_down) FROM site_traffic WHERE hour>=? GROUP BY site_id`, time.Now().Add(-24*time.Hour).Unix()); err == nil {
		for rows.Next() {
			var id, q, u, d int64
			rows.Scan(&id, &q, &u, &d)
			t24[id] = [3]int64{q, u, d}
		}
		rows.Close()
	}
	webs := map[int64]*siteNode{}
	for id := range nodes {
		x := &siteNode{}
		x.w, x.st, _ = s.db.NodeWeb(id)
		st := s.mgr.Status(id)
		x.state = st.State
		if st.Report != nil && st.Report.Web != nil && !st.Report.Web.Up {
			x.up = st.Report.Web.Error
		}
		webs[id] = x
	}
	res := []*siteOut{}
	for _, st := range sites {
		o := &siteOut{Site: st, Req24: t24[st.ID][0], Up24: t24[st.ID][1], Down24: t24[st.ID][2], Auth: st.Opts.AuthUser != ""}
		o.Opts.AuthHash = ""
		if u := users[st.UserID]; u != nil {
			o.Owner, o.Public = u.Username, u.Role == "admin"
			if o.Public {
				o.Owner = "公共"
			}
		}
		if v := vms[st.VMID]; v != nil {
			o.VMName, o.VMVMID, o.VMShared, o.Target4 = v.Name, v.VMID, v.Shared, v.IPv4
		}
		if st.CertID != nil {
			if c := certs[*st.CertID]; c != nil {
				o.CertName, o.CertDays = c.Name, c.Days()
			}
		}
		nw := webs[st.NodeID]
		if n := nodes[st.NodeID]; n != nil && nw != nil {
			o.NodeName, o.NodeStatus = n.Name, nw.state
			o.HTTPS = nw.w.IsHTTPS(st.Port)
		}
		if st.Enabled {
			o.Error = s.siteError(st, o, nw)
		}
		res = append(res, o)
	}
	return res, nil
}

// siteNode 为站点所在节点的 Web 入口状态。
type siteNode struct {
	w     store.NodeWeb
	st    store.WebState
	state string
	up    string // OpenResty 运行异常
}

// siteError 返回站点当前的异常原因（按影响范围从大到小）。
func (s *Server) siteError(st *store.Site, o *siteOut, nw *siteNode) string {
	switch {
	case nw == nil || !nw.w.Enabled:
		return "节点未启用 Web 转发"
	case st.Error != "":
		return st.Error
	case nw.st.Location == "":
		return "节点的 Web 入口尚未部署"
	case nw.st.Error != "":
		return "Web 配置下发失败：" + nw.st.Error
	case o.NodeStatus == "offline":
		return "节点 " + o.NodeName + " 离线，状态未知"
	case nw.up != "":
		return nw.up
	}
	if e := s.mgr.SiteError(st.NodeID, st.ID); e != "" {
		return e
	}
	for _, h := range st.Hosts {
		if h.DNSError != "" {
			return "自动解析 " + h.Host + " 失败：" + h.DNSError
		}
	}
	if o.HTTPS && o.CertDays < 0 {
		return "证书 " + o.CertName + " 已过期"
	}
	return ""
}

func (s *Server) listSites(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uid := int64(0)
	if u.Role != "admin" {
		uid = u.ID
	}
	rows, err := s.siteRows(uid)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, rows)
}

type siteHostIn struct {
	Sub      string `json:"sub"`
	DomainID int64  `json:"domainId"`
}

type siteIn struct {
	UserID     int64        `json:"userId"`
	NodeID     int64        `json:"nodeId"`
	Port       int          `json:"port"`
	Scheme     string       `json:"scheme"`
	VMID       int64        `json:"vmId"`
	TargetPort int          `json:"targetPort"`
	CertID     *int64       `json:"certId"`
	Remark     string       `json:"remark"`
	Enabled    *bool        `json:"enabled"`
	Hosts      []siteHostIn `json:"hosts"`
	Opts       struct {
		store.SiteOpts
		AuthPassword string `json:"authPassword"`
	} `json:"opts"`
}

// subRe 为子域名前缀：一级或多级，如 dev-zcy、api.dev
var subRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
var authUserRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

func (s *Server) saveSite(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	id := pathID(r)
	var in siteIn
	if !readJSON(w, r, &in) {
		return
	}
	var old *store.Site
	if id > 0 {
		var err error
		if old, err = s.db.Site(id); err != nil || (me.Role != "admin" && old.UserID != me.ID) {
			jsonErr(w, http.StatusNotFound, "站点不存在")
			return
		}
	}
	// 只切换启用状态
	if old != nil && in.NodeID == 0 && in.Enabled != nil {
		s.db.Exec(`UPDATE sites SET enabled=?, error_msg='' WHERE id=?`, *in.Enabled, id)
		s.mgr.Kick(old.NodeID)
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if me.Role != "admin" {
		in.UserID = me.ID
	} else if old != nil && in.UserID == 0 {
		in.UserID = old.UserID
	}
	owner, err := s.db.User(in.UserID)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "请选择所属用户")
		return
	}
	if owner.Role == "admin" {
		owner = me // 公共站点统一记在当前管理员名下
	}
	site, err := s.validateSite(r.Context(), me, owner, old, &in)
	if err != nil {
		fail(w, err)
		return
	}
	removed, err := s.db.SaveSite(site)
	if err != nil {
		fail(w, bad("%v", err))
		return
	}
	// 自动解析：新增的域名添加 A 记录，移除的域名删除本系统添加的记录
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n, _ := s.db.Node(site.NodeID)
	// 备注变化时同步修改已有解析的备注
	results := s.syncSiteDNS(ctx, site, n.Net.WANIP4, old != nil && (old.Remark != site.Remark || old.UserID != site.UserID))
	for _, h := range removed {
		s.deleteHostDNS(ctx, h)
	}
	if old != nil && old.NodeID != site.NodeID {
		s.mgr.Kick(old.NodeID)
	}
	s.mgr.Kick(site.NodeID)
	writeJSON(w, map[string]any{"id": site.ID, "dns": results})
}

func (s *Server) validateSite(ctx context.Context, me, owner *store.User, old *store.Site, in *siteIn) (*store.Site, error) {
	n, err := s.db.Node(in.NodeID)
	if err != nil || !n.Initialized {
		return nil, bad("请选择已初始化的节点")
	}
	web, _, _ := s.db.NodeWeb(n.ID)
	if !web.Enabled {
		return nil, bad("节点 %s 未启用 Web 转发", n.Name)
	}
	if !web.HasPort(in.Port) {
		return nil, bad("入口端口 %d 不是节点 %s 的 Web 端口", in.Port, n.Name)
	}
	if n.Net.WANIP4 == "" {
		return nil, bad("节点 %s 没有公网 IPv4 地址，无法自动解析", n.Name)
	}
	if in.Scheme != "https" {
		in.Scheme = "http"
	}
	if in.TargetPort < 1 || in.TargetPort > 65535 {
		return nil, bad("目标端口无效")
	}
	public := owner.Role == "admin"
	v, err := s.db.VM(in.VMID)
	if err != nil || v.NodeID != n.ID || !v.Present {
		return nil, bad("请选择该节点上的虚拟机")
	}
	if !public && !v.Shared && (v.UserID == nil || *v.UserID != owner.ID) {
		return nil, bad("只能选择 %s 的虚拟机或公共虚拟机", owner.Username)
	}
	if v.IPv4 == "" {
		return nil, bad("虚拟机 %d 没有 IPv4 地址", v.VMID)
	}

	// 域名
	if len(in.Hosts) == 0 {
		return nil, bad("请填写域名")
	}
	if len(in.Hosts) > 10 {
		return nil, bad("每个站点最多 10 个域名")
	}
	mine := map[string]bool{}
	if old != nil {
		for _, h := range old.Hosts {
			mine[h.Host] = true
		}
	}
	site := &store.Site{UserID: owner.ID, NodeID: n.ID, Port: in.Port, Scheme: in.Scheme, VMID: v.ID, TargetPort: in.TargetPort, Enabled: true}
	if old != nil {
		site.ID, site.Enabled = old.ID, old.Enabled
	}
	if in.Enabled != nil {
		site.Enabled = *in.Enabled
	}
	seen := map[string]bool{}
	for _, h := range in.Hosts {
		sub := strings.ToLower(strings.TrimSpace(h.Sub))
		if !subRe.MatchString(sub) {
			return nil, bad("子域名前缀 %q 格式错误：只能包含字母、数字、短横线和点，如 dev-zcy", h.Sub)
		}
		d, err := s.db.WebDomain(h.DomainID)
		if err != nil {
			return nil, bad("请选择主域名")
		}
		if !d.Allows(owner) {
			return nil, bad("用户 %s 不能使用主域名 %s", owner.Username, d.Name)
		}
		host := sub + "." + d.Name
		if len(host) > 253 {
			return nil, bad("域名 %s 太长", host)
		}
		if seen[host] {
			return nil, bad("域名 %s 重复填写", host)
		}
		seen[host] = true
		if !mine[host] {
			if ok, msg := s.hostAvailable(ctx, sub, d, site.ID); !ok {
				return nil, bad("%s", msg)
			}
		}
		site.Hosts = append(site.Hosts, &store.SiteHost{Sub: sub, DomainID: d.ID, Host: host})
	}

	// 证书：HTTPS 入口必须选择覆盖全部域名的证书
	if web.IsHTTPS(in.Port) {
		if in.CertID == nil {
			return nil, bad("HTTPS 入口需要选择证书")
		}
		c, err := s.db.Cert(*in.CertID)
		if err != nil {
			return nil, bad("证书不存在")
		}
		for _, h := range site.Hosts {
			if !webcfg.Covers(c.Domains, h.Host) {
				return nil, bad("证书 %s 不包含域名 %s", c.Name, h.Host)
			}
		}
		site.CertID = &c.ID
	}

	// 高级参数
	o := in.Opts.SiteOpts
	if o.BodyMax < 0 || o.BodyMax > 10240 {
		return nil, bad("上传大小上限为 1 - 10240 MB")
	}
	if o.Timeout != 0 && (o.Timeout < 5 || o.Timeout > 3600) {
		return nil, bad("超时为 5 - 3600 秒")
	}
	o.Headers = strings.TrimSpace(o.Headers)
	if err := webcfg.CheckHeaders(o.Headers); err != nil {
		return nil, bad("%v", err)
	}
	o.AuthUser = strings.TrimSpace(o.AuthUser)
	o.AuthHash = ""
	if o.AuthUser != "" {
		if !authUserRe.MatchString(o.AuthUser) {
			return nil, bad("访问密码的用户名只能包含字母、数字、点、短横线和下划线")
		}
		switch {
		case in.Opts.AuthPassword != "":
			if len(in.Opts.AuthPassword) < 4 {
				return nil, bad("访问密码至少 4 位")
			}
			o.AuthHash = webcfg.SSHA(in.Opts.AuthPassword)
		case old != nil && old.Opts.AuthUser == o.AuthUser && old.Opts.AuthHash != "":
			o.AuthHash = old.Opts.AuthHash
		default:
			return nil, bad("请填写访问密码")
		}
	}
	// 自定义配置只有管理员可以修改；普通用户编辑时保留原值
	if me.Role != "admin" {
		o.Custom = ""
		if old != nil {
			o.Custom = old.Opts.Custom
		}
	}
	o.Custom = strings.TrimSpace(o.Custom)
	if len(o.Custom) > 8000 {
		return nil, bad("自定义配置太长")
	}
	if !web.IsHTTPS(in.Port) {
		o.ForceHTTPS = false
	}
	site.Opts = o
	site.Remark = strings.TrimSpace(in.Remark)
	if site.Remark == "" {
		return nil, bad("请填写备注（会写入域名解析的备注）")
	}
	if len([]rune(site.Remark)) > 40 {
		return nil, bad("备注不超过 40 个字符")
	}
	return site, nil
}

// hostAvailable 检查域名是否可用：没有被其他站点使用，DNS 中也没有解析记录。
func (s *Server) hostAvailable(ctx context.Context, sub string, d *store.WebDomain, siteID int64) (bool, string) {
	host := sub + "." + d.Name
	if other := s.db.HostOwner(host); other != 0 && other != siteID {
		return false, host + " 已被使用"
	}
	cl, _, err := s.dnsFor(d.ID)
	if err != nil {
		return false, "无法查询 " + host + " 的解析：" + err.Error()
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rs, err := cl.Records(cctx, d.Name, sub)
	if err != nil {
		return false, "无法查询 " + host + " 的解析：" + err.Error()
	}
	if len(rs) > 0 {
		return false, host + " 已被使用"
	}
	return true, host + " 可以使用"
}

// checkHost 供表单输入时检查域名是否可用。
func (s *Server) checkHost(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	var in struct {
		Sub      string `json:"sub"`
		DomainID int64  `json:"domainId"`
		SiteID   int64  `json:"siteId"`
		UserID   int64  `json:"userId"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	sub := strings.ToLower(strings.TrimSpace(in.Sub))
	if !subRe.MatchString(sub) {
		writeJSON(w, map[string]any{"ok": false, "msg": "子域名前缀只能包含字母、数字、短横线和点，如 dev-zcy"})
		return
	}
	d, err := s.db.WebDomain(in.DomainID)
	owner := me
	if me.Role == "admin" && in.UserID > 0 {
		if u, err := s.db.User(in.UserID); err == nil {
			owner = u
		}
	}
	if err != nil || !d.Allows(owner) {
		writeJSON(w, map[string]any{"ok": false, "msg": "请选择主域名"})
		return
	}
	host := sub + "." + d.Name
	// 编辑时：站点自己已有的域名可以继续使用
	if in.SiteID > 0 {
		if st, err := s.db.Site(in.SiteID); err == nil && (me.Role == "admin" || st.UserID == me.ID) {
			for _, h := range st.Hosts {
				if h.Host == host {
					writeJSON(w, map[string]any{"ok": true, "msg": host + " 可以使用"})
					return
				}
			}
		}
	}
	ok, msg := s.hostAvailable(r.Context(), sub, d, in.SiteID)
	writeJSON(w, map[string]any{"ok": ok, "msg": msg})
}

type dnsResult struct {
	Host  string `json:"host"`
	Error string `json:"error,omitempty"`
}

// dnsRemark 返回解析记录的备注：站点备注(所属用户名)。
func (s *Server) dnsRemark(site *store.Site) string {
	name := ""
	if u, err := s.db.User(site.UserID); err == nil {
		name = u.Username
	}
	return site.Remark + "(" + name + ")"
}

// syncSiteDNS 为站点的域名添加或更新 A 记录（指向节点公网 IP，备注为 站点备注(用户名)）。
// force 为 true 时即使地址未变也更新（备注变化）。
func (s *Server) syncSiteDNS(ctx context.Context, site *store.Site, ip string, force bool) []dnsResult {
	res := []dnsResult{}
	remark := s.dnsRemark(site)
	for _, h := range site.Hosts {
		if !force && h.RecordID != "" && h.RecordIP == ip && h.DNSError == "" {
			continue
		}
		recordID, err := s.upsertA(ctx, h, ip, remark)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		s.db.SetHostDNS(h.ID, recordID, ip, msg)
		h.RecordID, h.RecordIP, h.DNSError = recordID, ip, msg
		res = append(res, dnsResult{Host: h.Host, Error: msg})
	}
	return res
}

func (s *Server) upsertA(ctx context.Context, h *store.SiteHost, ip, remark string) (string, error) {
	cl, d, err := s.dnsFor(h.DomainID)
	if err != nil {
		return h.RecordID, err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if h.RecordID != "" {
		if err := cl.Update(cctx, d.Name, h.RecordID, h.Sub, "A", ip, remark); err == nil {
			return h.RecordID, nil
		}
		// 记录可能已被手动删除：重新查找或新建
	}
	rs, err := cl.Records(cctx, d.Name, h.Sub)
	if err != nil {
		return h.RecordID, err
	}
	if rec := dnsapi.Find(rs, "A"); rec != nil {
		if rec.Value == ip {
			return rec.ID, nil
		}
		if h.RecordID == "" || rec.ID != h.RecordID {
			return "", fmt.Errorf("%s 已有指向 %s 的解析记录，未自动修改", h.Host, rec.Value)
		}
	}
	if len(rs) > 0 {
		return "", fmt.Errorf("%s 已有 %s 解析记录，未自动修改", h.Host, rs[0].Type)
	}
	return cl.Create(cctx, d.Name, h.Sub, "A", ip, remark)
}

// deleteHostDNS 删除本系统为域名添加的解析记录。
func (s *Server) deleteHostDNS(ctx context.Context, h *store.SiteHost) error {
	if h.RecordID == "" {
		return nil
	}
	cl, d, err := s.dnsFor(h.DomainID)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return cl.Delete(cctx, d.Name, h.RecordID)
}

// deleteSitesDNS 删除一组站点的解析记录（删除站点、用户、节点前调用），返回失败的域名。
func (s *Server) deleteSitesDNS(sites []*store.Site) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var failed []string
	for _, st := range sites {
		for _, h := range st.Hosts {
			if err := s.deleteHostDNS(ctx, h); err != nil {
				failed = append(failed, h.Host)
				s.db.AddEvent("warn", st.UserID, st.NodeID, fmt.Sprintf("删除 %s 的解析记录失败：%v，请手动删除", h.Host, err))
			}
		}
	}
	return failed
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	st, err := s.db.Site(pathID(r))
	if err != nil || (me.Role != "admin" && st.UserID != me.ID) {
		jsonErr(w, http.StatusNotFound, "站点不存在")
		return
	}
	failed := s.deleteSitesDNS([]*store.Site{st})
	s.db.Exec(`DELETE FROM sites WHERE id=?`, st.ID)
	s.mgr.Kick(st.NodeID)
	writeJSON(w, map[string]any{"ok": true, "dnsFailed": failed})
}

func (s *Server) batchSites(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	var req struct {
		IDs    []int64 `json:"ids"`
		Action string  `json:"action"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Action != "enable" && req.Action != "disable" && req.Action != "delete" {
		jsonErr(w, http.StatusBadRequest, "未知操作")
		return
	}
	n := 0
	for _, id := range req.IDs {
		st, err := s.db.Site(id)
		if err != nil || (me.Role != "admin" && st.UserID != me.ID) {
			continue
		}
		if req.Action == "delete" {
			s.deleteSitesDNS([]*store.Site{st})
			s.db.Exec(`DELETE FROM sites WHERE id=?`, id)
		} else {
			s.db.Exec(`UPDATE sites SET enabled=?, error_msg='' WHERE id=?`, req.Action == "enable", id)
		}
		s.mgr.Kick(st.NodeID)
		n++
	}
	writeJSON(w, map[string]int{"count": n})
}

// siteOptions 返回站点表单的可选项：启用了 Web 转发的节点、主域名、证书、虚拟机。
func (s *Server) siteOptions(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	uid := queryInt(r, "user")
	if me.Role != "admin" {
		uid = me.ID
	}
	owner, err := s.db.User(uid)
	if err != nil {
		owner = me
	}
	public := owner.Role == "admin"
	res := map[string]any{"public": public}

	type nodeOpt struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Status   string `json:"status"`
		WANIP4   string `json:"wanIp4"`
		HTTP     []int  `json:"http"`
		HTTPS    []int  `json:"https"`
		Default  int    `json:"default"`
		Deployed bool   `json:"deployed"`
	}
	nodes := []nodeOpt{}
	ns, _ := s.db.Nodes()
	for _, n := range ns {
		web, st, _ := s.db.NodeWeb(n.ID)
		if !n.Initialized || !web.Enabled {
			continue
		}
		// 普通用户：只列出有自己虚拟机或公共虚拟机的节点
		if !public {
			var cnt int
			s.db.QueryRow(`SELECT COUNT(*) FROM vms WHERE node_id=? AND present=1 AND (shared=1 OR user_id=?)`, n.ID, owner.ID).Scan(&cnt)
			if cnt == 0 {
				continue
			}
		}
		nodes = append(nodes, nodeOpt{n.ID, n.Name, s.mgr.Status(n.ID).State, n.Net.WANIP4, web.HTTP, web.HTTPS, web.Default, st.Location != ""})
	}
	res["nodes"] = nodes

	type domainOpt struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	domains := []domainOpt{}
	ds, _ := s.db.WebDomains()
	for _, d := range ds {
		if d.Allows(owner) {
			domains = append(domains, domainOpt{d.ID, d.Name})
		}
	}
	res["domains"] = domains

	type certOpt struct {
		ID      int64    `json:"id"`
		Name    string   `json:"name"`
		Domains []string `json:"domains"`
		Days    int      `json:"days"`
	}
	certs := []certOpt{}
	cs, _ := s.db.Certs()
	for _, c := range cs {
		certs = append(certs, certOpt{c.ID, c.Name, c.Domains, c.Days()})
	}
	res["certs"] = certs

	if nodeID := queryInt(r, "node"); nodeID > 0 {
		type vmOpt struct {
			ID     int64  `json:"id"`
			VMID   int    `json:"vmid"`
			Name   string `json:"name"`
			Status string `json:"status"`
			IPv4   string `json:"ipv4"`
			Shared bool   `json:"shared"`
		}
		vms, _ := s.db.VMs(nodeID)
		opts := []vmOpt{}
		for _, v := range vms {
			if !v.Present || !(public || v.Shared || (v.UserID != nil && *v.UserID == owner.ID)) {
				continue
			}
			opts = append(opts, vmOpt{v.ID, v.VMID, v.Name, v.Status, v.IPv4, v.Shared})
		}
		res["vms"] = opts
	}
	writeJSON(w, res)
}

// resyncNodeDNS 节点公网 IP 变化后，更新该节点全部站点的解析记录。
func (s *Server) resyncNodeDNS(nodeID int64, ip string) {
	sites, err := s.db.Sites(0, nodeID)
	if err != nil || ip == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, st := range sites {
		for _, r := range s.syncSiteDNS(ctx, st, ip, false) {
			if r.Error != "" {
				s.db.AddEvent("warn", st.UserID, nodeID, fmt.Sprintf("更新 %s 的解析失败：%s", r.Host, r.Error))
			}
		}
	}
}
