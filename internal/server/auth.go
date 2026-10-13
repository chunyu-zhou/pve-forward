package server

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"pveforward/internal/secret"
	"pveforward/internal/store"
	"pveforward/internal/totp"
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,20}$`)

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	st := s.settings()
	res := map[string]any{"setup": s.setupDone(), "site": st.Site, "version": Version}
	if tok := sessionToken(r); tok != "" && s.setupDone() {
		if u, err := s.db.SessionUser(secret.Hash(tok)); err == nil {
			res["user"] = u
			if u.Role == "admin" {
				s.rememberOrigin(r)
				res["updateAvailable"] = s.loadUpdateStatus().available()
			}
		}
	}
	writeJSON(w, res)
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if s.setupDone() {
		jsonErr(w, http.StatusConflict, "控制端已初始化")
		return
	}
	var req struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
		Site     string `json:"site"`
		URL      string `json:"url"`
		Port     int    `json:"port"`
		TLS      string `json:"tls"`
		Check    bool   `json:"check"` // 仅校验令牌（向导第 1 步）
	}
	if !readJSON(w, r, &req) {
		return
	}
	ip := clientIP(r)
	if s.limiter.Blocked("setup:"+ip, 10, 15*time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试")
		return
	}
	if secret.Hash(strings.ToUpper(strings.TrimSpace(req.Token))) != s.db.Setting("setup_token_hash", "-") {
		s.limiter.Fail("setup:" + ip)
		jsonErr(w, http.StatusBadRequest, "初始化令牌不正确")
		return
	}
	if err := validateAdminCredentials(req.Username, req.Password); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Check {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	u, err := url.Parse(strings.TrimRight(req.URL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Path != "" || u.Port() != "" {
		jsonErr(w, http.StatusBadRequest, "访问地址格式应为 https://域名或IP（不含端口和路径）")
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		jsonErr(w, http.StatusBadRequest, "端口无效")
		return
	}
	if req.TLS != "self" && req.TLS != "acme" && req.TLS != "proxy" {
		req.TLS = "proxy"
	}
	if req.TLS == "acme" && net.ParseIP(u.Hostname()) != nil {
		jsonErr(w, http.StatusBadRequest, "Let's Encrypt 需要使用域名")
		return
	}
	old := s.settings()
	st := old
	st.Site = strings.TrimSpace(req.Site)
	if st.Site == "" {
		st.Site = "PVE Forward"
	}
	st.URL, st.Port, st.TLS = u.String(), req.Port, req.TLS

	admin, err := s.initializeAdmin(req.Username, req.Password, st)
	if errors.Is(err, errAlreadyInitialized) {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, admin.ID)
	restart := s.needRestart(old, st)
	if st.TLS == "self" && old.URL != st.URL {
		// 访问地址变化后重新生成包含新地址的证书
		os.RemoveAll(filepath.Join(s.dataDir, "tls"))
	}
	writeJSON(w, map[string]any{"ok": true, "restart": restart, "url": st.URL, "port": st.Port})
	if restart {
		s.scheduleRestart()
	}
}

// needRestart 判断设置变化是否需要重新监听（命令行覆盖的项不计）。
func (s *Server) needRestart(old, st Settings) bool {
	if s.TLSMode != "" {
		old.TLS, st.TLS = s.TLSMode, s.TLSMode
	}
	if s.listen != "" {
		old.Port = st.Port
	}
	return old.Port != st.Port || old.TLS != st.TLS || (st.TLS != "proxy" && old.URL != st.URL)
}

func (s *Server) scheduleRestart() {
	go func() {
		time.Sleep(time.Second)
		select {
		case s.restart <- struct{}{}:
		default:
		}
	}()
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"` // 两步验证码（已启用时必填）
	}
	if !readJSON(w, r, &req) {
		return
	}
	st := s.settings()
	key := "login:" + clientIP(r) + ":" + strings.ToLower(req.Username)
	if s.limiter.Blocked(key, st.LockAttempts, 15*time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "登录失败次数过多，请 15 分钟后再试")
		return
	}
	u, err := s.db.UserByName(req.Username)
	if err != nil || !checkPassword(u.PasswordHash, req.Password) {
		s.limiter.Fail(key)
		jsonErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if !u.Enabled && u.Role != "admin" {
		jsonErr(w, http.StatusForbidden, "账号已被禁用，请联系管理员")
		return
	}
	tok := secret.RandomToken(32)
	days := st.SessionDays
	if days <= 0 {
		days = 7
	}
	err = s.db.CreateAuthenticatedSession(secret.Hash(tok), u, time.Duration(days)*24*time.Hour, req.Code != "", func(enc []byte, last int64) (int64, bool) {
		plain, err := s.box.Open(enc)
		if err != nil {
			return 0, false
		}
		return totp.Verify(plain, req.Code, time.Now(), last)
	})
	switch {
	case errors.Is(err, store.ErrOTPRequired):
		writeJSON(w, map[string]bool{"need2fa": true})
		return
	case errors.Is(err, store.ErrOTPInvalid):
		s.limiter.Fail(key)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": err.Error(), "need2fa": true})
		return
	case errors.Is(err, store.ErrAuthChanged):
		jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	case err != nil:
		fail(w, err)
		return
	}
	s.limiter.Reset(key)
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName(r), Value: tok, Path: "/", HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: days * 86400})
	writeJSON(w, map[string]any{"user": u})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if tok := sessionToken(r); tok != "" {
		s.db.DeleteSession(secret.Hash(tok))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName(r), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureRequest(r)})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.meInfo(currentUser(r)))
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	key := "password:" + clientIP(r) + ":" + u.Username
	if s.limiter.Blocked(key, 5, 15*time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试")
		return
	}
	if !checkPassword(u.PasswordHash, req.Old) {
		s.limiter.Fail(key)
		jsonErr(w, http.StatusBadRequest, "当前密码不正确")
		return
	}
	if len(req.New) < 8 || len(req.New) > 72 {
		jsonErr(w, http.StatusBadRequest, "新密码须为 8–72 字节（中文等字符占多个字节）")
		return
	}
	h, err := hashPassword(req.New)
	if err != nil {
		fail(w, err)
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE users SET password_hash=?, totp_pending_enc=NULL, totp_pending_expires=0 WHERE id=? AND password_hash=?`, h, u.ID, u.PasswordHash)
	if err != nil {
		fail(w, err)
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		jsonErr(w, http.StatusConflict, "密码已变化，请重新登录后操作")
		return
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, u.ID, secret.Hash(sessionToken(r))); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	s.limiter.Reset(key)
	writeJSON(w, map[string]bool{"ok": true})
}

func removeTLS(dataDir string) { os.RemoveAll(filepath.Join(dataDir, "tls")) }

// meInfo 返回当前用户及其配额（端口分配、已用端口、虚拟机数）。
func (s *Server) meInfo(u *store.User) map[string]any {
	allocs, _ := s.db.Allocations(u.ID)
	if allocs == nil {
		allocs = []*store.Allocation{}
	}
	used := map[int64][]int{}
	rules, _ := s.db.Rules(u.ID, 0)
	for _, r := range rules {
		used[r.NodeID] = append(used[r.NodeID], r.Port)
	}
	nodes := map[int64]string{}
	if ns, err := s.db.Nodes(); err == nil {
		for _, n := range ns {
			nodes[n.ID] = n.Name
		}
	}
	var vms int
	s.db.QueryRow(`SELECT COUNT(*) FROM vms WHERE user_id=? AND present=1`, u.ID).Scan(&vms)
	return map[string]any{"user": u, "allocations": allocs, "usedPorts": used, "nodeNames": nodes, "vms": vms, "rules": len(rules)}
}
