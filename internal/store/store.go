// Package store 封装控制端的 SQLite 数据库。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"pveforward/internal/ports"
	"pveforward/internal/proto"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users(
  id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'user', enabled INTEGER NOT NULL DEFAULT 1,
  quota_bytes INTEGER NOT NULL DEFAULT 0, used_bytes INTEGER NOT NULL DEFAULT 0,
  max_rules INTEGER NOT NULL DEFAULT 10, remark TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS credentials(
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL, username TEXT NOT NULL,
  secret_enc BLOB, passphrase_enc BLOB, key_fp TEXT NOT NULL DEFAULT '', key_algo TEXT NOT NULL DEFAULT '',
  remark TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS nodes(
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, remark TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL,
  initialized INTEGER NOT NULL DEFAULT 0,
  ssh_host TEXT NOT NULL DEFAULT '', ssh_port INTEGER NOT NULL DEFAULT 22, ssh_auth TEXT NOT NULL DEFAULT '',
  ssh_user TEXT NOT NULL DEFAULT 'root', ssh_secret_enc BLOB, ssh_pass_enc BLOB,
  cred_id INTEGER REFERENCES credentials(id), host_key TEXT NOT NULL DEFAULT '',
  agent_secret_hash TEXT NOT NULL DEFAULT '', agent_version TEXT NOT NULL DEFAULT '',
  hostname TEXT NOT NULL DEFAULT '', pve_version TEXT NOT NULL DEFAULT '',
  net_json TEXT NOT NULL DEFAULT '{}', ports_json TEXT NOT NULL DEFAULT '',
  state_version INTEGER NOT NULL DEFAULT 1, applied_version INTEGER NOT NULL DEFAULT 0, apply_error TEXT NOT NULL DEFAULT '',
  last_seen INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS join_tokens(token_hash TEXT PRIMARY KEY, node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS port_allocations(
  id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, kind TEXT NOT NULL, spec TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS allocated_ports(
  node_id INTEGER NOT NULL, port INTEGER NOT NULL, user_id INTEGER NOT NULL,
  allocation_id INTEGER NOT NULL REFERENCES port_allocations(id) ON DELETE CASCADE, PRIMARY KEY(node_id, port));
CREATE TABLE IF NOT EXISTS vms(
  id INTEGER PRIMARY KEY, node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, vmid INTEGER NOT NULL,
  type TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', mac TEXT NOT NULL DEFAULT '',
  user_id INTEGER REFERENCES users(id) ON DELETE SET NULL, shared INTEGER NOT NULL DEFAULT 0,
  ipv4 TEXT NOT NULL DEFAULT '', ipv6 TEXT NOT NULL DEFAULT '', present INTEGER NOT NULL DEFAULT 1,
  synced_at INTEGER NOT NULL, UNIQUE(node_id, vmid));
CREATE TABLE IF NOT EXISTS rules(
  id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, listen_port INTEGER NOT NULL,
  proto TEXT NOT NULL, ip_version TEXT NOT NULL, vm_id INTEGER NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
  target_port INTEGER NOT NULL, remark TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
  error_msg TEXT NOT NULL DEFAULT '', bytes_up INTEGER NOT NULL DEFAULT 0, bytes_down INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, UNIQUE(node_id, listen_port));
CREATE TABLE IF NOT EXISTS traffic(
  rule_id INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE, hour INTEGER NOT NULL,
  bytes_up INTEGER NOT NULL DEFAULT 0, bytes_down INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(rule_id, hour));
CREATE TABLE IF NOT EXISTS node_traffic(
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, hour INTEGER NOT NULL,
  bytes_up INTEGER NOT NULL DEFAULT 0, bytes_down INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(node_id, hour));
CREATE TABLE IF NOT EXISTS events(
  id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, level TEXT NOT NULL, user_id INTEGER, node_id INTEGER, message TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS idx_rules_user ON rules(user_id);
CREATE INDEX IF NOT EXISTS idx_vms_user ON vms(user_id);
`

type DB struct{ *sql.DB }

func Open(dir string) (*DB, error) {
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "data.db")) +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// WAL 模式允许事务进行中继续读取；事务以 IMMEDIATE 方式开始，写者按 busy_timeout 排队
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("初始化数据库失败: %w", err)
	}
	d := &DB{db}
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("升级数据库失败: %w", err)
	}
	return d, nil
}

// migrate 升级旧版本创建的数据库。
func (d *DB) migrate() error {
	// 节点端口策略（可分配范围、保留端口）：旧节点不限制范围，保留端口沿用 net_json 中的列表
	var has int
	if err := d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('nodes') WHERE name='ports_json'`).Scan(&has); err != nil {
		return err
	}
	if has == 0 {
		if _, err := d.Exec(`ALTER TABLE nodes ADD COLUMN ports_json TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	rows, err := d.Query(`SELECT id, net_json FROM nodes WHERE ports_json=''`)
	if err != nil {
		return err
	}
	type old struct {
		id  int64
		net proto.NetConfig
	}
	var olds []old
	for rows.Next() {
		var o old
		var netJSON string
		if err := rows.Scan(&o.id, &netJSON); err != nil {
			rows.Close()
			return err
		}
		json.Unmarshal([]byte(netJSON), &o.net)
		olds = append(olds, o)
	}
	rows.Close()
	for _, o := range olds {
		b, _ := json.Marshal(NodePorts{Range: "1-65535", Reserved: ports.FromList(o.net.Reserved).String()})
		if _, err := d.Exec(`UPDATE nodes SET ports_json=? WHERE id=?`, string(b), o.id); err != nil {
			return err
		}
	}
	// 取消“规则上限”：每个分配的端口可建一条规则，不再单独限制数量
	if d.Setting("migrated_max_rules", "") == "" {
		if _, err := d.Exec(`UPDATE users SET max_rules=0`); err != nil {
			return err
		}
		d.SetSetting("migrated_max_rules", "1")
	}
	if err := d.migrateWeb(); err != nil {
		return err
	}
	return d.migrate2FA()
}

var ErrNotFound = errors.New("记录不存在")

func now() int64 { return time.Now().Unix() }

// ---------------- 设置 ----------------

func (d *DB) Setting(key, def string) string {
	var v string
	if err := d.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (d *DB) SetSetting(key, value string) error {
	_, err := d.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (d *DB) SettingJSON(key string, v any) bool {
	s := d.Setting(key, "")
	return s != "" && json.Unmarshal([]byte(s), v) == nil
}

func (d *DB) SetSettingJSON(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return d.SetSetting(key, string(b))
}

// ---------------- 用户 ----------------

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	AuthVersion  int64  `json:"-"`
	Role         string `json:"role"`
	Enabled      bool   `json:"enabled"`
	QuotaBytes   int64  `json:"quotaBytes"`
	UsedBytes    int64  `json:"usedBytes"`
	MaxRules     int    `json:"maxRules"`
	Remark       string `json:"remark"`
	CreatedAt    int64  `json:"createdAt"`
	TwoFA        bool   `json:"twoFa"` // 已启用两步验证
}

// OverQuota 用户是否超出流量上限
func (u *User) OverQuota() bool { return u.QuotaBytes > 0 && u.UsedBytes >= u.QuotaBytes }

// Active 用户的规则是否应生效
func (u *User) Active() bool { return u.Enabled && !u.OverQuota() }

const userCols = `id,username,password_hash,role,enabled,quota_bytes,used_bytes,max_rules,remark,created_at,totp_enabled,auth_version`

func scanUser(r interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	err := r.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.QuotaBytes, &u.UsedBytes, &u.MaxRules, &u.Remark, &u.CreatedAt, &u.TwoFA, &u.AuthVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (d *DB) User(id int64) (*User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (d *DB) UserByName(name string) (*User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE username=?`, name))
}

func (d *DB) Users() ([]*User, error) {
	rows, err := d.Query(`SELECT ` + userCols + ` FROM users ORDER BY role='admin' DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, u)
	}
	return res, rows.Err()
}

func (d *DB) CreateUser(tx *sql.Tx, u *User) error {
	r, err := tx.Exec(`INSERT INTO users(username,password_hash,role,enabled,quota_bytes,max_rules,remark,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		u.Username, u.PasswordHash, u.Role, u.Enabled, u.QuotaBytes, u.MaxRules, u.Remark, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return errors.New("用户名已存在")
		}
		return err
	}
	u.ID, _ = r.LastInsertId()
	return nil
}

// ---------------- 会话 ----------------

func (d *DB) CreateSession(tokenHash string, uid int64, ttl time.Duration) error {
	_, err := d.Exec(`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`, tokenHash, uid, time.Now().Add(ttl).Unix())
	return err
}

var ErrAuthChanged = errors.New("认证状态已变化，请重新登录")
var ErrOTPRequired = errors.New("需要两步验证码")
var ErrOTPInvalid = errors.New("两步验证码不正确")

// CreateAuthenticatedSession 将凭据快照校验、验证码消费及新会话写入放入同一写事务。
func (d *DB) CreateAuthenticatedSession(tokenHash string, u *User, ttl time.Duration, hasCode bool, verify func([]byte, int64) (int64, bool)) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int64
	var password string
	var enabled, twoFA bool
	var enc []byte
	var last int64
	if err := tx.QueryRow(`SELECT auth_version,password_hash,enabled,totp_enabled,totp_secret_enc,totp_last FROM users WHERE id=?`, u.ID).Scan(&version, &password, &enabled, &twoFA, &enc, &last); err != nil {
		return err
	}
	if version != u.AuthVersion || password != u.PasswordHash || enabled != u.Enabled || twoFA != u.TwoFA {
		return ErrAuthChanged
	}
	if twoFA {
		if !hasCode {
			return ErrOTPRequired
		}
		step, ok := verify(enc, last)
		if !ok {
			return ErrOTPInvalid
		}
		if _, err := tx.Exec(`UPDATE users SET totp_last=? WHERE id=?`, step, u.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`, tokenHash, u.ID, time.Now().Add(ttl).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) SessionUser(tokenHash string) (*User, error) {
	return scanUser(d.QueryRow(`SELECT `+prefixCols("u.", userCols)+` FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`, tokenHash, now()))
}

func (d *DB) DeleteSession(tokenHash string) {
	d.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
}

func prefixCols(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ",")
}

// ---------------- 凭证 ----------------

type Credential struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"` // password | key
	Username      string `json:"username"`
	SecretEnc     []byte `json:"-"`
	PassphraseEnc []byte `json:"-"`
	KeyFP         string `json:"keyFp"`
	KeyAlgo       string `json:"keyAlgo"`
	Remark        string `json:"remark"`
	UpdatedAt     int64  `json:"updatedAt"`
}

const credCols = `id,name,type,username,secret_enc,passphrase_enc,key_fp,key_algo,remark,updated_at`

func scanCred(r interface{ Scan(...any) error }) (*Credential, error) {
	c := &Credential{}
	err := r.Scan(&c.ID, &c.Name, &c.Type, &c.Username, &c.SecretEnc, &c.PassphraseEnc, &c.KeyFP, &c.KeyAlgo, &c.Remark, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (d *DB) Credential(id int64) (*Credential, error) {
	return scanCred(d.QueryRow(`SELECT `+credCols+` FROM credentials WHERE id=?`, id))
}

func (d *DB) Credentials() ([]*Credential, error) {
	rows, err := d.Query(`SELECT ` + credCols + ` FROM credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*Credential
	for rows.Next() {
		c, err := scanCred(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, c)
	}
	return res, rows.Err()
}

func (d *DB) SaveCredential(c *Credential) error {
	c.UpdatedAt = now()
	if c.ID == 0 {
		r, err := d.Exec(`INSERT INTO credentials(name,type,username,secret_enc,passphrase_enc,key_fp,key_algo,remark,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			c.Name, c.Type, c.Username, c.SecretEnc, c.PassphraseEnc, c.KeyFP, c.KeyAlgo, c.Remark, c.UpdatedAt)
		if err != nil {
			return err
		}
		c.ID, _ = r.LastInsertId()
		return nil
	}
	_, err := d.Exec(`UPDATE credentials SET name=?,type=?,username=?,secret_enc=?,passphrase_enc=?,key_fp=?,key_algo=?,remark=?,updated_at=? WHERE id=?`,
		c.Name, c.Type, c.Username, c.SecretEnc, c.PassphraseEnc, c.KeyFP, c.KeyAlgo, c.Remark, c.UpdatedAt, c.ID)
	return err
}

// ---------------- 节点 ----------------

type Node struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	Remark          string          `json:"remark"`
	Mode            string          `json:"mode"` // agent | ssh
	Initialized     bool            `json:"initialized"`
	SSHHost         string          `json:"sshHost"`
	SSHPort         int             `json:"sshPort"`
	SSHAuth         string          `json:"sshAuth"` // password | key | cred
	SSHUser         string          `json:"sshUser"`
	SSHSecretEnc    []byte          `json:"-"`
	SSHPassEnc      []byte          `json:"-"`
	CredID          *int64          `json:"credId"`
	HostKey         string          `json:"-"`
	AgentSecretHash string          `json:"-"`
	AgentVersion    string          `json:"agentVersion"`
	Hostname        string          `json:"hostname"`
	PVEVersion      string          `json:"pveVersion"`
	Net             proto.NetConfig `json:"net"`
	Ports           NodePorts       `json:"ports"`
	StateVersion    int64           `json:"stateVersion"`
	AppliedVersion  int64           `json:"appliedVersion"`
	ApplyError      string          `json:"applyError"`
	LastSeen        int64           `json:"lastSeen"`
	CreatedAt       int64           `json:"createdAt"`
}

// NodePorts 为节点的端口策略：只能把可分配范围内、且不是保留端口的端口分配给用户。
// 两者均为规范化的端口集合文本（见 ports.ParseSet），如 "10000-60000" 与 "22,8006,3000-3100"。
type NodePorts struct {
	Range    string `json:"range"`
	Reserved string `json:"reserved"`
}

// Allowed 判断端口是否可以分配给用户。
func (p NodePorts) Allowed(port int) bool {
	return ports.MustSet(p.Range).Contains(port) && !ports.MustSet(p.Reserved).Contains(port)
}

const nodeCols = `id,name,remark,mode,initialized,ssh_host,ssh_port,ssh_auth,ssh_user,ssh_secret_enc,ssh_pass_enc,cred_id,host_key,agent_secret_hash,agent_version,hostname,pve_version,net_json,ports_json,state_version,applied_version,apply_error,last_seen,created_at`

func scanNode(r interface{ Scan(...any) error }) (*Node, error) {
	n := &Node{}
	var netJSON, portsJSON string
	err := r.Scan(&n.ID, &n.Name, &n.Remark, &n.Mode, &n.Initialized, &n.SSHHost, &n.SSHPort, &n.SSHAuth, &n.SSHUser, &n.SSHSecretEnc, &n.SSHPassEnc,
		&n.CredID, &n.HostKey, &n.AgentSecretHash, &n.AgentVersion, &n.Hostname, &n.PVEVersion, &netJSON, &portsJSON, &n.StateVersion, &n.AppliedVersion, &n.ApplyError, &n.LastSeen, &n.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err == nil {
		json.Unmarshal([]byte(netJSON), &n.Net)
		json.Unmarshal([]byte(portsJSON), &n.Ports)
	}
	return n, err
}

func (d *DB) Node(id int64) (*Node, error) {
	return scanNode(d.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id=?`, id))
}

func (d *DB) Nodes() ([]*Node, error) {
	rows, err := d.Query(`SELECT ` + nodeCols + ` FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, n)
	}
	return res, rows.Err()
}

func (d *DB) SaveNode(n *Node) error {
	netJSON, _ := json.Marshal(n.Net)
	portsJSON, _ := json.Marshal(n.Ports)
	if n.ID == 0 {
		r, err := d.Exec(`INSERT INTO nodes(name,remark,mode,initialized,ssh_host,ssh_port,ssh_auth,ssh_user,ssh_secret_enc,ssh_pass_enc,cred_id,host_key,agent_secret_hash,net_json,ports_json,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			n.Name, n.Remark, n.Mode, n.Initialized, n.SSHHost, n.SSHPort, n.SSHAuth, n.SSHUser, n.SSHSecretEnc, n.SSHPassEnc, n.CredID, n.HostKey, n.AgentSecretHash, string(netJSON), string(portsJSON), now())
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errors.New("节点名称已存在")
			}
			return err
		}
		n.ID, _ = r.LastInsertId()
		return nil
	}
	_, err := d.Exec(`UPDATE nodes SET name=?,remark=?,mode=?,initialized=?,ssh_host=?,ssh_port=?,ssh_auth=?,ssh_user=?,ssh_secret_enc=?,ssh_pass_enc=?,cred_id=?,host_key=?,agent_secret_hash=?,net_json=?,ports_json=? WHERE id=?`,
		n.Name, n.Remark, n.Mode, n.Initialized, n.SSHHost, n.SSHPort, n.SSHAuth, n.SSHUser, n.SSHSecretEnc, n.SSHPassEnc, n.CredID, n.HostKey, n.AgentSecretHash, string(netJSON), string(portsJSON), n.ID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("节点名称已存在")
	}
	return err
}

// BumpState 增加节点的期望状态版本，触发重新下发。
func (d *DB) BumpState(nodeID int64) {
	d.Exec(`UPDATE nodes SET state_version=state_version+1 WHERE id=?`, nodeID)
}

// ---------------- 端口分配 ----------------

type Allocation struct {
	ID     int64  `json:"id"`
	UserID int64  `json:"userId"`
	NodeID int64  `json:"nodeId"`
	Kind   string `json:"kind"`
	Spec   string `json:"spec"`
	Count  int    `json:"count"`
}

func (d *DB) Allocations(userID int64) ([]*Allocation, error) {
	q := `SELECT a.id,a.user_id,a.node_id,a.kind,a.spec,(SELECT COUNT(*) FROM allocated_ports p WHERE p.allocation_id=a.id) FROM port_allocations a`
	args := []any{}
	if userID > 0 {
		q += ` WHERE a.user_id=?`
		args = append(args, userID)
	}
	rows, err := d.Query(q+` ORDER BY a.node_id, a.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*Allocation
	for rows.Next() {
		a := &Allocation{}
		if err := rows.Scan(&a.ID, &a.UserID, &a.NodeID, &a.Kind, &a.Spec, &a.Count); err != nil {
			return nil, err
		}
		res = append(res, a)
	}
	return res, rows.Err()
}

// UserPorts 返回用户在节点上被分配的全部端口。
func (d *DB) UserPorts(userID, nodeID int64) (map[int]bool, error) {
	rows, err := d.Query(`SELECT port FROM allocated_ports WHERE user_id=? AND node_id=?`, userID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := map[int]bool{}
	for rows.Next() {
		var p int
		rows.Scan(&p)
		res[p] = true
	}
	return res, rows.Err()
}

// ---------------- 虚拟机 ----------------

type VM struct {
	ID       int64  `json:"id"`
	NodeID   int64  `json:"nodeId"`
	VMID     int    `json:"vmid"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	MAC      string `json:"mac"`
	UserID   *int64 `json:"userId"`
	Shared   bool   `json:"shared"`
	IPv4     string `json:"ipv4"`
	IPv6     string `json:"ipv6"`
	Present  bool   `json:"present"`
	SyncedAt int64  `json:"syncedAt"`
}

const vmCols = `id,node_id,vmid,type,name,status,mac,user_id,shared,ipv4,ipv6,present,synced_at`

func scanVM(r interface{ Scan(...any) error }) (*VM, error) {
	v := &VM{}
	err := r.Scan(&v.ID, &v.NodeID, &v.VMID, &v.Type, &v.Name, &v.Status, &v.MAC, &v.UserID, &v.Shared, &v.IPv4, &v.IPv6, &v.Present, &v.SyncedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (d *DB) VM(id int64) (*VM, error) {
	return scanVM(d.QueryRow(`SELECT `+vmCols+` FROM vms WHERE id=?`, id))
}

// VMs 列出虚拟机；nodeID 为 0 表示全部节点。
func (d *DB) VMs(nodeID int64) ([]*VM, error) {
	q := `SELECT ` + vmCols + ` FROM vms`
	args := []any{}
	if nodeID > 0 {
		q += ` WHERE node_id=?`
		args = append(args, nodeID)
	}
	rows, err := d.Query(q+` ORDER BY node_id, vmid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*VM
	for rows.Next() {
		v, err := scanVM(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, v)
	}
	return res, rows.Err()
}

// SyncVMs 用节点上报的虚拟机列表更新数据库，返回是否有影响期望状态的变化（MAC 变化）。
func (d *DB) SyncVMs(nodeID int64, list []proto.VMInfo) (bool, error) {
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	changed := false
	ts := now()
	seen := map[int]bool{}
	for _, v := range list {
		seen[v.VMID] = true
		var id int64
		var mac, ip4, ip6 string
		err := tx.QueryRow(`SELECT id,mac,ipv4,ipv6 FROM vms WHERE node_id=? AND vmid=?`, nodeID, v.VMID).Scan(&id, &mac, &ip4, &ip6)
		if errors.Is(err, sql.ErrNoRows) {
			r, err := tx.Exec(`INSERT INTO vms(node_id,vmid,type,name,status,mac,synced_at) VALUES(?,?,?,?,?,?,?)`,
				nodeID, v.VMID, v.Type, v.Name, v.Status, v.MAC, ts)
			if err != nil {
				return false, err
			}
			id, _ = r.LastInsertId()
		} else if err != nil {
			return false, err
		} else {
			if mac != v.MAC {
				changed = true
			}
			if _, err := tx.Exec(`UPDATE vms SET type=?,name=?,status=?,mac=?,present=1,synced_at=? WHERE id=?`, v.Type, v.Name, v.Status, v.MAC, ts, id); err != nil {
				return false, err
			}
		}
		// 虚拟机当前实际使用的地址：识别到时记录（已有虚拟机沿用原来的 IP）；未识别到（如已关机）时保留上次的记录
		if v.IP4 != "" && (v.IP4 != ip4 || (v.IP6 != "" && v.IP6 != ip6)) {
			if v.IP6 == "" {
				v.IP6 = ip6
				if ip4 != v.IP4 {
					v.IP6 = ""
				}
			}
			// 同一地址只属于一台虚拟机：清除其他虚拟机上过期的记录
			if _, err := tx.Exec(`UPDATE vms SET ipv4='', ipv6='' WHERE node_id=? AND id<>? AND ipv4=?`, nodeID, id, v.IP4); err != nil {
				return false, err
			}
			if _, err := tx.Exec(`UPDATE vms SET ipv4=?, ipv6=? WHERE id=?`, v.IP4, v.IP6, id); err != nil {
				return false, err
			}
			changed = true
		}
	}
	rows, err := tx.Query(`SELECT vmid FROM vms WHERE node_id=? AND present=1`, nodeID)
	if err != nil {
		return false, err
	}
	var gone []int
	for rows.Next() {
		var id int
		rows.Scan(&id)
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		changed = true
		// 已删除的虚拟机：未被引用则删除记录，否则标记为不存在（引用它的规则会被停用）
		var refs int
		tx.QueryRow(`SELECT (SELECT COUNT(*) FROM rules r JOIN vms v ON v.id=r.vm_id WHERE v.node_id=? AND v.vmid=?)
			+ (SELECT COUNT(*) FROM sites s JOIN vms v ON v.id=s.vm_id WHERE v.node_id=? AND v.vmid=?)`, nodeID, id, nodeID, id).Scan(&refs)
		if refs == 0 {
			tx.Exec(`DELETE FROM vms WHERE node_id=? AND vmid=? AND user_id IS NULL AND shared=0`, nodeID, id)
		}
		tx.Exec(`UPDATE vms SET present=0, status='deleted' WHERE node_id=? AND vmid=?`, nodeID, id)
	}
	return changed, tx.Commit()
}

// ---------------- 规则 ----------------

type Rule struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"userId"`
	NodeID     int64  `json:"nodeId"`
	Port       int    `json:"port"`
	Proto      string `json:"proto"`
	IPVer      string `json:"ipVer"`
	VMID       int64  `json:"vmId"`
	TargetPort int    `json:"targetPort"`
	Remark     string `json:"remark"`
	Enabled    bool   `json:"enabled"`
	Error      string `json:"error"`
	BytesUp    int64  `json:"bytesUp"`
	BytesDown  int64  `json:"bytesDown"`
	CreatedAt  int64  `json:"createdAt"`
}

const ruleCols = `id,user_id,node_id,listen_port,proto,ip_version,vm_id,target_port,remark,enabled,error_msg,bytes_up,bytes_down,created_at`

func scanRule(r interface{ Scan(...any) error }) (*Rule, error) {
	x := &Rule{}
	err := r.Scan(&x.ID, &x.UserID, &x.NodeID, &x.Port, &x.Proto, &x.IPVer, &x.VMID, &x.TargetPort, &x.Remark, &x.Enabled, &x.Error, &x.BytesUp, &x.BytesDown, &x.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return x, err
}

func (d *DB) Rule(id int64) (*Rule, error) {
	return scanRule(d.QueryRow(`SELECT `+ruleCols+` FROM rules WHERE id=?`, id))
}

// Rules 列出规则；userID / nodeID 为 0 表示不限。
func (d *DB) Rules(userID, nodeID int64) ([]*Rule, error) {
	q := `SELECT ` + ruleCols + ` FROM rules WHERE 1=1`
	var args []any
	if userID > 0 {
		q += ` AND user_id=?`
		args = append(args, userID)
	}
	if nodeID > 0 {
		q += ` AND node_id=?`
		args = append(args, nodeID)
	}
	rows, err := d.Query(q+` ORDER BY node_id, listen_port`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*Rule
	for rows.Next() {
		x, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, x)
	}
	return res, rows.Err()
}

// ---------------- 流量 ----------------

// AddTraffic 累加规则流量（同时累加到用户已用流量与节点小时统计）。
func (d *DB) AddTraffic(nodeID int64, deltas map[int64]proto.Counter) error {
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
		if c.Up == 0 && c.Down == 0 {
			continue
		}
		var uid int64
		if err := tx.QueryRow(`SELECT user_id FROM rules WHERE id=?`, id).Scan(&uid); err != nil {
			continue
		}
		tx.Exec(`UPDATE rules SET bytes_up=bytes_up+?, bytes_down=bytes_down+? WHERE id=?`, c.Up, c.Down, id)
		tx.Exec(`UPDATE users SET used_bytes=used_bytes+? WHERE id=?`, c.Up+c.Down, uid)
		tx.Exec(`INSERT INTO traffic(rule_id,hour,bytes_up,bytes_down) VALUES(?,?,?,?)
			ON CONFLICT(rule_id,hour) DO UPDATE SET bytes_up=bytes_up+excluded.bytes_up, bytes_down=bytes_down+excluded.bytes_down`, id, hour, c.Up, c.Down)
		tu += c.Up
		td += c.Down
	}
	if tu+td > 0 {
		tx.Exec(`INSERT INTO node_traffic(node_id,hour,bytes_up,bytes_down) VALUES(?,?,?,?)
			ON CONFLICT(node_id,hour) DO UPDATE SET bytes_up=bytes_up+excluded.bytes_up, bytes_down=bytes_down+excluded.bytes_down`, nodeID, hour, tu, td)
	}
	return tx.Commit()
}

// ---------------- 事件 ----------------

type Event struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"`
	Level   string `json:"level"` // info | warn | error | todo
	UserID  *int64 `json:"userId"`
	NodeID  *int64 `json:"nodeId"`
	Message string `json:"message"`
}

func (d *DB) AddEvent(level string, userID, nodeID int64, msg string) {
	var u, n any
	if userID > 0 {
		u = userID
	}
	if nodeID > 0 {
		n = nodeID
	}
	d.Exec(`INSERT INTO events(ts,level,user_id,node_id,message) VALUES(?,?,?,?,?)`, now(), level, u, n, msg)
}

func (d *DB) Events(userID int64, limit int) ([]*Event, error) {
	q := `SELECT id,ts,level,user_id,node_id,message FROM events`
	args := []any{}
	if userID > 0 {
		q += ` WHERE user_id=?`
		args = append(args, userID)
	}
	args = append(args, limit)
	rows, err := d.Query(q+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []*Event
	for rows.Next() {
		e := &Event{}
		rows.Scan(&e.ID, &e.TS, &e.Level, &e.UserID, &e.NodeID, &e.Message)
		res = append(res, e)
	}
	return res, rows.Err()
}

// Cleanup 清理过期数据
func (d *DB) Cleanup(ctx context.Context) {
	t := now()
	d.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<?`, t)
	d.ExecContext(ctx, `DELETE FROM join_tokens WHERE expires_at<?`, t)
	d.ExecContext(ctx, `DELETE FROM events WHERE ts<?`, t-30*86400)
	d.ExecContext(ctx, `DELETE FROM traffic WHERE hour<?`, t-90*86400)
	d.ExecContext(ctx, `DELETE FROM node_traffic WHERE hour<?`, t-90*86400)
	d.ExecContext(ctx, `DELETE FROM site_traffic WHERE hour<?`, t-90*86400)
}
