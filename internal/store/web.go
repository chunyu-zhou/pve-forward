package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"pveforward/internal/proto"
)

// Web 转发（OpenResty）相关的表。
const webSchema = `
CREATE TABLE IF NOT EXISTS dns_providers(
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL, account TEXT NOT NULL DEFAULT '',
  secret_enc BLOB, status TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS web_domains(
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, provider_id INTEGER NOT NULL REFERENCES dns_providers(id),
  users_json TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS certs(
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, cert_pem TEXT NOT NULL, key_enc BLOB,
  domains_json TEXT NOT NULL DEFAULT '[]', issuer TEXT NOT NULL DEFAULT '', not_after INTEGER NOT NULL DEFAULT 0,
  notified INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sites(
  id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, port INTEGER NOT NULL,
  scheme TEXT NOT NULL DEFAULT 'http', vm_id INTEGER NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
  target_port INTEGER NOT NULL, cert_id INTEGER REFERENCES certs(id), remark TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1, opts_json TEXT NOT NULL DEFAULT '{}', error_msg TEXT NOT NULL DEFAULT '',
  requests INTEGER NOT NULL DEFAULT 0, bytes_up INTEGER NOT NULL DEFAULT 0, bytes_down INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS site_hosts(
  id INTEGER PRIMARY KEY, site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  sub TEXT NOT NULL, domain_id INTEGER NOT NULL REFERENCES web_domains(id), host TEXT UNIQUE NOT NULL,
  record_id TEXT NOT NULL DEFAULT '', record_ip TEXT NOT NULL DEFAULT '', dns_error TEXT NOT NULL DEFAULT '',
  pos INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS dns_remark_jobs(
 host_id INTEGER PRIMARY KEY REFERENCES site_hosts(id) ON DELETE CASCADE,
 record_id TEXT NOT NULL, remark TEXT NOT NULL, error TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS site_traffic(
  site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE, hour INTEGER NOT NULL,
  requests INTEGER NOT NULL DEFAULT 0, bytes_up INTEGER NOT NULL DEFAULT 0, bytes_down INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(site_id, hour));
CREATE INDEX IF NOT EXISTS idx_sites_user ON sites(user_id);
CREATE INDEX IF NOT EXISTS idx_site_hosts_site ON site_hosts(site_id);
`

// migrateWeb 创建 Web 转发的表，并为旧数据库的节点表增加 Web 入口配置列。
func (d *DB) migrateWeb() error {
	if _, err := d.Exec(webSchema); err != nil {
		return err
	}
	for _, col := range []string{"web_json", "web_state_json"} {
		var has int
		if err := d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('nodes') WHERE name=?`, col).Scan(&has); err != nil {
			return err
		}
		if has == 0 {
			if _, err := d.Exec(`ALTER TABLE nodes ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var has int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sites') WHERE name='ports_json'`).Scan(&has); err != nil {
		return err
	}
	if has == 0 {
		if _, err := tx.Exec(`ALTER TABLE sites ADD COLUMN ports_json TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE sites SET ports_json='[' || port || ']' WHERE ports_json=''`); err != nil {
		return err
	}
	// 解析是否已暂停（站点停用或节点停用 Web 转发时）
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('site_hosts') WHERE name='dns_paused'`).Scan(&has); err != nil {
		return err
	}
	if has == 0 {
		if _, err := tx.Exec(`ALTER TABLE site_hosts ADD COLUMN dns_paused INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------- 节点 Web 入口 ----------------

// NodeWeb 为节点的 Web 入口配置（由管理员设置）。
type NodeWeb struct {
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode"`   // host（宿主机运行）| vm（指定虚拟机运行）
	VMID           int64  `json:"vmId"`   // 入口虚拟机（vms.id）
	Deploy         string `json:"deploy"` // binary | docker | manual（使用已安装的）；已检测到 OpenResty 时直接使用，不按此安装
	Image          string `json:"image"`
	DownloadMirror string `json:"downloadMirror"` // 空为直连；加速须显式选择
	DockerSource   string `json:"dockerSource"`   // 未安装 Docker 时的 Docker CE 软件源（空为官方源）
	Registry       string `json:"registry"`       // Docker Hub 镜像加速（空为官方）
	SSHUser        string `json:"sshUser"`
	SSHPort        int    `json:"sshPort"`
	SSHAuth        string `json:"sshAuth"` // node（宿主机的 Web 转发密钥）| password | key | cred（SSH 凭证）
	SSHSecretEnc   []byte `json:"sshSecretEnc,omitempty"`
	CredID         int64  `json:"credId,omitempty"`
	HTTP           []int  `json:"http"`
	HTTPS          []int  `json:"https"`
	Default        int    `json:"default"`
}

// Ports 返回全部 Web 端口（未启用时为空）。
func (w NodeWeb) Ports() []int {
	if !w.Enabled {
		return nil
	}
	return append(append([]int(nil), w.HTTP...), w.HTTPS...)
}

func (w NodeWeb) HasPort(p int) bool {
	for _, x := range w.Ports() {
		if x == p {
			return true
		}
	}
	return false
}

func (w NodeWeb) IsHTTPS(p int) bool {
	for _, x := range w.HTTPS {
		if x == p {
			return true
		}
	}
	return false
}

// Location 返回配置所指的 OpenResty 运行位置：host 或 vm:<vms.id>。
func (w NodeWeb) Location() string {
	if w.Mode == "vm" {
		return "vm:" + itoa(w.VMID)
	}
	return "host"
}

// WebState 为 Web 入口的运行状态（由控制端维护）。
type WebState struct {
	Location  string `json:"location"` // 已部署的位置，见 NodeWeb.Location；为空表示未部署
	Version   string `json:"version"`  // OpenResty 版本
	Hash      string `json:"hash"`     // 已生效的配置摘要
	FailHash  string `json:"failHash"` // 下发失败的配置摘要（配置不变时不重复下发）
	FailAt    int64  `json:"failAt"`
	Error     string `json:"error"`
	AppliedAt int64  `json:"appliedAt"`
	// 部署时使用的配置（运行位置与 SSH 参数），用于换位置或停用时清理原位置
	Deployed NodeWeb `json:"deployed"`
}

func (d *DB) NodeWeb(id int64) (NodeWeb, WebState, error) {
	var a, b string
	err := d.QueryRow(`SELECT web_json, web_state_json FROM nodes WHERE id=?`, id).Scan(&a, &b)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	var w NodeWeb
	var st WebState
	json.Unmarshal([]byte(a), &w)
	json.Unmarshal([]byte(b), &st)
	// 兼容旧配置，不再通过 apt 自动安装 OpenResty。
	if w.Deploy == "auto" {
		w.Deploy = "binary"
	}
	if st.Deployed.Deploy == "auto" {
		st.Deployed.Deploy = "binary"
	}
	return w, st, err
}

func (d *DB) SetNodeWeb(id int64, w NodeWeb) error {
	b, _ := json.Marshal(w)
	_, err := d.Exec(`UPDATE nodes SET web_json=? WHERE id=?`, string(b), id)
	return err
}

func (d *DB) SetWebState(id int64, st WebState) error {
	b, _ := json.Marshal(st)
	_, err := d.Exec(`UPDATE nodes SET web_state_json=? WHERE id=?`, string(b), id)
	return err
}

// ---------------- DNS 服务商 ----------------

type DNSProvider struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`    // dnspod | aliyun | tencent | huawei | cloudflare
	Account   string `json:"account"` // 账号的脱敏展示，如 "LTAI5t****9Wqz"
	SecretEnc []byte `json:"-"`       // 加密保存的参数（JSON）
	Status    string `json:"status"`  // 最近一次测试结果：空表示正常
	CreatedAt int64  `json:"createdAt"`
}

const providerCols = `id,name,type,account,secret_enc,status,created_at`

func scanProvider(r interface{ Scan(...any) error }) (*DNSProvider, error) {
	p := &DNSProvider{}
	err := r.Scan(&p.ID, &p.Name, &p.Type, &p.Account, &p.SecretEnc, &p.Status, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (d *DB) DNSProvider(id int64) (*DNSProvider, error) {
	return scanProvider(d.QueryRow(`SELECT `+providerCols+` FROM dns_providers WHERE id=?`, id))
}

func (d *DB) DNSProviders() ([]*DNSProvider, error) {
	rows, err := d.Query(`SELECT ` + providerCols + ` FROM dns_providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*DNSProvider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, p)
	}
	return res, rows.Err()
}

func (d *DB) SaveDNSProvider(p *DNSProvider) error {
	if p.ID == 0 {
		p.CreatedAt = now()
		r, err := d.Exec(`INSERT INTO dns_providers(name,type,account,secret_enc,status,created_at) VALUES(?,?,?,?,?,?)`,
			p.Name, p.Type, p.Account, p.SecretEnc, p.Status, p.CreatedAt)
		if err != nil {
			return err
		}
		p.ID, _ = r.LastInsertId()
		return nil
	}
	_, err := d.Exec(`UPDATE dns_providers SET name=?,account=?,secret_enc=?,status=? WHERE id=?`, p.Name, p.Account, p.SecretEnc, p.Status, p.ID)
	return err
}

// ---------------- 主域名 ----------------

type WebDomain struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	ProviderID int64   `json:"providerId"`
	Users      []int64 `json:"users"` // 可使用的用户；为空表示全部用户
	CreatedAt  int64   `json:"createdAt"`
}

// Allows 判断用户是否可以使用该主域名（管理员总是可以）。
func (w *WebDomain) Allows(u *User) bool {
	if u.Role == "admin" || len(w.Users) == 0 {
		return true
	}
	for _, id := range w.Users {
		if id == u.ID {
			return true
		}
	}
	return false
}

func scanDomain(r interface{ Scan(...any) error }) (*WebDomain, error) {
	w := &WebDomain{}
	var users string
	err := r.Scan(&w.ID, &w.Name, &w.ProviderID, &users, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	json.Unmarshal([]byte(users), &w.Users)
	if w.Users == nil {
		w.Users = []int64{}
	}
	return w, err
}

func (d *DB) WebDomain(id int64) (*WebDomain, error) {
	return scanDomain(d.QueryRow(`SELECT id,name,provider_id,users_json,created_at FROM web_domains WHERE id=?`, id))
}

func (d *DB) WebDomains() ([]*WebDomain, error) {
	rows, err := d.Query(`SELECT id,name,provider_id,users_json,created_at FROM web_domains ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*WebDomain
	for rows.Next() {
		w, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, w)
	}
	return res, rows.Err()
}

func (d *DB) SaveWebDomain(w *WebDomain) error {
	users := ""
	if len(w.Users) > 0 {
		b, _ := json.Marshal(w.Users)
		users = string(b)
	}
	var err error
	if w.ID == 0 {
		w.CreatedAt = now()
		var r sql.Result
		r, err = d.Exec(`INSERT INTO web_domains(name,provider_id,users_json,created_at) VALUES(?,?,?,?)`, w.Name, w.ProviderID, users, w.CreatedAt)
		if err == nil {
			w.ID, _ = r.LastInsertId()
		}
	} else {
		_, err = d.Exec(`UPDATE web_domains SET name=?,provider_id=?,users_json=? WHERE id=?`, w.Name, w.ProviderID, users, w.ID)
	}
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("主域名 " + w.Name + " 已存在")
	}
	return err
}

// ---------------- 证书 ----------------

type Cert struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	CertPEM   string   `json:"-"`
	KeyEnc    []byte   `json:"-"`
	Domains   []string `json:"domains"`
	Issuer    string   `json:"issuer"`
	NotAfter  int64    `json:"notAfter"`
	Notified  int      `json:"-"` // 已发出的到期提醒级别：30 / 7 / 0
	UpdatedAt int64    `json:"updatedAt"`
}

// Days 返回距离到期的天数（已过期为负数）。
func (c *Cert) Days() int {
	return int(time.Until(time.Unix(c.NotAfter, 0)).Hours() / 24)
}

const certCols = `id,name,cert_pem,key_enc,domains_json,issuer,not_after,notified,updated_at`

func scanCert(r interface{ Scan(...any) error }) (*Cert, error) {
	c := &Cert{}
	var domains string
	err := r.Scan(&c.ID, &c.Name, &c.CertPEM, &c.KeyEnc, &domains, &c.Issuer, &c.NotAfter, &c.Notified, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	json.Unmarshal([]byte(domains), &c.Domains)
	return c, err
}

func (d *DB) Cert(id int64) (*Cert, error) {
	return scanCert(d.QueryRow(`SELECT `+certCols+` FROM certs WHERE id=?`, id))
}

func (d *DB) Certs() ([]*Cert, error) {
	rows, err := d.Query(`SELECT ` + certCols + ` FROM certs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*Cert
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, c)
	}
	return res, rows.Err()
}

func (d *DB) SaveCert(c *Cert) error {
	c.UpdatedAt = now()
	domains, _ := json.Marshal(c.Domains)
	if c.ID == 0 {
		r, err := d.Exec(`INSERT INTO certs(name,cert_pem,key_enc,domains_json,issuer,not_after,notified,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			c.Name, c.CertPEM, c.KeyEnc, string(domains), c.Issuer, c.NotAfter, c.Notified, c.UpdatedAt)
		if err != nil {
			return err
		}
		c.ID, _ = r.LastInsertId()
		return nil
	}
	_, err := d.Exec(`UPDATE certs SET name=?,cert_pem=?,key_enc=?,domains_json=?,issuer=?,not_after=?,notified=?,updated_at=? WHERE id=?`,
		c.Name, c.CertPEM, c.KeyEnc, string(domains), c.Issuer, c.NotAfter, c.Notified, c.UpdatedAt, c.ID)
	return err
}

// ---------------- 站点 ----------------

// SiteOpts 为站点的高级参数。
type SiteOpts struct {
	ForceHTTPSPort int    `json:"forceHttpsPort"`
	WS             bool   `json:"ws"`         // 支持 WebSocket
	RealIP         bool   `json:"realIp"`     // 传递真实客户端 IP
	Block          bool   `json:"block"`      // 拦截常见攻击
	Cache          bool   `json:"cache"`      // 缓存静态资源
	ForceHTTPS     bool   `json:"forceHttps"` // HTTP 自动跳转 HTTPS
	AuthUser       string `json:"authUser"`   // 访问密码（HTTP Basic Auth）用户名；为空表示不启用
	AuthHash       string `json:"authHash"`   // 密码的 {SSHA} 摘要
	BodyMax        int    `json:"bodyMax"`    // 上传大小上限（MB）
	Timeout        int    `json:"timeout"`    // 超时（秒）
	Headers        string `json:"headers"`    // 自定义请求头，每行 "名称: 值"
	Custom         string `json:"custom"`     // 自定义配置（仅管理员），写入 server 块
}

type SiteHost struct {
	ID       int64  `json:"id"`
	SiteID   int64  `json:"-"`
	Sub      string `json:"sub"`
	DomainID int64  `json:"domainId"`
	Host     string `json:"host"`
	RecordID string `json:"-"`
	RecordIP string `json:"recordIp"`
	DNSError string `json:"dnsError"`
	// DNSPaused 解析已暂停：支持暂停的服务商停用记录；Cloudflare 删除记录（RecordID 为空），恢复时重新创建
	DNSPaused bool `json:"dnsPaused"`
}

// 解析状态说明（保存在 dns_error 中）：解析已创建但备注尚未写入或写入失败时，解析本身是正常的。
const (
	DNSRemarkPending    = "解析已创建，备注等待后台同步"
	DNSRemarkFailPrefix = "解析备注同步失败: "
)

// DNSState 返回解析状态：ok | remark_pending（解析已生效，备注待写入）| remark_failed（解析已生效，备注写入失败，自动重试）| failed（解析失败）。
func DNSState(msg string) string {
	switch {
	case msg == "":
		return "ok"
	case msg == DNSRemarkPending:
		return "remark_pending"
	case strings.HasPrefix(msg, DNSRemarkFailPrefix):
		return "remark_failed"
	}
	return "failed"
}

func (h SiteHost) MarshalJSON() ([]byte, error) {
	type plain SiteHost
	return json.Marshal(struct {
		plain
		DNSState string `json:"dnsState"`
	}{plain(h), DNSState(h.DNSError)})
}

type Site struct {
	ID         int64       `json:"id"`
	UserID     int64       `json:"userId"`
	NodeID     int64       `json:"nodeId"`
	Port       int         `json:"port"`
	Ports      []int       `json:"ports"`
	Scheme     string      `json:"scheme"`
	VMID       int64       `json:"vmId"`
	TargetPort int         `json:"targetPort"`
	CertID     *int64      `json:"certId"`
	Remark     string      `json:"remark"`
	Enabled    bool        `json:"enabled"`
	Opts       SiteOpts    `json:"opts"`
	Error      string      `json:"error"`
	Requests   int64       `json:"requests"`
	BytesUp    int64       `json:"bytesUp"`
	BytesDown  int64       `json:"bytesDown"`
	CreatedAt  int64       `json:"createdAt"`
	Hosts      []*SiteHost `json:"hosts"`
}

// NormalizeSitePorts validates and canonicalizes a site's entry ports.
func NormalizeSitePorts(ports []int) ([]int, error) {
	if len(ports) == 0 {
		return nil, errors.New("请至少选择一个入口端口")
	}
	seen := map[int]bool{}
	result := make([]int, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("入口端口 %d 无效", p)
		}
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	sort.Ints(result)
	return result, nil
}

func (s *Site) EntryPorts() []int {
	if s.Ports == nil {
		return []int{s.Port}
	}
	return s.Ports
}

const siteCols = `id,user_id,node_id,port,scheme,vm_id,target_port,cert_id,remark,enabled,opts_json,error_msg,requests,bytes_up,bytes_down,created_at,ports_json`

func scanSite(r interface{ Scan(...any) error }) (*Site, error) {
	s := &Site{}
	var opts, ports string
	err := r.Scan(&s.ID, &s.UserID, &s.NodeID, &s.Port, &s.Scheme, &s.VMID, &s.TargetPort, &s.CertID, &s.Remark, &s.Enabled,
		&opts, &s.Error, &s.Requests, &s.BytesUp, &s.BytesDown, &s.CreatedAt, &ports)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ports == "" {
		s.Ports = []int{s.Port}
	} else if err := json.Unmarshal([]byte(ports), &s.Ports); err != nil {
		return nil, err
	}
	s.Ports, err = NormalizeSitePorts(s.Ports)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(opts), &s.Opts)
	return s, nil
}

func (d *DB) Site(id int64) (*Site, error) {
	s, err := scanSite(d.QueryRow(`SELECT `+siteCols+` FROM sites WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	hosts, err := d.siteHosts(`WHERE site_id=?`, id)
	if err != nil {
		return nil, err
	}
	s.Hosts = hosts[id]
	return s, nil
}

// Sites 列出站点（含域名）；userID / nodeID 为 0 表示不限。
func (d *DB) Sites(userID, nodeID int64) ([]*Site, error) {
	q := `SELECT ` + siteCols + ` FROM sites WHERE 1=1`
	var args []any
	if userID > 0 {
		q += ` AND user_id=?`
		args = append(args, userID)
	}
	if nodeID > 0 {
		q += ` AND node_id=?`
		args = append(args, nodeID)
	}
	rows, err := d.Query(q+` ORDER BY node_id, id`, args...)
	if err != nil {
		return nil, err
	}
	var res []*Site
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		res = append(res, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hosts, err := d.siteHosts("")
	if err != nil {
		return nil, err
	}
	for _, s := range res {
		s.Hosts = hosts[s.ID]
	}
	return res, nil
}

func (d *DB) siteHosts(where string, args ...any) (map[int64][]*SiteHost, error) {
	rows, err := d.Query(`SELECT id,site_id,sub,domain_id,host,record_id,record_ip,dns_error,dns_paused FROM site_hosts `+where+` ORDER BY site_id, pos, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := map[int64][]*SiteHost{}
	for rows.Next() {
		h := &SiteHost{}
		if err := rows.Scan(&h.ID, &h.SiteID, &h.Sub, &h.DomainID, &h.Host, &h.RecordID, &h.RecordIP, &h.DNSError, &h.DNSPaused); err != nil {
			return nil, err
		}
		res[h.SiteID] = append(res[h.SiteID], h)
	}
	return res, rows.Err()
}

// HostOwner 返回使用该域名的站点 ID；没有时返回 0。
func (d *DB) HostOwner(host string) int64 {
	var id int64
	d.QueryRow(`SELECT site_id FROM site_hosts WHERE host=?`, host).Scan(&id)
	return id
}

// SaveSite 保存站点与域名（事务内）。域名按 host 匹配：保留已有记录的 DNS 解析信息，删除不再使用的。
// 返回被移除的域名（调用方负责删除其 DNS 解析）。
func (d *DB) SaveSite(s *Site) ([]*SiteHost, error) {
	ports, err := NormalizeSitePorts(s.EntryPorts())
	if err != nil {
		return nil, err
	}
	s.Ports = ports
	s.Port = ports[0]
	portsJSON, _ := json.Marshal(ports)
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	opts, _ := json.Marshal(s.Opts)
	if s.ID == 0 {
		s.CreatedAt = now()
		r, err := tx.Exec(`INSERT INTO sites(user_id,node_id,port,scheme,vm_id,target_port,cert_id,remark,enabled,opts_json,created_at,ports_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.UserID, s.NodeID, s.Port, s.Scheme, s.VMID, s.TargetPort, s.CertID, s.Remark, s.Enabled, string(opts), s.CreatedAt, string(portsJSON))
		if err != nil {
			return nil, err
		}
		s.ID, _ = r.LastInsertId()
	} else if _, err := tx.Exec(`UPDATE sites SET user_id=?,node_id=?,port=?,scheme=?,vm_id=?,target_port=?,cert_id=?,remark=?,enabled=?,opts_json=?,ports_json=?,error_msg='' WHERE id=?`,
		s.UserID, s.NodeID, s.Port, s.Scheme, s.VMID, s.TargetPort, s.CertID, s.Remark, s.Enabled, string(opts), string(portsJSON), s.ID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT id,site_id,sub,domain_id,host,record_id,record_ip,dns_error,dns_paused FROM site_hosts WHERE site_id=?`, s.ID)
	if err != nil {
		return nil, err
	}
	old := map[string]*SiteHost{}
	for rows.Next() {
		h := &SiteHost{}
		rows.Scan(&h.ID, &h.SiteID, &h.Sub, &h.DomainID, &h.Host, &h.RecordID, &h.RecordIP, &h.DNSError, &h.DNSPaused)
		old[h.Host] = h
	}
	rows.Close()
	for i, h := range s.Hosts {
		h.SiteID = s.ID
		if o := old[h.Host]; o != nil {
			h.ID, h.RecordID, h.RecordIP, h.DNSError, h.DNSPaused = o.ID, o.RecordID, o.RecordIP, o.DNSError, o.DNSPaused
			delete(old, h.Host)
			if _, err := tx.Exec(`UPDATE site_hosts SET pos=? WHERE id=?`, i, h.ID); err != nil {
				return nil, err
			}
			continue
		}
		r, err := tx.Exec(`INSERT INTO site_hosts(site_id,sub,domain_id,host,pos) VALUES(?,?,?,?,?)`, s.ID, h.Sub, h.DomainID, h.Host, i)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return nil, errors.New("域名 " + h.Host + " 已被使用")
			}
			return nil, err
		}
		h.ID, _ = r.LastInsertId()
	}
	var removed []*SiteHost
	for _, h := range old {
		removed = append(removed, h)
		if _, err := tx.Exec(`DELETE FROM site_hosts WHERE id=?`, h.ID); err != nil {
			return nil, err
		}
	}
	return removed, tx.Commit()
}

// SetHostDNS 记录域名的解析结果。
// SetHostPaused 记录解析的暂停状态与记录 ID（Cloudflare 暂停时删除记录，记录 ID 清空）。
func (d *DB) SetHostPaused(id int64, paused bool, recordID, ip string) error {
	_, err := d.Exec(`UPDATE site_hosts SET dns_paused=?, record_id=?, record_ip=? WHERE id=?`, paused, recordID, ip, id)
	return err
}

func (d *DB) SetHostDNS(id int64, recordID, ip, errMsg string) error {
	_, err := d.Exec(`UPDATE site_hosts SET record_id=?, record_ip=?, dns_error=? WHERE id=?`, recordID, ip, errMsg, id)
	return err
}

// QueueDNSRemark 同一事务保存解析ID和备注任务，重启后仍可重试。
func (d *DB) QueueDNSRemark(id int64, recordID, ip, remark string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE site_hosts SET record_id=?,record_ip=?,dns_error=? WHERE id=?`, recordID, ip, DNSRemarkPending, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO dns_remark_jobs(host_id,record_id,remark) VALUES(?,?,?) ON CONFLICT(host_id) DO UPDATE SET record_id=excluded.record_id,remark=excluded.remark,error=''`, id, recordID, remark); err != nil {
		return err
	}
	return tx.Commit()
}

// AddSiteTraffic 累加站点的请求数与流量（同时累加到用户已用流量与节点小时统计）。
func (d *DB) AddSiteTraffic(nodeID int64, deltas map[int64]proto.WebCounter) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hour := time.Now().Truncate(time.Hour).Unix()
	var tu, td uint64
	for id, c := range deltas {
		if c.Requests == 0 && c.Up == 0 && c.Down == 0 {
			continue
		}
		var uid int64
		if err := tx.QueryRow(`SELECT user_id FROM sites WHERE id=?`, id).Scan(&uid); err != nil {
			continue
		}
		tx.Exec(`UPDATE sites SET requests=requests+?, bytes_up=bytes_up+?, bytes_down=bytes_down+? WHERE id=?`, c.Requests, c.Up, c.Down, id)
		tx.Exec(`UPDATE users SET used_bytes=used_bytes+? WHERE id=?`, c.Up+c.Down, uid)
		tx.Exec(`INSERT INTO site_traffic(site_id,hour,requests,bytes_up,bytes_down) VALUES(?,?,?,?,?)
			ON CONFLICT(site_id,hour) DO UPDATE SET requests=requests+excluded.requests, bytes_up=bytes_up+excluded.bytes_up, bytes_down=bytes_down+excluded.bytes_down`,
			id, hour, c.Requests, c.Up, c.Down)
		tu += c.Up
		td += c.Down
	}
	if tu+td > 0 {
		tx.Exec(`INSERT INTO node_traffic(node_id,hour,bytes_up,bytes_down) VALUES(?,?,?,?)
			ON CONFLICT(node_id,hour) DO UPDATE SET bytes_up=bytes_up+excluded.bytes_up, bytes_down=bytes_down+excluded.bytes_down`, nodeID, hour, tu, td)
	}
	return tx.Commit()
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
