package server

import (
	"errors"
	"net/http"

	secretbox "pveforward/internal/secret"
	"time"

	"pveforward/internal/store"
	"pveforward/internal/totp"
)

// ---------------- 两步验证（TOTP） ----------------

// check2FA 校验用户的两步验证码（已启用时）。返回是否通过；同一验证码只能使用一次。
func (s *Server) check2FA(u *store.User, code string) bool {
	enc, last, enabled, err := s.db.TOTP(u.ID)
	if err != nil || !enabled {
		return false
	}
	secret, err := s.box.Open(enc)
	if err != nil {
		return false
	}
	step, ok := totp.Verify(secret, code, time.Now(), last)
	return ok && s.db.UseTOTPStep(u.ID, enc, step)
}

// post2FASetup 开始绑定：校验当前密码后生成新密钥，返回二维码（启用前需用验证码确认）。
func (s *Server) post2FASetup(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if u.TwoFA {
		jsonErr(w, http.StatusBadRequest, "已启用两步验证，请先解除绑定")
		return
	}
	key := "2fa:" + clientIP(r) + ":" + u.Username
	if s.limiter.Blocked(key, 5, 15*time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试")
		return
	}
	if !checkPassword(u.PasswordHash, req.Password) {
		s.limiter.Fail(key)
		jsonErr(w, http.StatusBadRequest, "当前密码不正确")
		return
	}
	secret := totp.NewSecret()
	if err := s.db.SetTOTPPending(u, s.box.Seal(secret), secretbox.Hash(sessionToken(r))); err != nil {
		fail(w, err)
		return
	}
	uri := totp.URI(s.settings().Site, u.Username, secret)
	qr, err := totp.QR(uri)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"secret": secret, "uri": uri, "qr": qr})
}

// post2FAEnable 用认证应用生成的验证码确认绑定。
func (s *Server) post2FAEnable(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key := "2fa:" + clientIP(r) + ":" + u.Username
	if s.limiter.Blocked(key, 5, 15*time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试")
		return
	}
	enc, err := s.db.TOTPPending(u.ID, secretbox.Hash(sessionToken(r)))
	if err != nil || len(enc) == 0 {
		jsonErr(w, http.StatusBadRequest, "请先生成二维码")
		return
	}
	secret, err := s.box.Open(enc)
	if err != nil {
		fail(w, err)
		return
	}
	step, ok := totp.Verify(secret, req.Code, time.Now(), 0)
	if !ok {
		s.limiter.Fail(key)
		jsonErr(w, http.StatusBadRequest, "验证码不正确，请确认手机时间准确后重试")
		return
	}
	if err := s.db.EnableTOTP(u.ID, enc, step, secretbox.Hash(sessionToken(r))); err != nil {
		if errors.Is(err, store.ErrTOTPChanged) {
			jsonErr(w, http.StatusConflict, err.Error())
		} else {
			fail(w, err)
		}
		return
	}
	s.limiter.Reset(key)
	s.db.AddEvent("info", u.ID, 0, u.Username+" 已启用两步验证")
	writeJSON(w, map[string]bool{"ok": true})
}

// post2FADisable 解除绑定：需要当前密码和验证码。
func (s *Server) post2FADisable(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key := "2fa:" + clientIP(r) + ":" + u.Username
	if s.limiter.Blocked(key, 5, 15*time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试")
		return
	}
	if !checkPassword(u.PasswordHash, req.Password) {
		s.limiter.Fail(key)
		jsonErr(w, http.StatusBadRequest, "当前密码不正确")
		return
	}
	enc, last, enabled, err := s.db.TOTP(u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if !enabled {
		jsonErr(w, http.StatusBadRequest, "尚未启用两步验证")
		return
	}
	secret, err := s.box.Open(enc)
	if err != nil {
		fail(w, err)
		return
	}
	step, ok := totp.Verify(secret, req.Code, time.Now(), last)
	if !ok {
		s.limiter.Fail(key)
		jsonErr(w, http.StatusBadRequest, "验证码不正确")
		return
	}
	if err := s.db.DisableVerifiedTOTP(u, enc, step, secretbox.Hash(sessionToken(r))); err != nil {
		if errors.Is(err, store.ErrTOTPChanged) {
			jsonErr(w, http.StatusConflict, err.Error())
		} else {
			fail(w, err)
		}
		return
	}
	s.limiter.Reset(key)
	s.db.AddEvent("warn", u.ID, 0, u.Username+" 已解除两步验证")
	writeJSON(w, map[string]bool{"ok": true})
}
