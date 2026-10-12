package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pveforward/internal/store"
	"pveforward/internal/update"
)

// nextPatch 返回比当前版本高一个修订号的版本。
func nextPatch(t *testing.T) string {
	var a, b, c int
	if _, err := fmt.Sscanf(Version, "%d.%d.%d", &a, &b, &c); err != nil {
		t.Fatalf("无法解析当前版本 %q", Version)
	}
	return fmt.Sprintf("%d.%d.%d", a, b, c+1)
}

func TestUpdateCheckAndApply(t *testing.T) {
	s := newEnvTestServer(t)
	st := s.settings()
	st.URL = "https://panel.example.com"
	if _, err := s.initializeAdmin("admin_t", "admin-pass-1", st); err != nil {
		t.Fatal(err)
	}
	tx, _ := s.db.Begin()
	hash, _ := hashPassword("alice-pass-1")
	if err := s.db.CreateUser(tx, &store.User{Username: "alice", PasswordHash: hash, Role: "user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	newer := nextPatch(t)
	published := false
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/version.json":
			fmt.Fprintf(w, `{"version":%q,"notes":"新功能"}`, newer)
		case r.URL.Path == "/rel/v"+newer+"/SHA256SUMS" && published:
			fmt.Fprintf(w, "%s  pve-forward-linux-amd64\n", strings.Repeat("a", 64))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	s.upd.source = &update.Source{VersionURL: gh.URL + "/version.json", ReleaseBase: gh.URL + "/rel"}
	s.upd.install = func(_ context.Context, in *update.Installer, v string, p update.Progress) (string, error) {
		p("下载", 50)
		path := update.BinaryPath(in.DataDir, v)
		os.MkdirAll(filepath.Dir(path), 0o700)
		return path, os.WriteFile(path, []byte("bin"), 0o755)
	}

	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	admin := newClient(t, ts.URL)
	admin.do("POST", "/api/login", map[string]string{"username": "admin_t", "password": "admin-pass-1"}, 200, nil)
	alice := newClient(t, ts.URL)
	alice.do("POST", "/api/login", map[string]string{"username": "alice", "password": "alice-pass-1"}, 200, nil)
	alice.do("GET", "/api/update", nil, http.StatusForbidden, nil)
	alice.do("POST", "/api/update/apply", map[string]string{"version": newer}, http.StatusForbidden, nil)

	type info struct {
		Current   string       `json:"current"`
		Available string       `json:"available"`
		Ready     bool         `json:"ready"`
		Latest    *update.Info `json:"latest"`
		LastCheck int64        `json:"lastCheck"`
		AutoCheck bool         `json:"autoCheck"`
		Job       updateJob    `json:"job"`
	}
	var got info
	admin.do("GET", "/api/update", nil, 200, &got)
	if got.Current != Version || got.Available != "" || got.LastCheck != 0 || !got.AutoCheck {
		t.Fatalf("初始状态 %+v", got)
	}

	// version.json 已更新但 GitHub 仍在构建：提示新版本但不可更新
	admin.do("POST", "/api/update/check", nil, 200, &got)
	if got.Latest == nil || got.Latest.Version != newer || got.Ready || got.Available != "" || got.LastCheck == 0 {
		t.Fatalf("未发布时 %+v", got)
	}
	admin.do("POST", "/api/update/apply", map[string]string{"version": newer}, http.StatusBadRequest, nil)

	published = true
	admin.do("POST", "/api/update/check", nil, 200, &got)
	if got.Available != newer || !got.Ready || got.Latest.Notes != "新功能" {
		t.Fatalf("发布后 %+v", got)
	}
	var boot map[string]any
	admin.do("GET", "/api/bootstrap", nil, 200, &boot)
	if boot["updateAvailable"] != newer {
		t.Fatalf("bootstrap 应提示新版本：%v", boot)
	}
	var aliceBoot map[string]any
	alice.do("GET", "/api/bootstrap", nil, 200, &aliceBoot)
	if _, ok := aliceBoot["updateAvailable"]; ok {
		t.Fatal("普通用户不应看到更新提示")
	}
	ev, _ := s.db.Events(0, 10)
	if len(ev) == 0 || !strings.Contains(ev[0].Message, "发现新版本 v"+newer) {
		t.Fatalf("应记录发现新版本事件：%+v", ev)
	}
	// 重复检测不重复通知
	admin.do("POST", "/api/update/check", nil, 200, nil)
	if ev2, _ := s.db.Events(0, 10); len(ev2) != len(ev) {
		t.Fatal("同一版本不应重复通知")
	}

	admin.do("POST", "/api/update/apply", map[string]string{"version": "99.0.0"}, http.StatusBadRequest, nil)
	admin.do("POST", "/api/update/apply", map[string]string{"version": newer}, 200, nil)
	select {
	case up := <-s.upd.upgrade:
		if up.Version != newer || up.Path != update.BinaryPath(s.dataDir, newer) {
			t.Fatalf("切换请求 %+v", up)
		}
	case <-time.After(10 * time.Second):
		admin.do("GET", "/api/update", nil, 200, &got)
		t.Fatalf("未发出切换请求：%+v", got.Job)
	}
	us, err := update.ReadState(s.dataDir)
	if err != nil || us.Current != newer || !us.Pending || us.Attempts != 1 {
		t.Fatalf("更新状态 %+v %v", us, err)
	}
	if b, _ := filepath.Glob(filepath.Join(s.dataDir, "backups", "pre-update-*.db")); len(b) != 1 {
		t.Fatalf("更新前应备份数据库：%v", b)
	}
	admin.do("GET", "/api/update", nil, 200, &got)
	if !got.Job.Running || got.Job.Percent != 100 {
		t.Fatalf("任务状态 %+v", got.Job)
	}
	admin.do("POST", "/api/update/apply", map[string]string{"version": newer}, http.StatusConflict, nil)
}

func TestUpdateCheckNetworkErrorKeepsLastKnown(t *testing.T) {
	s := newEnvTestServer(t)
	newer := nextPatch(t)
	s.db.SetSettingJSON("update_status", updateStatus{LastCheck: 1, Latest: &update.Info{Version: newer}, Ready: true})
	s.upd.source = &update.Source{VersionURL: "http://127.0.0.1:1/version.json", ReleaseBase: "http://127.0.0.1:1/rel"}
	st := s.checkUpdate(context.Background())
	if st.Error == "" || st.available() != newer || st.LastCheck <= 1 {
		t.Fatalf("网络错误时应保留上次结果：%+v", st)
	}
}

func TestValidMirror(t *testing.T) {
	for in, want := range map[string]string{"": "", " https://gh.example.com ": "https://gh.example.com/", "https://10.0.0.2:8443/gh/": "https://10.0.0.2:8443/gh/"} {
		if got, ok := validMirror(in); !ok || got != want {
			t.Errorf("validMirror(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"http://gh.example.com/", "ftp://x/", "gh.example.com", "https://u:p@x/", "https://x/?a=1", "https://x/#f"} {
		if _, ok := validMirror(bad); ok {
			t.Errorf("validMirror(%q) 应拒绝", bad)
		}
	}
}
