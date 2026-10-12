package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"pveforward/internal/store"
)

var errAlreadyInitialized = errors.New("控制端已初始化")

// BootstrapFromEnv is only called by the run command, before listeners or workers
// start. Once initialized, all bootstrap inputs (including secret files) are
// ignored; rotating credentials or changing settings must use the existing UI/CLI.
// Runtime listen/TLS overrides are resolved separately by the command entrypoint.
func (s *Server) BootstrapFromEnv(getenv func(string) string) error {
	var done string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key='setup_done'`).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("读取初始化状态失败: %w", err)
	}
	if done == "1" {
		return nil
	}

	username := getenv("PVE_FORWARD_ADMIN_USER")
	password := getenv("PVE_FORWARD_ADMIN_PASSWORD")
	passwordFile := getenv("PVE_FORWARD_ADMIN_PASSWORD_FILE")
	if username == "" && password == "" && passwordFile == "" {
		return nil // Preserve the interactive Web setup when no admin is provided.
	}
	if username == "" || (password == "" && passwordFile == "") {
		return errors.New("首次环境变量初始化需同时设置 PVE_FORWARD_ADMIN_USER 和 PVE_FORWARD_ADMIN_PASSWORD（或 PVE_FORWARD_ADMIN_PASSWORD_FILE）")
	}
	if password != "" && passwordFile != "" {
		return errors.New("PVE_FORWARD_ADMIN_PASSWORD 和 PVE_FORWARD_ADMIN_PASSWORD_FILE 不能同时设置")
	}
	if passwordFile != "" {
		password, err = readBootstrapPassword(passwordFile)
		if err != nil {
			return err
		}
	}
	if err := validateAdminCredentials(username, password); err != nil {
		return fmt.Errorf("PVE_FORWARD_ADMIN_USER / PVE_FORWARD_ADMIN_PASSWORD 配置无效: %w", err)
	}

	// PVE_FORWARD_PUBLIC_URL 可选：未设置时，管理员第一次用浏览器访问控制台时记录访问地址
	// （rememberOrigin），节点 Agent 使用该地址连接。
	st := s.settings()
	publicURL := strings.TrimSpace(getenv("PVE_FORWARD_PUBLIC_URL"))
	if publicURL != "" {
		baseURL, port, err := parsePublicOrigin(publicURL)
		if err != nil {
			return err
		}
		st.URL, st.Port = baseURL, port
	}
	if site := strings.TrimSpace(getenv("PVE_FORWARD_SITE")); site != "" {
		st.Site = site
	}
	// The effective override includes explicit CLI flags, which take precedence
	// over env. Persist it on first setup only, never on subsequent launches.
	if s.TLSMode != "" {
		st.TLS = s.TLSMode
	}
	if st.TLS != "self" && st.TLS != "acme" && st.TLS != "proxy" {
		return errors.New("PVE_FORWARD_TLS / -tls 必须是 self、acme 或 proxy")
	}
	u, _ := url.Parse(st.URL) // 已由 parsePublicOrigin 校验，或为空
	if st.URL != "" && st.TLS != "proxy" && u.Scheme != "https" {
		return errors.New("PVE_FORWARD_PUBLIC_URL 使用 http 时必须设置 PVE_FORWARD_TLS=proxy（或 -tls proxy）")
	}
	if st.TLS == "acme" && (st.URL == "" || net.ParseIP(u.Hostname()) != nil) {
		return errors.New("PVE_FORWARD_TLS=acme 需要 PVE_FORWARD_PUBLIC_URL 使用域名而非 IP")
	}
	_, err = s.initializeAdmin(username, password, st)
	if errors.Is(err, errAlreadyInitialized) {
		return nil // Another startup may have completed setup while we validated.
	}
	return err
}

// rememberOrigin 在尚未设置访问地址时（Docker 首次启动未指定 PVE_FORWARD_PUBLIC_URL），
// 把管理员浏览器访问控制台的地址记录为访问地址，节点 Agent 通过它连接控制端。
// 只在已登录的管理员请求中调用；之后可在系统设置中修改。
func (s *Server) rememberOrigin(r *http.Request) {
	st := s.settings()
	if st.URL != "" || r.Host == "" {
		return
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" && st.TLS == "proxy" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	baseURL, port, err := parsePublicOrigin(scheme + "://" + host)
	if err != nil {
		return
	}
	st.URL, st.Port = baseURL, port
	if err := s.db.SetSettingJSON("settings", st); err == nil {
		log.Printf("已记录控制端访问地址 %s:%d（节点 Agent 使用此地址连接，可在系统设置中修改）", baseURL, port)
	}
}

func readBootstrapPassword(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		// Do not include the secret path or OS error (which contains that path).
		return "", errors.New("无法读取 PVE_FORWARD_ADMIN_PASSWORD_FILE；请检查 Docker secret 挂载与读取权限")
	}
	defer f.Close()
	// At most 72 password bytes plus an optional newline (LF or CRLF). Limit
	// reads even if a misconfigured mount points to a large file or device.
	b, err := io.ReadAll(io.LimitReader(f, 75))
	if err != nil {
		return "", errors.New("读取 PVE_FORWARD_ADMIN_PASSWORD_FILE 失败")
	}
	password := string(b)
	if strings.HasSuffix(password, "\n") {
		password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
	}
	if len(password) < 8 || len(password) > 72 {
		return "", errors.New("PVE_FORWARD_ADMIN_PASSWORD_FILE 中的密码需为 8-72 字节（可带一个末尾换行）")
	}
	return password, nil
}

// parsePublicOrigin splits an origin into the existing Settings representation:
// URL has no port; Port is the public port, not necessarily the container port.
func parsePublicOrigin(raw string) (string, int, error) {
	invalid := errors.New("PVE_FORWARD_PUBLIC_URL 必须是完整的 http(s) 公网 origin（如 https://panel.example.com:8443 或 https://[2001:db8::1]:8443），不能包含凭据、路径、查询或片段，端口须为 1-65535")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || strings.HasSuffix(u.Host, ":") {
		return "", 0, invalid
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		if net.ParseIP(host) == nil || !strings.HasPrefix(u.Host, "[") {
			return "", 0, invalid
		}
		host = "[" + host + "]"
	} else {
		if strings.HasPrefix(u.Host, "[") {
			return "", 0, invalid
		}
		for _, c := range host {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
				return "", 0, invalid
			}
		}
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return "", 0, invalid
		}
	}
	return u.Scheme + "://" + host, port, nil
}

func validateAdminCredentials(username, password string) error {
	if !usernameRe.MatchString(username) {
		return bad("管理员账号需为 3-20 位字母、数字或下划线")
	}
	if len(password) < 8 || len(password) > 72 {
		return bad("密码需为 8-72 字节")
	}
	return nil
}

// initializeAdmin is shared by Web setup and env bootstrap. Never use DB-level
// setting writers inside this transaction: those use another SQLite connection.
func (s *Server) initializeAdmin(username, password string, st Settings) (*store.User, error) {
	if err := validateAdminCredentials(username, password); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	settingsJSON, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var done string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key='setup_done'`).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if done == "1" {
		return nil, errAlreadyInitialized
	}
	admin := &store.User{Username: username, PasswordHash: hash, Role: "admin", Enabled: true, MaxRules: 0}
	if err := s.db.CreateUser(tx, admin); err != nil {
		return nil, err
	}
	for _, setting := range [][2]string{{"settings", string(settingsJSON)}, {"setup_done", "1"}, {"setup_token_hash", ""}} {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, setting[0], setting[1]); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Only invalidate the file after commit; on rollback the Web setup token
	// must remain usable. Its hash was already cleared atomically in SQLite.
	if err := os.Remove(filepath.Join(s.dataDir, "setup-token")); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Print("初始化已完成，旧初始化令牌已失效，但 setup-token 文件无法删除；请手动清理该文件")
	}
	return admin, nil
}
