package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"pveforward/internal/store"
)

func newEnvTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, nil, nil, dir, nil, "")
}

func envLookup(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func validBootstrapEnv() map[string]string {
	return map[string]string{
		"PVE_FORWARD_ADMIN_USER":     "admin_test",
		"PVE_FORWARD_ADMIN_PASSWORD": "bootstrap-test-password",
		"PVE_FORWARD_PUBLIC_URL":     "https://panel.example.com:8443",
		"PVE_FORWARD_SITE":           "  Test panel  ",
	}
}

func assertNoEnvInitialization(t *testing.T, s *Server) {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || s.setupDone() {
		t.Fatalf("partial initialization: users=%d setup=%v", count, s.setupDone())
	}
	if settings := s.db.Setting("settings", ""); settings != "" {
		t.Fatalf("unexpected settings: %s", settings)
	}
}

func TestBootstrapEnvEmptyPreservesWebSetup(t *testing.T) {
	s := newEnvTestServer(t)
	token := s.NewSetupToken()
	// Public URL/site alone do not opt into admin bootstrapping.
	if err := s.BootstrapFromEnv(envLookup(map[string]string{
		"PVE_FORWARD_PUBLIC_URL": "not-an-origin", "PVE_FORWARD_SITE": "unused",
	})); err != nil {
		t.Fatal(err)
	}
	assertNoEnvInitialization(t, s)
	b, err := os.ReadFile(filepath.Join(s.dataDir, "setup-token"))
	if err != nil || strings.TrimSpace(string(b)) != token || s.db.Setting("setup_token_hash", "") == "" {
		t.Fatalf("Web setup token was changed: %v", err)
	}
	if err := s.BootstrapFromEnv(envLookup(nil)); err != nil {
		t.Fatal(err)
	}
	assertNoEnvInitialization(t, s)
}

func TestBootstrapEnvFirstStart(t *testing.T) {
	s := newEnvTestServer(t)
	s.NewSetupToken()
	s.TLSMode = "proxy"
	s.listen = ":5000"
	env := validBootstrapEnv()
	if err := s.BootstrapFromEnv(envLookup(env)); err != nil {
		t.Fatal(err)
	}
	u, err := s.db.UserByName(env["PVE_FORWARD_ADMIN_USER"])
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != "admin" || !u.Enabled || u.MaxRules != 0 || !checkPassword(u.PasswordHash, env["PVE_FORWARD_ADMIN_PASSWORD"]) {
		t.Fatal("admin credentials/permissions were not initialized correctly")
	}
	if cost, err := bcrypt.Cost([]byte(u.PasswordHash)); err != nil || cost != bcrypt.DefaultCost {
		t.Fatalf("bcrypt cost=%d err=%v", cost, err)
	}
	st := s.settings()
	if st.URL != "https://panel.example.com" || st.Port != 8443 || st.TLS != "proxy" || st.Site != "Test panel" {
		t.Fatalf("unexpected settings: %+v", st)
	}
	if st.Interval != 60 || st.SessionDays != 7 || !reflect.DeepEqual(st.DefaultReserved, []int{22, 8006}) {
		t.Fatalf("lost default settings: %+v", st)
	}
	if s.agentURL() != env["PVE_FORWARD_PUBLIC_URL"] || s.listen != ":5000" {
		t.Fatal("public Agent port must be independent of the container listener")
	}
	if !s.setupDone() || s.db.Setting("setup_token_hash", "not-cleared") != "" {
		t.Fatal("setup state/token was not finalized")
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "setup-token")); !os.IsNotExist(err) {
		t.Fatalf("setup-token not removed: %v", err)
	}
}

func TestBootstrapEnvRestartPreservesAccountAndSettings(t *testing.T) {
	s := newEnvTestServer(t)
	env := validBootstrapEnv()
	if err := s.BootstrapFromEnv(envLookup(env)); err != nil {
		t.Fatal(err)
	}
	original, err := s.db.UserByName("admin_test")
	if err != nil {
		t.Fatal(err)
	}
	st := s.settings()
	st.Site, st.URL, st.Port, st.TLS = "Edited in UI", "https://changed.example.com", 9443, "acme"
	if err := s.db.SetSettingJSON("settings", st); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted := New(db, nil, nil, s.dataDir, nil, ":5001")
	restarted.TLSMode = "proxy"
	// Even invalid/partial credentials and a missing Docker secret are ignored.
	for _, stale := range []map[string]string{
		{"PVE_FORWARD_ADMIN_USER": "invalid user", "PVE_FORWARD_ADMIN_PASSWORD": "short", "PVE_FORWARD_PUBLIC_URL": "invalid"},
		{"PVE_FORWARD_ADMIN_USER": "new_admin", "PVE_FORWARD_ADMIN_PASSWORD_FILE": filepath.Join(s.dataDir, "missing-secret")},
		{"PVE_FORWARD_ADMIN_PASSWORD": "new-password"},
	} {
		if err := restarted.BootstrapFromEnv(envLookup(stale)); err != nil {
			t.Fatalf("stale bootstrap env rejected on restart: %v", err)
		}
	}
	// Assert the early return does not even read the bootstrap environment.
	if err := restarted.BootstrapFromEnv(func(string) string {
		t.Fatal("read bootstrap env after initialization")
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.UserByName("admin_test")
	if err != nil || got.PasswordHash != original.PasswordHash {
		t.Fatalf("existing admin changed: %v", err)
	}
	if !reflect.DeepEqual(restarted.settings(), st) || restarted.TLSMode != "proxy" || restarted.listen != ":5001" {
		t.Fatal("runtime override changed persisted settings")
	}
	users, err := db.Users()
	if err != nil || len(users) != 1 {
		t.Fatalf("unexpected user count: %d err=%v", len(users), err)
	}
}

func TestBootstrapEnvRejectsInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name string
		edit func(map[string]string)
	}{
		{"user only", func(e map[string]string) { delete(e, "PVE_FORWARD_ADMIN_PASSWORD") }},
		{"password only", func(e map[string]string) { delete(e, "PVE_FORWARD_ADMIN_USER") }},
		{"bad username", func(e map[string]string) { e["PVE_FORWARD_ADMIN_USER"] = "bad user" }},
		{"short username", func(e map[string]string) { e["PVE_FORWARD_ADMIN_USER"] = "aa" }},
		{"long username", func(e map[string]string) { e["PVE_FORWARD_ADMIN_USER"] = strings.Repeat("a", 21) }},
		{"short password", func(e map[string]string) { e["PVE_FORWARD_ADMIN_PASSWORD"] = "secret" }},
		{"long password", func(e map[string]string) { e["PVE_FORWARD_ADMIN_PASSWORD"] = strings.Repeat("x", 73) }},
		{"utf8 byte length", func(e map[string]string) { e["PVE_FORWARD_ADMIN_PASSWORD"] = strings.Repeat("密", 25) }},
		{"missing public origin", func(e map[string]string) { delete(e, "PVE_FORWARD_PUBLIC_URL") }},
		{"origin credentials", func(e map[string]string) { e["PVE_FORWARD_PUBLIC_URL"] = "https://user:secret@example.com" }},
		{"two password sources", func(e map[string]string) { e["PVE_FORWARD_ADMIN_PASSWORD_FILE"] = "missing-secret" }},
		{"unreadable secret", func(e map[string]string) {
			delete(e, "PVE_FORWARD_ADMIN_PASSWORD")
			e["PVE_FORWARD_ADMIN_PASSWORD_FILE"] = filepath.Join(t.TempDir(), "missing-secret")
		}},
		{"http with TLS", func(e map[string]string) { e["PVE_FORWARD_PUBLIC_URL"] = "http://panel.example.com:5000" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newEnvTestServer(t)
			env := validBootstrapEnv()
			tc.edit(env)
			err := s.BootstrapFromEnv(envLookup(env))
			if err == nil {
				t.Fatal("expected configuration error")
			}
			if password := env["PVE_FORWARD_ADMIN_PASSWORD"]; password != "" && strings.Contains(err.Error(), password) {
				t.Fatal("error leaks password")
			}
			assertNoEnvInitialization(t, s)
		})
	}
}

func TestBootstrapEnvRejectsInvalidTLSMode(t *testing.T) {
	for _, mode := range []string{"invalid", "acme"} {
		t.Run(mode, func(t *testing.T) {
			s := newEnvTestServer(t)
			s.TLSMode = mode
			env := validBootstrapEnv()
			env["PVE_FORWARD_PUBLIC_URL"] = "https://192.0.2.1:8443"
			if err := s.BootstrapFromEnv(envLookup(env)); err == nil {
				t.Fatal("expected TLS configuration error")
			}
			assertNoEnvInitialization(t, s)
		})
	}
}

func TestBootstrapEnvPasswordFile(t *testing.T) {
	for _, password := range []string{"12345678", strings.Repeat("p", 72), strings.Repeat("密", 24), "  spaces are kept  "} {
		for _, ending := range []string{"", "\n", "\r\n"} {
			t.Run(fmt.Sprintf("bytes=%d/newline=%q", len(password), ending), func(t *testing.T) {
				s := newEnvTestServer(t)
				path := filepath.Join(s.dataDir, "admin-password")
				if err := os.WriteFile(path, []byte(password+ending), 0o600); err != nil {
					t.Fatal(err)
				}
				env := validBootstrapEnv()
				delete(env, "PVE_FORWARD_ADMIN_PASSWORD")
				env["PVE_FORWARD_ADMIN_PASSWORD_FILE"] = path
				if err := s.BootstrapFromEnv(envLookup(env)); err != nil {
					t.Fatal(err)
				}
				u, err := s.db.UserByName("admin_test")
				if err != nil || !checkPassword(u.PasswordHash, password) {
					t.Fatalf("secret password not preserved: %v", err)
				}
			})
		}
	}
	for _, password := range []string{"", "short\n", strings.Repeat("x", 73), strings.Repeat("x", 4096)} {
		s := newEnvTestServer(t)
		path := filepath.Join(s.dataDir, "invalid-password")
		if err := os.WriteFile(path, []byte(password), 0o600); err != nil {
			t.Fatal(err)
		}
		env := validBootstrapEnv()
		delete(env, "PVE_FORWARD_ADMIN_PASSWORD")
		env["PVE_FORWARD_ADMIN_PASSWORD_FILE"] = path
		if err := s.BootstrapFromEnv(envLookup(env)); err == nil {
			t.Fatal("invalid file password accepted")
		}
		assertNoEnvInitialization(t, s)
	}
}

func TestParsePublicOrigin(t *testing.T) {
	for _, tc := range []struct {
		input, base string
		port        int
	}{
		{"https://panel.example.com:8443", "https://panel.example.com", 8443},
		{"https://panel.example.com/", "https://panel.example.com", 443},
		{"http://192.0.2.1", "http://192.0.2.1", 80},
		{"https://[2001:db8::1]:8443/", "https://[2001:db8::1]", 8443},
		{"https://[2001:db8::1]", "https://[2001:db8::1]", 443},
		{"http://[::1]:80", "http://[::1]", 80},
	} {
		t.Run(tc.input, func(t *testing.T) {
			base, port, err := parsePublicOrigin(tc.input)
			if err != nil || base != tc.base || port != tc.port {
				t.Fatalf("got %q:%d, err=%v", base, port, err)
			}
			s := newEnvTestServer(t)
			s.TLSMode = "proxy"
			env := validBootstrapEnv()
			env["PVE_FORWARD_PUBLIC_URL"] = tc.input
			if err := s.BootstrapFromEnv(envLookup(env)); err != nil {
				t.Fatal(err)
			}
			if got := s.agentURL(); got != fmt.Sprintf("%s:%d", tc.base, tc.port) {
				t.Fatalf("Agent origin=%q", got)
			}
		})
	}
	for _, input := range []string{
		"", "panel.example.com:8443", "ftp://panel.example.com", "https:///", "https://panel.example.com/path",
		"https://panel.example.com?", "https://panel.example.com?q=1", "https://panel.example.com#", "https://panel.example.com#fragment",
		"https://user:password@panel.example.com", "https://panel.example.com:0", "https://panel.example.com:65536",
		"https://panel.example.com:-1", "https://panel.example.com:abc", "https://panel.example.com:", "https://2001:db8::1",
		"https://[not-ip]:443", "https://[example.com]", "https://[::1]:", "https://panel.example.com/%2f",
		"https://evil'host", "https://evil;host", "https://evil$(command)",
	} {
		if _, _, err := parsePublicOrigin(input); err == nil {
			t.Errorf("accepted invalid origin %q", input)
		}
	}
}

func TestBootstrapEnvRollsBackAllSetupWrites(t *testing.T) {
	for _, failingKey := range []string{"settings", "setup_done", "setup_token_hash"} {
		t.Run(failingKey, func(t *testing.T) {
			s := newEnvTestServer(t)
			s.NewSetupToken()
			tokenHash := s.db.Setting("setup_token_hash", "")
			// Fail after user insertion; the last case also rolls back both
			// settings and setup_done, proving there is no partial setup.
			_, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_bootstrap BEFORE INSERT ON settings WHEN NEW.key='%s' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, failingKey))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.BootstrapFromEnv(envLookup(validBootstrapEnv())); err == nil {
				t.Fatal("expected injected database failure")
			}
			assertNoEnvInitialization(t, s)
			if s.db.Setting("setup_token_hash", "") != tokenHash {
				t.Fatal("setup token was invalidated before commit")
			}
			if _, err := os.Stat(filepath.Join(s.dataDir, "setup-token")); err != nil {
				t.Fatal("setup token file removed before commit")
			}
			if _, err := s.db.Exec(`DROP TRIGGER fail_bootstrap`); err != nil {
				t.Fatal(err)
			}
			if err := s.BootstrapFromEnv(envLookup(validBootstrapEnv())); err != nil {
				t.Fatalf("retry after rollback failed: %v", err)
			}
		})
	}
}

func TestWebSetupUsesAtomicInitialization(t *testing.T) {
	s := newEnvTestServer(t)
	token := s.NewSetupToken()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_setup BEFORE INSERT ON settings WHEN NEW.key='setup_done' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"token": token, "username": "web_admin", "password": "web-test-password", "url": "https://panel.example.com", "port": 5000, "tls": "self",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.setup(response, httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(payload)))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	assertNoEnvInitialization(t, s)
}

func TestInitializeAdminDoesNotOverwriteCompletedSetup(t *testing.T) {
	s := newEnvTestServer(t)
	if err := s.BootstrapFromEnv(envLookup(validBootstrapEnv())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.initializeAdmin("second_admin", "another-password", s.settings()); err != errAlreadyInitialized {
		t.Fatalf("expected transactional setup guard, got %v", err)
	}
}

func TestCertFingerprintUsesRuntimeTLS(t *testing.T) {
	s := newEnvTestServer(t)
	s.certFP = "test-fingerprint"
	for _, tc := range []struct{ stored, override, want string }{
		{"self", "", "test-fingerprint"},
		{"self", "proxy", ""},
		{"self", "acme", ""},
		{"proxy", "self", "test-fingerprint"},
		{"proxy", "", ""},
	} {
		st := s.settings()
		st.TLS = tc.stored
		if err := s.db.SetSettingJSON("settings", st); err != nil {
			t.Fatal(err)
		}
		s.TLSMode = tc.override
		if got := s.CertFingerprint(); got != tc.want {
			t.Errorf("stored=%s override=%s got=%q want=%q", tc.stored, tc.override, got, tc.want)
		}
	}
}
