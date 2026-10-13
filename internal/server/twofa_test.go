package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"pveforward/internal/manager"
	"pveforward/internal/secret"
	"pveforward/internal/store"
	"pveforward/internal/totp"
)

// 两步验证：绑定（需要密码与验证码确认）、登录要求验证码、验证码不能重复使用、解除绑定、管理员重置。
func Test2FA(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, _ := secret.Open(filepath.Join(dir, "master.key"))
	srv := New(db, box, manager.New(db, box), dir, fstest.MapFS{"index.html": {Data: []byte("<html>")}}, "")
	if _, err := srv.initializeAdmin("admin", "adminpass1", srv.settings()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	a := newClient(t, ts.URL)
	login := func(c *client, code string, want int) map[string]any {
		var out map[string]any
		c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "adminpass1", "code": code}, want, &out)
		return out
	}
	login(a, "", 200)
	other := newClient(t, ts.URL)
	login(other, "", 200)

	// 绑定：需要当前密码；验证码错误不能启用
	a.do("POST", "/api/me/2fa/setup", map[string]string{"password": "wrong"}, 400, nil)
	var setup struct{ Secret, URI, QR string }
	a.do("POST", "/api/me/2fa/setup", map[string]string{"password": "adminpass1"}, 200, &setup)
	if setup.Secret == "" || setup.QR == "" {
		t.Fatalf("绑定信息 %+v", setup)
	}
	a.do("POST", "/api/me/2fa/enable", map[string]string{"code": "000000"}, 400, nil)
	// 用上一个时间步的验证码确认，登录时当前时间步的验证码仍可使用
	a.do("POST", "/api/me/2fa/enable", map[string]string{"code": totp.Code(setup.Secret, time.Now().Add(-30*time.Second))}, 200, nil)
	other.do("GET", "/api/me", nil, http.StatusUnauthorized, nil)
	var me struct{ User store.User }
	a.do("GET", "/api/me", nil, 200, &me)
	if !me.User.TwoFA {
		t.Fatal("应已启用")
	}

	// 登录：密码正确但缺少验证码 → 提示输入，不建立会话
	b := newClient(t, ts.URL)
	if out := login(b, "", 200); out["need2fa"] != true || out["user"] != nil {
		t.Fatalf("缺少验证码 %v", out)
	}
	b.do("GET", "/api/me", nil, http.StatusUnauthorized, nil)
	login(b, "123456", http.StatusUnauthorized)
	code := totp.Code(setup.Secret, time.Now())
	if out := login(b, code, 200); out["user"] == nil {
		t.Fatalf("验证码登录 %v", out)
	}
	login(newClient(t, ts.URL), code, http.StatusUnauthorized) // 同一验证码不能再次使用

	// 解除：需要密码与验证码
	a.do("POST", "/api/me/2fa/disable", map[string]string{"password": "adminpass1", "code": "111111"}, 400, nil)
	db.Exec(`UPDATE users SET totp_last=0`) // 允许测试中复用当前验证码
	a.do("POST", "/api/me/2fa/disable", map[string]string{"password": "adminpass1", "code": totp.Code(setup.Secret, time.Now())}, 200, nil)
	login(newClient(t, ts.URL), "", 200)

	// 管理员重置用户的两步验证
	u, _ := db.UserByName("admin")
	enc := box.Seal(setup.Secret)
	if _, err := db.Exec(`UPDATE users SET totp_secret_enc=?, totp_enabled=1, totp_last=0 WHERE id=?`, enc, u.ID); err != nil {
		t.Fatal(err)
	}
	a.do("POST", "/api/users/1/action", map[string]string{"action": "reset-2fa"}, 200, nil)
	if out := login(newClient(t, ts.URL), "", 200); out["user"] == nil {
		t.Fatal("重置后应无需验证码")
	}
	a.do("GET", "/api/me", nil, http.StatusUnauthorized, nil)

	// 普通用户同样可绑定，且不能改变管理员的绑定状态。
	hash, err := hashPassword("userpass1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(username,password_hash,created_at) VALUES('alice',?,0)`, hash); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, ts.URL)
	c.do("POST", "/api/login", map[string]string{"username": "alice", "password": "userpass1"}, 200, nil)
	c.do("POST", "/api/me/2fa/setup", map[string]string{"password": "userpass1"}, 200, &setup)
	for i := 0; i < 5; i++ {
		c.do("POST", "/api/me/2fa/enable", map[string]string{"code": "invalid"}, 400, nil)
	}
	c.do("POST", "/api/me/2fa/enable", map[string]string{"code": totp.Code(setup.Secret, time.Now())}, 429, nil)
}

func TestTOTPStateTransitions(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES(1,'alice','unused',0),(2,'bob','unused',0)`)
	if err != nil {
		t.Fatal(err)
	}
	u1, err := db.User(1)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := db.User(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession("one", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession("two", 2, time.Hour); err != nil {
		t.Fatal(err)
	}
	a, b := []byte("first encrypted secret"), []byte("second encrypted secret")
	if err := db.SetTOTPPending(u1, a, "one"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTOTPPending(u1, b, "one"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession("other", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TOTPPending(1, "other"); err == nil {
		t.Fatal("pending secret visible to another session")
	}
	if err := db.EnableTOTP(1, b, 1, "other"); !errors.Is(err, store.ErrTOTPChanged) {
		t.Fatalf("cross-session enrollment accepted: %v", err)
	}
	if err := db.EnableTOTP(1, a, 1, "one"); !errors.Is(err, store.ErrTOTPChanged) {
		t.Fatalf("stale enrollment: %v", err)
	}
	if err := db.EnableTOTP(1, b, 1, "one"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTOTPPending(u1, a, "one"); !errors.Is(err, store.ErrTOTPChanged) {
		t.Fatalf("overwrite enabled binding: %v", err)
	}
	if db.UseTOTPStep(1, a, 2) {
		t.Fatal("accepted different secret")
	}
	if !db.UseTOTPStep(1, b, 2) || db.UseTOTPStep(1, b, 2) {
		t.Fatal("replay protection")
	}
	if err := db.DisableVerifiedTOTP(u1, a, 3, "one"); !errors.Is(err, store.ErrTOTPChanged) {
		t.Fatalf("stale disable: %v", err)
	}
	if err := db.SetTOTPPending(u2, a, "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE users SET totp_pending_expires=1 WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnableTOTP(2, a, 1, "two"); !errors.Is(err, store.ErrTOTPChanged) {
		t.Fatalf("expired enrollment: %v", err)
	}
	if _, _, enabled, err := db.TOTP(2); err != nil || enabled {
		t.Fatal("another user's binding changed")
	}
	if err := db.DisableTOTP(1); err != nil {
		t.Fatal(err)
	}
	if db.UseTOTPStep(1, b, 3) {
		t.Fatal("accepted disabled binding")
	}
}

func TestAuthenticatedSessionRejectsStaleSnapshot(t *testing.T) {
	for _, change := range []string{
		`UPDATE users SET totp_enabled=1,totp_secret_enc=x'01' WHERE id=1`,
		`UPDATE users SET password_hash='replacement' WHERE id=1`,
		`UPDATE users SET enabled=0 WHERE id=1`,
	} {
		t.Run(change, func(t *testing.T) {
			db, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES(1,'alice','original',0)`); err != nil {
				t.Fatal(err)
			}
			before, err := db.User(1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(change); err != nil {
				t.Fatal(err)
			}
			err = db.CreateAuthenticatedSession("stale", before, time.Hour, false, nil)
			if !errors.Is(err, store.ErrAuthChanged) {
				t.Fatalf("stale authentication accepted: %v", err)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("sessions=%d err=%v", count, err)
			}
		})
	}
}

func TestAuthenticatedSessionRollsBackOTP(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,created_at,totp_enabled,totp_secret_enc) VALUES(1,'alice','hash',0,1,x'01')`); err != nil {
		t.Fatal(err)
	}
	u, err := db.User(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession("duplicate", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	verify := func(_ []byte, last int64) (int64, bool) { return 10, last < 10 }
	if err := db.CreateAuthenticatedSession("duplicate", u, time.Hour, true, verify); err == nil {
		t.Fatal("expected session insert failure")
	}
	_, last, _, err := db.TOTP(1)
	if err != nil || last != 0 {
		t.Fatalf("OTP consumed despite rollback: %d %v", last, err)
	}
	if err := db.CreateAuthenticatedSession("valid", u, time.Hour, true, verify); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAuthenticatedSession("replay", u, time.Hour, true, verify); !errors.Is(err, store.ErrOTPInvalid) {
		t.Fatalf("replay accepted: %v", err)
	}
}

func TestPasswordChangeValidation(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.Open(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(db, box, manager.New(db, box), dir, fstest.MapFS{"index.html": {Data: []byte("<html>")}}, "")
	if _, err := srv.initializeAdmin("admin", "adminpass1", srv.settings()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	a, b := newClient(t, ts.URL), newClient(t, ts.URL)
	for _, c := range []*client{a, b} {
		c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "adminpass1"}, 200, nil)
	}
	a.do("PUT", "/api/me/password", map[string]string{"old": "wrong", "new": "newpass123"}, 400, nil)
	a.do("PUT", "/api/me/password", map[string]string{"old": "adminpass1", "new": strings.Repeat("a", 73)}, 400, nil)
	a.do("PUT", "/api/me/password", map[string]string{"old": "adminpass1", "new": strings.Repeat("中", 25)}, 400, nil)
	if _, err := db.Exec(`CREATE TRIGGER reject_password BEFORE UPDATE OF password_hash ON users BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	a.do("PUT", "/api/me/password", map[string]string{"old": "adminpass1", "new": "newpass123"}, 500, nil)
	b.do("GET", "/api/me", nil, 200, nil)
	if _, err := db.Exec(`DROP TRIGGER reject_password`); err != nil {
		t.Fatal(err)
	}
	a.do("PUT", "/api/me/password", map[string]string{"old": "adminpass1", "new": "newpass123"}, 200, nil)
	a.do("GET", "/api/me", nil, 200, nil)
	b.do("GET", "/api/me", nil, 401, nil)
	b.do("POST", "/api/login", map[string]string{"username": "admin", "password": "adminpass1"}, 401, nil)
	b.do("POST", "/api/login", map[string]string{"username": "admin", "password": "newpass123"}, 200, nil)
}
