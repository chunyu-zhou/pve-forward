package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"pveforward/internal/dnsapi"
	"pveforward/internal/nodeops"
	"pveforward/internal/proto"
	"pveforward/internal/sshx"
	"pveforward/internal/store"
	"pveforward/internal/webbundle"
	"pveforward/internal/webcfg"
)

// ---------------- DNS 服务商 ----------------

type providerOut struct {
	*store.DNSProvider
	TypeName string   `json:"typeName"`
	Domains  []string `json:"domains"`
}

func (s *Server) providerParams(p *store.DNSProvider) map[string]string {
	m := map[string]string{}
	if raw, err := s.box.Open(p.SecretEnc); err == nil {
		json.Unmarshal([]byte(raw), &m)
	}
	return m
}

// dnsFor 返回主域名对应的 DNS 服务商客户端。
func (s *Server) dnsFor(domainID int64) (dnsapi.Provider, *store.WebDomain, error) {
	d, err := s.db.WebDomain(domainID)
	if err != nil {
		return nil, nil, bad("主域名不存在")
	}
	p, err := s.db.DNSProvider(d.ProviderID)
	if err != nil {
		return nil, d, bad("主域名 %s 的 DNS 服务商不存在", d.Name)
	}
	cl, err := dnsapi.New(p.Type, s.providerParams(p))
	return cl, d, err
}

func (s *Server) listDNSTypes(w http.ResponseWriter, r *http.Request) { writeJSON(w, dnsapi.Types) }

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	ps, err := s.db.DNSProviders()
	if err != nil {
		fail(w, err)
		return
	}
	domains, _ := s.db.WebDomains()
	res := []providerOut{}
	for _, p := range ps {
		t, _ := dnsapi.TypeOf(p.Type)
		o := providerOut{DNSProvider: p, TypeName: t.Name, Domains: []string{}}
		for _, d := range domains {
			if d.ProviderID == p.ID {
				o.Domains = append(o.Domains, d.Name)
			}
		}
		res = append(res, o)
	}
	writeJSON(w, res)
}

type providerIn struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Fields map[string]string `json:"fields"`
}

// mergeProvider 合并参数：编辑时留空的密钥沿用原值。
func (s *Server) mergeProvider(in *providerIn) (*store.DNSProvider, map[string]string, error) {
	p := &store.DNSProvider{}
	params := map[string]string{}
	if in.ID > 0 {
		old, err := s.db.DNSProvider(in.ID)
		if err != nil {
			return nil, nil, bad("DNS 服务商不存在")
		}
		p, params = old, s.providerParams(old)
		in.Type = old.Type
	}
	t, ok := dnsapi.TypeOf(in.Type)
	if !ok {
		return nil, nil, bad("不支持的 DNS 服务商类型")
	}
	p.Type = t.Type
	for _, f := range t.Fields {
		if v := strings.TrimSpace(in.Fields[f.Key]); v != "" || !f.Secret {
			params[f.Key] = v
		}
	}
	if _, err := dnsapi.New(p.Type, params); err != nil {
		return nil, nil, bad("%v", err)
	}
	p.Name = strings.TrimSpace(in.Name)
	if p.Name == "" {
		p.Name = t.Name
	}
	return p, params, nil
}

func (s *Server) testProvider(ctx context.Context, typ string, params map[string]string) ([]string, error) {
	cl, err := dnsapi.New(typ, params)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return cl.Zones(cctx)
}

func (s *Server) saveProvider(w http.ResponseWriter, r *http.Request) {
	var in providerIn
	if !readJSON(w, r, &in) {
		return
	}
	in.ID = pathID(r)
	p, params, err := s.mergeProvider(&in)
	if err != nil {
		fail(w, err)
		return
	}
	b, _ := json.Marshal(params)
	p.SecretEnc, p.Account = s.box.Seal(string(b)), dnsapi.Mask(p.Type, params)
	zones, terr := s.testProvider(r.Context(), p.Type, params)
	p.Status = ""
	if terr != nil {
		p.Status = terr.Error()
	}
	if err := s.db.SaveDNSProvider(p); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"id": p.ID, "status": p.Status, "zones": len(zones)})
}

// postTestProvider 测试未保存（或编辑中）的参数。
func (s *Server) postTestProvider(w http.ResponseWriter, r *http.Request) {
	var in providerIn
	if !readJSON(w, r, &in) {
		return
	}
	if id := pathID(r); id > 0 {
		p, err := s.db.DNSProvider(id)
		if err != nil {
			fail(w, err)
			return
		}
		zones, err := s.testProvider(r.Context(), p.Type, s.providerParams(p))
		p.Status = ""
		if err != nil {
			p.Status = err.Error()
		}
		s.db.SaveDNSProvider(p)
		writeJSON(w, map[string]any{"ok": err == nil, "error": p.Status, "zones": zones})
		return
	}
	p, params, err := s.mergeProvider(&in)
	if err != nil {
		fail(w, err)
		return
	}
	zones, err := s.testProvider(r.Context(), p.Type, params)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	writeJSON(w, map[string]any{"ok": err == nil, "error": msg, "zones": zones})
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM web_domains WHERE provider_id=?`, id).Scan(&n)
	if n > 0 {
		jsonErr(w, http.StatusBadRequest, fmt.Sprintf("有 %d 个主域名正在使用该 DNS 服务商，请先修改或删除这些主域名", n))
		return
	}
	s.db.Exec(`DELETE FROM dns_providers WHERE id=?`, id)
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------------- 主域名 ----------------

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

type domainOut struct {
	*store.WebDomain
	ProviderName string   `json:"providerName"`
	ProviderType string   `json:"providerType"`
	UserNames    []string `json:"userNames"`
	Sites        int      `json:"sites"`
	Certs        []string `json:"certs"`
}

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	ds, err := s.db.WebDomains()
	if err != nil {
		fail(w, err)
		return
	}
	providers := map[int64]*store.DNSProvider{}
	if ps, err := s.db.DNSProviders(); err == nil {
		for _, p := range ps {
			providers[p.ID] = p
		}
	}
	names := map[int64]string{}
	if us, err := s.db.Users(); err == nil {
		for _, u := range us {
			names[u.ID] = u.Username
		}
	}
	certs, _ := s.db.Certs()
	res := []domainOut{}
	for _, d := range ds {
		o := domainOut{WebDomain: d, UserNames: []string{}, Certs: []string{}}
		if p := providers[d.ProviderID]; p != nil {
			t, _ := dnsapi.TypeOf(p.Type)
			o.ProviderName, o.ProviderType = p.Name, t.Name
		}
		for _, id := range d.Users {
			o.UserNames = append(o.UserNames, names[id])
		}
		s.db.QueryRow(`SELECT COUNT(DISTINCT site_id) FROM site_hosts WHERE domain_id=?`, d.ID).Scan(&o.Sites)
		for _, c := range certs {
			if webcfg.Covers(c.Domains, "x."+d.Name) {
				o.Certs = append(o.Certs, c.Name)
			}
		}
		res = append(res, o)
	}
	writeJSON(w, res)
}

// providerZones 列出 DNS 服务商账号下的域名（添加主域名时选择），标出已添加的。
func (s *Server) providerZones(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.DNSProvider(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	zones, err := s.testProvider(r.Context(), p.Type, s.providerParams(p))
	if err != nil {
		fail(w, bad("无法获取 %s 的域名：%v", p.Name, err))
		return
	}
	added := map[string]bool{}
	if ds, err := s.db.WebDomains(); err == nil {
		for _, d := range ds {
			added[d.Name] = true
		}
	}
	type zone struct {
		Name  string `json:"name"`
		Added bool   `json:"added"`
	}
	res := []zone{}
	for _, z := range zones {
		z = strings.TrimSuffix(strings.ToLower(z), ".")
		res = append(res, zone{z, added[z]})
	}
	writeJSON(w, res)
}

// saveDomain 添加（可一次添加多个，names）或修改主域名。
func (s *Server) saveDomain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.WebDomain
		Names []string `json:"names"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.ID = pathID(r)
	names := in.Names
	if in.ID > 0 || len(names) == 0 {
		names = []string{in.Name}
	}
	seen := map[string]bool{}
	var clean []string
	for _, n := range names {
		n = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".")
		if n == "" || seen[n] {
			continue
		}
		if !domainRe.MatchString(n) {
			jsonErr(w, http.StatusBadRequest, "主域名 "+n+" 格式错误，如 example.com")
			return
		}
		seen[n] = true
		clean = append(clean, n)
	}
	if len(clean) == 0 {
		jsonErr(w, http.StatusBadRequest, "请选择或输入主域名")
		return
	}
	if in.Users == nil {
		in.Users = []int64{}
	}
	p, err := s.db.DNSProvider(in.ProviderID)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "请选择 DNS 服务商")
		return
	}
	// 确认 DNS 服务商账号下有这些域名，避免用户添加站点时才发现无法解析
	zones, err := s.testProvider(r.Context(), p.Type, s.providerParams(p))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "无法通过 DNS 服务商 "+p.Name+" 确认域名："+err.Error())
		return
	}
	inZone := map[string]bool{}
	for _, z := range zones {
		inZone[strings.TrimSuffix(strings.ToLower(z), ".")] = true
	}
	for _, n := range clean {
		if !inZone[n] {
			jsonErr(w, http.StatusBadRequest, fmt.Sprintf("DNS 服务商 %s 的账号下没有域名 %s", p.Name, n))
			return
		}
	}
	if in.ID == 0 {
		var ids []int64
		for _, n := range clean {
			d := store.WebDomain{Name: n, ProviderID: p.ID, Users: in.Users}
			if err := s.db.SaveWebDomain(&d); err != nil {
				fail(w, bad("%v", err))
				return
			}
			ids = append(ids, d.ID)
		}
		writeJSON(w, map[string]any{"ids": ids})
		return
	}
	in.Name = clean[0]
	if in.ID > 0 {
		old, err := s.db.WebDomain(in.ID)
		if err != nil {
			fail(w, err)
			return
		}
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM site_hosts WHERE domain_id=?`, in.ID).Scan(&n)
		if n > 0 && (old.Name != in.Name || old.ProviderID != in.ProviderID) {
			jsonErr(w, http.StatusBadRequest, fmt.Sprintf("已有 %d 个域名使用该主域名，不能修改主域名或 DNS 服务商（只能修改可用用户）", n))
			return
		}
	}
	if err := s.db.SaveWebDomain(&in.WebDomain); err != nil {
		fail(w, bad("%v", err))
		return
	}
	writeJSON(w, map[string]any{"id": in.ID})
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM site_hosts WHERE domain_id=?`, id).Scan(&n)
	if n > 0 {
		jsonErr(w, http.StatusBadRequest, fmt.Sprintf("有 %d 个站点域名正在使用该主域名，请先删除这些站点", n))
		return
	}
	s.db.Exec(`DELETE FROM web_domains WHERE id=?`, id)
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------------- 证书 ----------------

type certOut struct {
	*store.Cert
	Days  int `json:"days"`
	Sites int `json:"sites"`
}

func (s *Server) listCerts(w http.ResponseWriter, r *http.Request) {
	cs, err := s.db.Certs()
	if err != nil {
		fail(w, err)
		return
	}
	res := []certOut{}
	for _, c := range cs {
		o := certOut{Cert: c, Days: c.Days()}
		s.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE cert_id=?`, c.ID).Scan(&o.Sites)
		res = append(res, o)
	}
	writeJSON(w, res)
}

// getCert 返回证书与私钥（仅管理员）。
func (s *Server) getCert(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.Cert(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	key, err := s.box.Open(c.KeyEnc)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"cert": certOut{Cert: c, Days: c.Days()}, "pem": c.CertPEM, "key": key})
}

func (s *Server) parseCert(w http.ResponseWriter, r *http.Request) {
	var in struct{ Cert, Key string }
	if !readJSON(w, r, &in) {
		return
	}
	info, err := webcfg.ParseCert(in.Cert, in.Key)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, info)
}

func (s *Server) saveCert(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Cert, Key string }
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len([]rune(in.Name)) > 40 {
		jsonErr(w, http.StatusBadRequest, "请填写证书名称（不超过 40 个字符）")
		return
	}
	if strings.TrimSpace(in.Key) == "" {
		jsonErr(w, http.StatusBadRequest, "请填写私钥")
		return
	}
	info, err := webcfg.ParseCert(in.Cert, in.Key)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c := &store.Cert{}
	if id := pathID(r); id > 0 {
		if c, err = s.db.Cert(id); err != nil {
			fail(w, err)
			return
		}
		// 更换后的证书仍需覆盖正在使用它的站点域名
		sites, _ := s.db.Sites(0, 0)
		for _, st := range sites {
			if st.CertID == nil || *st.CertID != id {
				continue
			}
			for _, h := range st.Hosts {
				if !webcfg.Covers(info.Domains, h.Host) {
					jsonErr(w, http.StatusBadRequest, fmt.Sprintf("新证书不包含站点正在使用的域名 %s，请先修改该站点的证书", h.Host))
					return
				}
			}
		}
	}
	certPEM := strings.TrimSpace(in.Cert) + "\n"
	if c.CertPEM != certPEM {
		c.Notified = 0 // 新证书重新开始到期提醒
	}
	c.Name, c.CertPEM, c.KeyEnc = in.Name, certPEM, s.box.Seal(strings.TrimSpace(in.Key)+"\n")
	c.Domains, c.Issuer, c.NotAfter = info.Domains, info.Issuer, info.NotAfter.Unix()
	if err := s.db.SaveCert(c); err != nil {
		fail(w, err)
		return
	}
	s.kickWebNodes(`SELECT DISTINCT node_id FROM sites WHERE cert_id=?`, c.ID)
	writeJSON(w, map[string]any{"id": c.ID})
}

func (s *Server) deleteCert(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE cert_id=?`, id).Scan(&n)
	if n > 0 {
		jsonErr(w, http.StatusBadRequest, fmt.Sprintf("有 %d 个站点正在使用该证书，请先修改这些站点", n))
		return
	}
	s.db.Exec(`DELETE FROM certs WHERE id=?`, id)
	writeJSON(w, map[string]bool{"ok": true})
}

// kickWebNodes 让查询结果中的节点尽快重新生成 Web 配置。
func (s *Server) kickWebNodes(q string, args ...any) {
	rows, err := s.db.Query(q, args...)
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
	for _, id := range ids {
		s.mgr.Kick(id)
	}
}

// certNotify 每天检查证书到期：30 天内提醒一次，7 天内再提醒一次。
func (s *Server) certNotify(ctx context.Context) {
	check := func() {
		cs, err := s.db.Certs()
		if err != nil {
			return
		}
		for _, c := range cs {
			days := c.Days()
			level, lv := 0, ""
			switch {
			case days < 7:
				level, lv = 7, "error"
			case days < 30:
				level, lv = 30, "warn"
			}
			if level == 0 || (c.Notified != 0 && c.Notified <= level) {
				continue
			}
			msg := fmt.Sprintf("证书 %s 将在 %d 天后到期（%s），请及时更换", c.Name, days, time.Unix(c.NotAfter, 0).Format("2006-01-02"))
			if days < 0 {
				msg = fmt.Sprintf("证书 %s 已过期，使用它的 HTTPS 站点无法正常访问，请尽快更换", c.Name)
			}
			s.db.AddEvent(lv, 0, 0, msg)
			s.db.Exec(`UPDATE certs SET notified=? WHERE id=?`, level, c.ID)
		}
	}
	check()
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// ---------------- Web 入口 ----------------

// webPorts 返回宿主机上由 Web 转发占用的端口（这些端口不能再用于端口转发或分配给用户）。
// 入口虚拟机模式下 Web 端口在虚拟机内，不占用宿主机端口。
func (s *Server) webPorts(nodeID int64) map[int]bool {
	w, _, _ := s.db.NodeWeb(nodeID)
	res := map[int]bool{}
	if w.Mode == "vm" {
		return res
	}
	for _, p := range w.Ports() {
		res[p] = true
	}
	return res
}

type webEntryOut struct {
	Node struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		WANIP4 string `json:"wanIp4"`
		Status string `json:"status"`
		Mode   string `json:"mode"`
		Init   bool   `json:"initialized"`
	} `json:"node"`
	Web       store.NodeWeb    `json:"web"`
	HasSecret bool             `json:"hasSecret"` // 已保存 SSH 密码或私钥（不返回内容）
	State     store.WebState   `json:"state"`
	Up        *proto.WebReport `json:"report"`
	Rules     []map[string]any `json:"rules"` // 节点上的端口转发规则（用于提示端口冲突）
	VMs       []map[string]any `json:"vms"`
	Sites     int              `json:"sites"`
}

func (s *Server) webEntry(n *store.Node) *webEntryOut {
	o := &webEntryOut{Rules: []map[string]any{}, VMs: []map[string]any{}}
	o.Node.ID, o.Node.Name, o.Node.WANIP4, o.Node.Mode, o.Node.Init = n.ID, n.Name, n.Net.WANIP4, n.Mode, n.Initialized
	st := s.mgr.Status(n.ID)
	o.Node.Status = st.State
	if st.Report != nil {
		o.Up = st.Report.Web
	}
	o.Web, o.State, _ = s.db.NodeWeb(n.ID)
	o.HasSecret = len(o.Web.SSHSecretEnc) > 0
	o.Web.SSHSecretEnc, o.State.Deployed.SSHSecretEnc = nil, nil
	o.State.Hash, o.State.FailHash = "", ""
	if o.Web.HTTP == nil {
		o.Web = store.NodeWeb{Mode: "host", Deploy: "binary", SSHUser: "root", SSHPort: 22, SSHAuth: "node", HTTP: []int{80, 8080}, HTTPS: []int{443, 8443}, Default: 80}
	}
	users := map[int64]*store.User{}
	if us, err := s.db.Users(); err == nil {
		for _, u := range us {
			users[u.ID] = u
		}
	}
	if rules, err := s.db.Rules(0, n.ID); err == nil {
		for _, rl := range rules {
			owner := "公共"
			if u := users[rl.UserID]; u != nil && u.Role != "admin" {
				owner = u.Username
			}
			o.Rules = append(o.Rules, map[string]any{"id": rl.ID, "port": rl.Port, "remark": rl.Remark, "owner": owner, "public": owner == "公共",
				"vmId": rl.VMID, "targetPort": rl.TargetPort, "proto": rl.Proto})
		}
	}
	if vms, err := s.db.VMs(n.ID); err == nil {
		for _, v := range vms {
			if v.Present {
				o.VMs = append(o.VMs, map[string]any{"id": v.ID, "vmid": v.VMID, "name": v.Name, "ipv4": v.IPv4, "status": v.Status})
			}
		}
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE node_id=?`, n.ID).Scan(&o.Sites)
	return o
}

// listWebMirrors 返回部署 OpenResty 时可选的下载加速、Docker CE 软件源与镜像加速。
func (s *Server) listWebMirrors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"download":     []nodeops.Mirror{{Value: "", Label: "直连 GitHub（默认）"}, {Value: webbundle.DefaultMirror, Label: "大陆加速：ghfast.top（第三方，需信任该服务）"}},
		"dockerSource": nodeops.DockerCESources,
		"registry":     nodeops.RegistryMirrors,
		"defaultImage": nodeops.DefaultWebImage,
	})
}

func (s *Server) listWebEntries(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.db.Nodes()
	if err != nil {
		fail(w, err)
		return
	}
	res := []*webEntryOut{}
	for _, n := range nodes {
		res = append(res, s.webEntry(n))
	}
	writeJSON(w, res)
}

type webIn struct {
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode"`
	VMID           int64  `json:"vmId"`
	Deploy         string `json:"deploy"`
	Image          string `json:"image"`
	DownloadMirror string `json:"downloadMirror"`
	DockerSource   string `json:"dockerSource"`
	Registry       string `json:"registry"`
	SSHUser        string `json:"sshUser"`
	SSHPort        int    `json:"sshPort"`
	SSHAuth        string `json:"sshAuth"`
	SSHSecret      string `json:"sshSecret"` // 留空沿用已保存的
	CredID         int64  `json:"credId"`
	TakeOver       bool   `json:"takeOver"` // 部署时接管（删除）Web 端口上的端口转发规则
	// 部署时停止这些占用 Web 端口的容器（保留容器，停用 Web 转发时恢复）；只用于本次检测与部署，不保存
	Stop    []string `json:"stop"`
	HTTP    []int    `json:"http"`
	HTTPS   []int    `json:"https"`
	Default int      `json:"default"`
}

// buildWeb 校验并生成节点的 Web 入口配置。
func (s *Server) buildWeb(n *store.Node, in webIn) (store.NodeWeb, error) {
	old, _, _ := s.db.NodeWeb(n.ID)
	w := store.NodeWeb{Enabled: in.Enabled, Mode: in.Mode, VMID: in.VMID, Deploy: in.Deploy, SSHUser: strings.TrimSpace(in.SSHUser),
		SSHPort: in.SSHPort, SSHAuth: in.SSHAuth, HTTP: in.HTTP, HTTPS: in.HTTPS, Default: in.Default}
	if !n.Initialized {
		return w, bad("节点 %s 尚未完成网络初始化", n.Name)
	}
	if w.Mode != "vm" {
		w.Mode, w.VMID = "host", 0
	}
	if !w.Enabled && w.Deploy == "" {
		w.Deploy = old.Deploy
		if w.Deploy == "" {
			w.Deploy = "binary"
		}
	}
	if w.Deploy == "auto" {
		w.Deploy = "binary"
	} // 旧客户端显式兼容
	switch w.Deploy {
	case "binary", "docker", "manual":
	default:
		return w, bad("不支持的部署方式 %q", w.Deploy)
	}
	w.Image = strings.TrimSpace(in.Image)
	if w.Image == "" {
		w.Image = nodeops.DefaultWebImage
	}
	if !nodeops.ValidWebImage(w.Image) {
		return w, bad("Docker 镜像引用无效（须指定标签或 SHA256 摘要）")
	}
	w.DownloadMirror = strings.TrimSpace(in.DownloadMirror)
	if w.DownloadMirror != "" {
		u, err := url.Parse(w.DownloadMirror)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return w, bad("下载加速地址必须是无账号、查询参数的 HTTPS URL")
		}
		w.DownloadMirror = strings.TrimRight(w.DownloadMirror, "/") + "/"
	}
	w.DockerSource, w.Registry = strings.TrimSpace(in.DockerSource), strings.TrimSpace(in.Registry)
	if w.DockerSource == "" {
		w.DockerSource = nodeops.DockerSourceOfficial
	}
	if !nodeops.ValidDockerSource(w.DockerSource) {
		return w, bad("不支持的 Docker 软件源 %q", w.DockerSource)
	}
	if !nodeops.ValidRegistryMirror(w.Registry) {
		return w, bad("不支持的镜像加速 %q", w.Registry)
	}
	if w.HTTP == nil {
		w.HTTP = []int{}
	}
	if w.HTTPS == nil {
		w.HTTPS = []int{}
	}
	seen := map[int]bool{}
	for _, p := range w.Ports() {
		if p < 1 || p > 65535 {
			return w, bad("端口 %d 无效", p)
		}
		if seen[p] {
			return w, bad("端口 %d 重复", p)
		}
		seen[p] = true
	}
	if !w.Enabled {
		return w, nil
	}
	if len(w.HTTP)+len(w.HTTPS) == 0 {
		return w, bad("请至少设置一个 HTTP 或 HTTPS 端口")
	}
	if !seen[w.Default] {
		return w, bad("默认入口端口必须是已设置的 Web 端口")
	}
	if seen[webcfg.StatusPort] {
		return w, bad("端口 %d 由系统内部使用", webcfg.StatusPort)
	}
	// 宿主机运行时，端口不能已分配给用户（与端口转发规则的冲突由 webConflicts 处理）
	for _, p := range w.Ports() {
		if w.Mode == "vm" {
			break
		}
		var holder string
		if s.db.QueryRow(`SELECT u.username FROM allocated_ports a JOIN users u ON u.id=a.user_id WHERE a.node_id=? AND a.port=?`, n.ID, p).Scan(&holder) == nil {
			return w, bad("端口 %d 已分配给用户 %s，请先收回分配", p, holder)
		}
	}
	// 已有站点的每个入口都必须保留原协议。
	sites, err := s.db.Sites(0, n.ID)
	if err != nil {
		return w, err
	}
	for _, site := range sites {
		for _, p := range site.EntryPorts() {
			if !seen[p] {
				return w, bad("站点 %d 使用入口端口 %d，不能移除", site.ID, p)
			}
			if old.IsHTTPS(p) != w.IsHTTPS(p) {
				return w, bad("站点 %d 使用入口端口 %d，不能修改协议", site.ID, p)
			}
		}
	}
	if w.Mode == "vm" {
		v, err := s.db.VM(w.VMID)
		if err != nil || v.NodeID != n.ID || !v.Present {
			return w, bad("请选择该节点上的入口虚拟机")
		}
		if v.IPv4 == "" {
			return w, bad("入口虚拟机 %d 还没有内网 IPv4 地址", v.VMID)
		}
		if !sshUserRe.MatchString(w.SSHUser) {
			return w, bad("SSH 用户名无效")
		}
		if w.SSHPort == 0 {
			w.SSHPort = 22
		}
		if w.SSHPort < 1 || w.SSHPort > 65535 {
			return w, bad("SSH 端口无效")
		}
		switch w.SSHAuth {
		case "cred":
			c, err := s.db.Credential(in.CredID)
			if err != nil {
				return w, bad("请选择 SSH 凭证")
			}
			w.CredID, w.SSHUser = c.ID, c.Username
		case "password", "key":
			secret := strings.TrimSpace(in.SSHSecret)
			if w.SSHAuth == "password" {
				secret = in.SSHSecret
			}
			switch {
			case secret != "":
				if w.SSHAuth == "key" {
					if _, err := sshx.PlainKey(secret, ""); err != nil {
						return w, bad("%v", err)
					}
				}
				w.SSHSecretEnc = s.box.Seal(secret)
			case old.SSHAuth == w.SSHAuth && len(old.SSHSecretEnc) > 0:
				w.SSHSecretEnc = old.SSHSecretEnc
			default:
				return w, bad("请填写 SSH 密码或私钥")
			}
		default:
			w.SSHAuth = "node"
		}
	}
	return w, nil
}

var sshUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)

// webConflict 为与 Web 端口冲突的端口转发规则。
type webConflict struct {
	RuleID int64  `json:"ruleId"`
	Port   int    `json:"port"`
	Remark string `json:"remark"`
	// Auto 表示可自动接管：入口虚拟机模式下，该规则本就把此端口转发到入口虚拟机的同一端口，
	// 部署时删除该规则，由系统生成的转发代替，流量不中断
	Auto bool `json:"auto"`
}

// webConflicts 返回 Web 端口上已有的端口转发规则。
func (s *Server) webConflicts(n *store.Node, w store.NodeWeb) []webConflict {
	var res []webConflict
	if w.Mode == "vm" {
		return nil // 入口虚拟机模式不使用宿主机端口
	}
	rules, _ := s.db.Rules(0, n.ID)
	for _, rl := range rules {
		if !w.HasPort(rl.Port) {
			continue
		}
		auto := w.Mode == "vm" && rl.VMID == w.VMID && rl.TargetPort == rl.Port && rl.Proto != "udp"
		res = append(res, webConflict{RuleID: rl.ID, Port: rl.Port, Remark: rl.Remark, Auto: auto})
	}
	return res
}

// blockingConflicts 返回需要管理员处理的冲突说明；为空表示可以部署。
func blockingConflicts(cs []webConflict) error {
	var msg []string
	for _, c := range cs {
		if !c.Auto {
			msg = append(msg, fmt.Sprintf("宿主机端口 %d 正被端口转发规则「%s」使用", c.Port, c.Remark))
		}
	}
	if len(msg) > 0 {
		return bad("%s：请删除这些规则（由 Web 转发接管），或从 Web 端口中移除这些端口", strings.Join(msg, "；"))
	}
	return nil
}

// postWebPubkey 返回宿主机连接入口虚拟机使用的公钥（首次调用时在宿主机上生成）。
func (s *Server) postWebPubkey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := s.mgr.WebOp(ctx, pathID(r), &proto.WebReq{Op: "pubkey"})
	if err != nil {
		fail(w, bad("%v", err))
		return
	}
	writeJSON(w, map[string]string{"publicKey": res.PublicKey})
}

// postWebCheck 检测运行位置的环境（使用表单中的参数，不保存）。
func (s *Server) postWebCheck(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var in webIn
	if !readJSON(w, r, &in) {
		return
	}
	in.Enabled = true
	cfg, err := s.buildWeb(n, in)
	if err != nil {
		fail(w, err)
		return
	}
	target, err := s.mgr.WebTarget(cfg)
	if err != nil {
		fail(w, bad("%v", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	ports := cfg.Ports()
	// 已部署在同一位置时，自己占用的端口不算冲突（脚本按“由本系统部署”识别）
	res, err := s.mgr.WebOp(ctx, n.ID, &proto.WebReq{Op: "check", Target: target, Ports: ports, Deploy: cfg.Deploy, DockerSource: cfg.DockerSource, Stop: in.Stop})
	if err != nil {
		fail(w, bad("%v", err))
		return
	}
	if cfg.Mode == "vm" {
		if v, err := s.db.VM(cfg.VMID); err == nil && v.Status != "running" {
			res.Checks = append([]proto.WebCheck{{Name: "虚拟机运行中", OK: false, Detail: "虚拟机当前状态：" + v.Status}}, res.Checks...)
		} else {
			res.Checks = append([]proto.WebCheck{{Name: "虚拟机运行中", OK: true}}, res.Checks...)
		}
	}
	writeJSON(w, res)
}

// installWeb 检测运行位置：已安装 OpenResty（本系统部署的、自行安装的或 Docker 容器的镜像）时直接使用，
// 不重新安装；否则按部署方式安装。管理员确认停止的容器在检测通过后停止，返回已停止的容器（部署失败时恢复）。
// 二进制包在下载之前检测架构，经校验缓存后分块传送，节点安装时再次校验。
func (s *Server) installWeb(ctx context.Context, id int64, cfg store.NodeWeb, target proto.WebTarget, stop []string) (res *proto.WebResult, freed []string, err error) {
	log := func(text string) {
		if emit := nodeops.OutputLog(ctx); emit != nil {
			emit(text + "\n")
		}
	}
	log("正在检测安装环境…")
	check, err := s.mgr.WebOp(ctx, id, &proto.WebReq{Op: "check", Target: target, Deploy: cfg.Deploy, DockerSource: cfg.DockerSource, Ports: cfg.Ports(), Stop: stop})
	if err != nil {
		return nil, nil, err
	}
	for _, c := range check.Checks {
		if !c.OK {
			return nil, nil, fmt.Errorf("%s: %s", c.Name, c.Detail)
		}
	}
	if len(stop) > 0 {
		if _, err := s.mgr.WebOp(ctx, id, &proto.WebReq{Op: "free", Target: target, Stop: stop}); err != nil {
			return nil, nil, err
		}
		freed = stop
		defer func() {
			if err != nil {
				s.restoreFreed(id, target, freed)
				freed = nil
			}
		}()
	}
	if ex := check.Existing; ex != nil {
		if ex.Kind != "container" {
			log("已安装 " + ex.Version + "，直接使用，跳过安装")
			return &proto.WebResult{Version: ex.Version}, freed, nil
		}
		log(fmt.Sprintf("使用 Docker 容器 %s 的镜像 %s（%s），不重新下载", ex.Container, ex.Image, ex.Version))
		res, err = s.mgr.WebOp(ctx, id, &proto.WebReq{Op: "install", Target: target, Deploy: "adopt", Image: ex.ImageID})
		return res, freed, err
	}
	if cfg.Deploy == "manual" {
		return nil, freed, bad("未检测到已安装的 OpenResty")
	}
	req := &proto.WebReq{Op: "install", Target: target, Deploy: cfg.Deploy, Image: nodeops.PullImage(cfg.Image, cfg.Registry), DockerSource: cfg.DockerSource}
	if cfg.Deploy == "binary" {
		if req.SHA256, err = s.uploadBundle(ctx, id, target, check.Arch, cfg.DownloadMirror); err != nil {
			return nil, freed, err
		}
	}
	res, err = s.mgr.WebOp(ctx, id, req)
	return res, freed, err
}

// restoreFreed 部署失败时恢复本次为空出端口而停止的容器。
func (s *Server) restoreFreed(id int64, target proto.WebTarget, names []string) {
	if len(names) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := s.mgr.WebOp(ctx, id, &proto.WebReq{Op: "restore", Target: target, Stop: names}); err != nil {
		log.Printf("恢复容器 %s 失败: %v", strings.Join(names, "、"), err)
	}
}

// uploadBundle 下载（缓存）并校验 OpenResty 二进制包后分块上传到运行位置，返回包的 SHA256。
func (s *Server) uploadBundle(ctx context.Context, id int64, target proto.WebTarget, machineArch, mirror string) (string, error) {
	log := func(text string) {
		if emit := nodeops.OutputLog(ctx); emit != nil {
			emit(text + "\n")
		}
	}
	arch, err := webbundle.Arch(machineArch)
	if err != nil {
		return "", err
	}
	log("正在获取并校验 OpenResty 安装包…")
	path, err := webbundle.Fetch(ctx, filepath.Join(s.dataDir, "openresty-cache"), arch, mirror)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256<<10)
	var offset int64
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			if _, err := s.mgr.WebOp(ctx, id, &proto.WebReq{Op: "upload", Target: target, Offset: offset, Data: buf[:n]}); err != nil {
				return "", err
			}
			offset += int64(n)
			log(fmt.Sprintf("已上传 %d KiB", offset/1024))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if offset == 0 {
		return "", fmt.Errorf("OpenResty 包为空")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// postWeb 保存 Web 入口配置并部署，以 NDJSON 流式返回每一步。
func (s *Server) postWeb(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var in webIn
	if !readJSON(w, r, &in) {
		return
	}
	cfg, err := s.buildWeb(n, in)
	if err == nil && cfg.Enabled {
		if !in.TakeOver {
			err = blockingConflicts(s.webConflicts(n, cfg))
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	_, st, _ := s.db.NodeWeb(n.ID)
	oldLoc, oldDeployed := st.Location, st.Deployed
	newLoc := ""
	if cfg.Enabled {
		newLoc = cfg.Location()
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	var sendMu sync.Mutex
	streamOpen := true
	defer func() { sendMu.Lock(); streamOpen = false; sendMu.Unlock() }()
	send := func(v any) {
		sendMu.Lock()
		defer sendMu.Unlock()
		if !streamOpen {
			return
		}
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		defer controller.SetWriteDeadline(time.Time{})
		b, _ := json.Marshal(v)
		if _, err := w.Write(append(b, '\n')); err != nil {
			streamOpen = false
			return
		}
		if err := controller.Flush(); err != nil {
			streamOpen = false
		}
	}
	step := func(name string, err error) bool {
		st := proto.Step{Name: name, OK: err == nil}
		if err != nil {
			st.Error = err.Error()
		}
		send(map[string]any{"step": st})
		return err == nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := s.db.SetNodeWeb(n.ID, cfg); !step("保存配置", err) {
		send(map[string]any{"error": err.Error()})
		return
	}
	if cfg.Enabled {
		target, err := s.mgr.WebTarget(cfg)
		var freed []string
		if err == nil {
			var res *proto.WebResult
			installCtx := nodeops.WithOutputLog(ctx, func(log string) { send(map[string]string{"log": log}) })
			res, freed, err = s.installWeb(installCtx, n.ID, cfg, target, in.Stop)
			if err == nil {
				st.Version = res.Version
			}
			if !step("准备 OpenResty", err) {
				send(map[string]any{"error": err.Error()})
				return
			}
			if len(freed) > 0 {
				step("停止容器 "+strings.Join(freed, "、")+"（停用 Web 转发时恢复）", nil)
			}
		} else {
			step("连接运行位置", err)
			send(map[string]any{"error": err.Error()})
			return
		}
		// 换位置时先部署到新位置，成功后再停止原位置，减少中断时间
		st.Location, st.Deployed, st.Hash, st.FailHash, st.Error = newLoc, cfg, "", "", ""
		s.db.SetWebState(n.ID, st)
		if err := s.mgr.ApplyWeb(ctx, n.ID); !step("下发 Web 配置", err) {
			// 本次为空出端口而停止的容器：部署没有成功，恢复它们，避免两边都不提供服务
			if len(freed) > 0 {
				s.restoreFreed(n.ID, target, freed)
				step("已恢复容器 "+strings.Join(freed, "、"), nil)
			}
			send(map[string]any{"error": err.Error()})
			return
		}
	}
	// 接管目标相同的端口转发规则（入口虚拟机模式），由系统生成的转发代替
	if cfg.Enabled {
		var taken []string
		for _, c := range s.webConflicts(n, cfg) {
			if c.Auto || in.TakeOver {
				s.db.Exec(`DELETE FROM rules WHERE id=?`, c.RuleID)
				taken = append(taken, fmt.Sprintf("%d「%s」", c.Port, c.Remark))
			}
		}
		if len(taken) > 0 {
			step("接管端口转发规则 "+strings.Join(taken, "、"), nil)
		}
	}
	// 入口虚拟机模式需要宿主机转发 Web 端口；位置变化或停用时也要更新
	s.db.BumpState(n.ID)
	step("更新端口转发规则", s.mgr.RunOnce(ctx, n.ID))
	if oldLoc != "" && oldLoc != newLoc {
		err := s.removeWeb(ctx, n.ID, oldDeployed)
		step("停止原位置的 Web 转发", err)
	}
	if !cfg.Enabled {
		st = store.WebState{}
		s.db.SetWebState(n.ID, st)
		s.db.AddEvent("info", 0, n.ID, "节点 "+n.Name+" 已停用 Web 转发")
	} else {
		s.db.AddEvent("info", 0, n.ID, fmt.Sprintf("节点 %s 的 Web 转发已部署到%s", n.Name, locationName(s, newLoc)))
	}
	send(map[string]any{"done": true})
}

// removeWeb 停用部署在 deployed 所指位置（宿主机或入口虚拟机，使用部署时的 SSH 参数）的 Web 配置。
func (s *Server) removeWeb(ctx context.Context, nodeID int64, deployed store.NodeWeb) error {
	target, err := s.mgr.WebTarget(deployed)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err = s.mgr.WebOp(cctx, nodeID, &proto.WebReq{Op: "remove", Target: target})
	return err
}

func locationName(s *Server, loc string) string {
	if id, ok := strings.CutPrefix(loc, "vm:"); ok {
		var vid int64
		fmt.Sscan(id, &vid)
		if v, err := s.db.VM(vid); err == nil {
			return fmt.Sprintf("虚拟机 %d %s", v.VMID, v.Name)
		}
		return "虚拟机"
	}
	return "宿主机"
}
