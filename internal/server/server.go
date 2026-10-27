// Package server 实现控制端的 HTTP API、Agent 接入与前端页面。
package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/bcrypt"

	"pveforward"
	"pveforward/internal/manager"
	"pveforward/internal/ports"
	"pveforward/internal/secret"
	"pveforward/internal/store"
)

// Version 为控制端版本（来自仓库根目录 version.json）。
var Version = pveforward.Version

type Server struct {
	db      *store.DB
	box     *secret.Box
	mgr     *manager.Manager
	dataDir string
	web     fs.FS
	listen  string // 命令行或环境变量指定的监听地址（覆盖设置）
	TLSMode string // 命令行或环境变量指定的证书方式（覆盖设置）

	restart chan struct{}
	limiter *limiter

	dnsMu  sync.Mutex
	certMu sync.Mutex
	certFP string

	upd updater
}

func New(db *store.DB, box *secret.Box, mgr *manager.Manager, dataDir string, web fs.FS, listen string) *Server {
	return &Server{db: db, box: box, mgr: mgr, dataDir: dataDir, web: web, listen: listen,
		restart: make(chan struct{}, 1), limiter: newLimiter(), upd: updater{upgrade: make(chan *UpgradeRequest, 1)}}
}

// ---------------- 设置 ----------------

type Settings struct {
	Site                 string `json:"site"`
	URL                  string `json:"url"` // 控制端访问地址（不含端口），Agent 用它连接
	Port                 int    `json:"port"`
	TLS                  string `json:"tls"` // self | acme | proxy
	Interval             int    `json:"interval"`
	OfflineAfter         int    `json:"offlineAfter"`
	DefaultPortRange     string `json:"defaultPortRange"`          // 新节点默认可分配端口范围
	DefaultReservedPorts string `json:"defaultReservedPorts"`      // 新节点默认保留端口
	DefaultReserved      []int  `json:"defaultReserved,omitempty"` // 旧版本的保留端口列表，读取时转换
	SessionDays          int    `json:"sessionDays"`
	LockAttempts         int    `json:"lockAttempts"`
	DiskBusyUtil         int    `json:"diskBusyUtil"`
	DiskSatUtil          int    `json:"diskSatUtil"`
	DiskBusyAwait        int    `json:"diskBusyAwait"`
	DiskSatAwait         int    `json:"diskSatAwait"`
	UpdateCheck          bool   `json:"updateCheck"`  // 每小时检测新版本
	UpdateMirror         string `json:"updateMirror"` // 访问 GitHub 的加速前缀（可选）
}

func (s *Server) settings() Settings {
	st := Settings{Site: "PVE Forward", Port: 5601, TLS: "proxy", Interval: 60, OfflineAfter: 180, DefaultPortRange: "10000-60000", DefaultReservedPorts: "22,8006",
		SessionDays: 7, LockAttempts: 5, DiskBusyUtil: 70, DiskSatUtil: 90, DiskBusyAwait: 20, DiskSatAwait: 40,
		UpdateCheck: true}
	s.db.SettingJSON("settings", &st)
	if st.DefaultReserved != nil && !strings.Contains(s.db.Setting("settings", ""), `"defaultReservedPorts"`) {
		st.DefaultReservedPorts = ports.FromList(st.DefaultReserved).String()
	}
	st.DefaultReserved = nil
	return st
}

func (s *Server) setupDone() bool { return s.db.Setting("setup_done", "") == "1" }

// ---------------- 运行 ----------------

func (s *Server) Run(ctx context.Context) error {
	if !s.setupDone() && s.db.Setting("setup_token_hash", "") == "" {
		tok := s.NewSetupToken()
		log.Printf("控制端尚未初始化，初始化令牌：%s", tok)
	}
	s.mgr.Interval = func() time.Duration {
		iv := s.settings().Interval
		if iv < 10 {
			iv = 10
		}
		return time.Duration(iv) * time.Second
	}
	s.mgr.OfflineAfter = func() time.Duration {
		iv := s.settings().OfflineAfter
		if iv < 30 {
			iv = 30
		}
		return time.Duration(iv) * time.Second
	}
	go s.updateLoop(ctx)
	go s.confirmUpdate(ctx)
	go s.certNotify(ctx)
	go s.dnsRemarkLoop(ctx)
	for {
		srv, extra, err := s.serve()
		if err != nil {
			return err
		}
		var upgrade *UpgradeRequest
		select {
		case <-ctx.Done():
		case <-s.restart:
			log.Printf("配置已变更，重新启动监听")
		case upgrade = <-s.upd.upgrade:
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		srv.Shutdown(sctx)
		if extra != nil {
			extra.Shutdown(sctx)
		}
		cancel()
		if upgrade != nil {
			return upgrade
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// NewSetupToken 生成新的初始化令牌（明文同时写入数据目录，便于找回）。
func (s *Server) NewSetupToken() string {
	tok := secret.ReadableToken()
	s.db.SetSetting("setup_token_hash", secret.Hash(tok))
	os.WriteFile(filepath.Join(s.dataDir, "setup-token"), []byte(tok+"\n"), 0o600)
	return tok
}

func (s *Server) serve() (*http.Server, *http.Server, error) {
	st := s.settings()
	addr := s.listen
	if addr == "" {
		addr = ":" + strconv.Itoa(st.Port)
	}
	if s.TLSMode != "" {
		st.TLS = s.TLSMode
	}
	srv := &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 15 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	var extra *http.Server
	switch st.TLS {
	case "proxy":
		log.Printf("HTTP 监听 %s（由反向代理处理 HTTPS）", addr)
		go srv.Serve(ln)
	case "acme":
		u, _ := url.Parse(st.URL)
		m := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(u.Hostname()),
			Cache: autocert.DirCache(filepath.Join(s.dataDir, "acme"))}
		srv.TLSConfig = m.TLSConfig()
		extra = &http.Server{Addr: ":80", Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 15 * time.Second}
		go func() {
			if err := extra.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("ACME HTTP-01 监听 :80 失败: %v", err)
			}
		}()
		log.Printf("HTTPS 监听 %s（Let's Encrypt：%s）", addr, u.Hostname())
		go srv.ServeTLS(ln, "", "")
	default:
		cert, err := s.selfCert(st)
		if err != nil {
			ln.Close()
			return nil, nil, err
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		log.Printf("HTTPS 监听 %s（自签名证书 SHA256 %s）", addr, s.CertFingerprint())
		go srv.ServeTLS(ln, "", "")
	}
	return srv, extra, nil
}

// selfCert 读取或生成自签名证书。
func (s *Server) selfCert(st Settings) (tls.Certificate, error) {
	dir := filepath.Join(s.dataDir, "tls")
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if c, err := tls.LoadX509KeyPair(cp, kp); err == nil {
		s.setFP(c.Certificate[0])
		return c, nil
	}
	os.MkdirAll(dir, 0o700)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "PVE Forward"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if h, err := os.Hostname(); err == nil {
		tpl.DNSNames = append(tpl.DNSNames, h)
	}
	if u, err := url.Parse(st.URL); err == nil && u.Hostname() != "" {
		if ip := net.ParseIP(u.Hostname()); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, u.Hostname())
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				tpl.IPAddresses = append(tpl.IPAddresses, n.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	s.setFP(der)
	return tls.LoadX509KeyPair(cp, kp)
}

// setFP 计算证书公钥的 SHA-256 固定值（curl --pinnedpubkey 与 Agent 使用同一格式）。
func (s *Server) setFP(der []byte) {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return
	}
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	s.certMu.Lock()
	s.certFP = "sha256//" + base64.StdEncoding.EncodeToString(h[:])
	s.certMu.Unlock()
}

// CertFingerprint 返回自签名证书指纹（Agent 用它校验控制端证书）；非自签名模式返回空。
func (s *Server) CertFingerprint() string {
	tlsMode := s.TLSMode
	if tlsMode == "" {
		tlsMode = s.settings().TLS
	}
	if tlsMode != "self" {
		return ""
	}
	s.certMu.Lock()
	defer s.certMu.Unlock()
	return s.certFP
}

// ---------------- 路由 ----------------

type ctxKey struct{}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// 无需登录
	mux.HandleFunc("GET /api/bootstrap", s.bootstrap)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /status/{slug}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/#/status/"+url.PathEscape(r.PathValue("slug")), http.StatusFound)
	})
	mux.HandleFunc("GET /api/public/{slug}", s.publicStatus)
	mux.HandleFunc("GET /api/public/{slug}/rrd", s.publicRRD)
	mux.HandleFunc("POST /agent/join", s.agentJoin)
	mux.HandleFunc("GET /agent/ws", s.agentWS)
	mux.HandleFunc("GET /agent/install.sh", s.agentInstallScript)
	mux.HandleFunc("GET /agent/download/{arch}", s.agentDownload)

	// 登录用户
	user := func(p string, h http.HandlerFunc) { mux.Handle(p, s.auth(h, false)) }
	user("GET /api/me", s.me)
	user("PUT /api/me/password", s.changePassword)
	user("POST /api/me/2fa/setup", s.post2FASetup)
	user("POST /api/me/2fa/enable", s.post2FAEnable)
	user("POST /api/me/2fa/disable", s.post2FADisable)
	user("GET /api/dashboard", s.dashboard)
	user("GET /api/monitor", s.monitor)
	user("GET /api/monitor/ws", s.monitorWS)
	user("GET /api/monitor/rrd", s.monitorRRD)
	user("GET /api/monitor/traffic", s.monitorTraffic)
	user("GET /api/rules", s.listRules)
	user("POST /api/rules", s.saveRule)
	user("PUT /api/rules/{id}", s.saveRule)
	user("DELETE /api/rules/{id}", s.deleteRule)
	user("POST /api/rules/batch", s.batchRules)
	user("GET /api/rule-options", s.ruleOptions)
	user("GET /api/sites", s.listSites)
	user("POST /api/sites", s.saveSite)
	user("PUT /api/sites/{id}", s.saveSite)
	user("DELETE /api/sites/{id}", s.deleteSite)
	user("POST /api/sites/batch", s.batchSites)
	user("POST /api/sites/check-host", s.checkHost)
	user("GET /api/site-options", s.siteOptions)

	// 管理员
	admin := func(p string, h http.HandlerFunc) { mux.Handle(p, s.auth(h, true)) }
	admin("GET /api/users", s.listUsers)
	admin("POST /api/users", s.saveUser)
	admin("GET /api/users/{id}", s.getUser)
	admin("PUT /api/users/{id}", s.saveUser)
	admin("DELETE /api/users/{id}", s.deleteUser)
	admin("POST /api/users/{id}/action", s.userAction)
	admin("POST /api/users/check-alloc", s.checkAlloc)
	admin("GET /api/nodes", s.listNodes)
	admin("POST /api/nodes", s.createNode)
	admin("GET /api/nodes/{id}", s.getNode)
	admin("PUT /api/nodes/{id}", s.updateNode)
	admin("DELETE /api/nodes/{id}", s.deleteNode)
	admin("POST /api/nodes/ssh-test", s.sshTest)
	admin("POST /api/nodes/{id}/ssh-test", s.nodeSSHTest)
	admin("POST /api/nodes/{id}/trust", s.trustHostKey)
	admin("POST /api/nodes/{id}/token", s.newJoinToken)
	admin("POST /api/nodes/{id}/detect", s.detectNode)
	admin("POST /api/nodes/{id}/init", s.initNode)
	admin("POST /api/nodes/{id}/sync", s.syncNode)
	admin("GET /api/vms", s.listVMs)
	admin("PUT /api/vms/{id}", s.updateVM)
	admin("GET /api/credentials", s.listCreds)
	admin("POST /api/credentials", s.saveCred)
	admin("PUT /api/credentials/{id}", s.saveCred)
	admin("DELETE /api/credentials/{id}", s.deleteCred)
	admin("POST /api/credentials/generate-key", s.generateKey)
	admin("GET /api/settings", s.getSettings)
	admin("PUT /api/settings", s.putSettings)
	admin("GET /api/events", s.listEvents)
	admin("GET /api/update", s.getUpdate)
	admin("POST /api/update/check", s.postUpdateCheck)
	admin("POST /api/update/apply", s.postUpdateApply)
	admin("GET /api/dns-types", s.listDNSTypes)
	admin("GET /api/dns-providers", s.listProviders)
	admin("POST /api/dns-providers", s.saveProvider)
	admin("PUT /api/dns-providers/{id}", s.saveProvider)
	admin("DELETE /api/dns-providers/{id}", s.deleteProvider)
	admin("POST /api/dns-providers/test", s.postTestProvider)
	admin("POST /api/dns-providers/{id}/test", s.postTestProvider)
	admin("GET /api/web-domains", s.listDomains)
	admin("POST /api/web-domains", s.saveDomain)
	admin("PUT /api/web-domains/{id}", s.saveDomain)
	admin("DELETE /api/web-domains/{id}", s.deleteDomain)
	admin("GET /api/certs", s.listCerts)
	admin("GET /api/certs/{id}", s.getCert)
	admin("POST /api/certs", s.saveCert)
	admin("PUT /api/certs/{id}", s.saveCert)
	admin("DELETE /api/certs/{id}", s.deleteCert)
	admin("POST /api/certs/parse", s.parseCert)
	admin("GET /api/web-entries", s.listWebEntries)
	admin("GET /api/web-mirrors", s.listWebMirrors)
	admin("POST /api/nodes/{id}/web/check", s.postWebCheck)
	admin("POST /api/nodes/{id}/web/pubkey", s.postWebPubkey)
	admin("GET /api/dns-providers/{id}/zones", s.providerZones)
	admin("POST /api/nodes/{id}/web", s.postWeb)

	mux.Handle("/", s.spa())
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// spa 提供前端静态文件，未知路径回退到 index.html。
func (s *Server) spa() http.Handler {
	files := http.FileServer(http.FS(s.web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(p, "api/") || strings.HasPrefix(p, "agent/") {
			jsonErr(w, http.StatusNotFound, "接口不存在")
			return
		}
		if p != "" {
			if f, err := s.web.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		b, err := fs.ReadFile(s.web, "index.html")
		if err != nil {
			http.Error(w, "前端未构建", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	})
}

// ---------------- 认证 ----------------

// 会话 Cookie：HTTPS 与 HTTP 使用不同的名称。浏览器不允许 HTTP 页面覆盖同名的 Secure Cookie，
// 若共用一个名称，从 HTTPS 切换为 HTTP（或反向代理）后会因旧 Cookie 无法登录。
const (
	cookieName     = "pf_session"
	cookieNameHTTP = "pf_session_http"
)

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func sessionCookieName(r *http.Request) string {
	if secureRequest(r) {
		return cookieName
	}
	return cookieNameHTTP
}

// sessionToken 返回当前请求的会话令牌，没有时为空。
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookieName(r)); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) auth(h http.HandlerFunc, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.setupDone() {
			jsonErr(w, http.StatusPreconditionRequired, "控制端尚未初始化")
			return
		}
		tok := sessionToken(r)
		if tok == "" {
			jsonErr(w, http.StatusUnauthorized, "请先登录")
			return
		}
		u, err := s.db.SessionUser(secret.Hash(tok))
		if err != nil {
			jsonErr(w, http.StatusUnauthorized, "登录已过期，请重新登录")
			return
		}
		if !u.Enabled && u.Role != "admin" {
			jsonErr(w, http.StatusForbidden, "账号已被禁用")
			return
		}
		if admin && u.Role != "admin" {
			jsonErr(w, http.StatusForbidden, "需要管理员权限")
			return
		}
		if u.Role == "admin" {
			s.rememberOrigin(r)
		}
		// 防 CSRF：写操作要求自定义请求头（跨站表单无法携带）
		if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "pveforward" {
			jsonErr(w, http.StatusForbidden, "请求来源无效")
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func currentUser(r *http.Request) *store.User { return r.Context().Value(ctxKey{}).(*store.User) }

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, uid int64) error {
	tok := secret.RandomToken(32)
	days := s.settings().SessionDays
	if days <= 0 {
		days = 7
	}
	if err := s.db.CreateSession(secret.Hash(tok), uid, time.Duration(days)*24*time.Hour); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName(r), Value: tok, Path: "/", HttpOnly: true, Secure: secureRequest(r),
		SameSite: http.SameSiteLaxMode, MaxAge: days * 86400})
	return nil
}

func hashPassword(p string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(b), err
}

func checkPassword(hash, p string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(p)) == nil
}

// ---------------- 限流（登录失败锁定、公开页） ----------------

type limiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{fails: map[string][]time.Time{}} }

// Blocked 判断 key 在窗口内失败次数是否达到上限。
func (l *limiter) Blocked(key string, max int, window time.Duration) bool {
	if max <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-window)
	var keep []time.Time
	for _, t := range l.fails[key] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	l.fails[key] = keep
	return len(keep) >= max
}

func (l *limiter) Fail(key string) {
	l.mu.Lock()
	l.fails[key] = append(l.fails[key], time.Now())
	l.mu.Unlock()
}

func (l *limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	// 仅在反向代理模式下信任 X-Forwarded-For
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	return h
}

// ---------------- JSON 辅助 ----------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return false
	}
	return true
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func queryInt(r *http.Request, k string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(k), 10, 64)
	return v
}

// fail 根据错误类型返回 4xx / 5xx
func fail(w http.ResponseWriter, err error) {
	var ue userError
	switch {
	case errors.As(err, &ue):
		jsonErr(w, http.StatusBadRequest, ue.msg)
	case errors.Is(err, store.ErrNotFound):
		jsonErr(w, http.StatusNotFound, err.Error())
	default:
		log.Printf("内部错误: %v", err)
		jsonErr(w, http.StatusInternalServerError, err.Error())
	}
}

type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func bad(format string, a ...any) error { return userError{fmt.Sprintf(format, a...)} }
