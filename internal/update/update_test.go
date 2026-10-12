package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.9", "1.0.10", -1},
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "10.0.0", -1},
		{"1.2.0-rc.1", "1.2.0", -1},
		{"1.2.0-rc.2", "1.2.0-rc.10", -1},
		{"1.2.0-alpha", "1.2.0-beta", -1},
		{"1.2.0-1", "1.2.0-alpha", -1},
		{"bad", "0.0.1", -1},
		{"1.0.0", "v1.0.0", 1}, // 带 v 前缀不是合法版本号
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
		if got := Compare(c.b, c.a); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d，期望 %d", c.b, c.a, got, -c.want)
		}
	}
	for _, bad := range []string{"", "1.0", "01.0.0", "1.0.0.0", "1.0.0-", "../1.0.0", "1.0.0/x"} {
		if Valid(bad) {
			t.Errorf("%q 不应是合法版本号", bad)
		}
	}
}

// release 模拟 GitHub：version.json 与 Release 文件。
type release struct {
	info      string
	files     map[string][]byte // 文件名 → 内容；为空表示尚未发布
	badSum    string            // 该文件在 SHA256SUMS 中写入错误的摘要
	prefix    string            // 模拟下载加速前缀
	requested []string
}

func (r *release) handler(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		r.requested = append(r.requested, p)
		if r.prefix != "" {
			// 加速前缀形如 <mirror>/http://upstream/...，还原为上游路径
			if !strings.HasPrefix(p, r.prefix) {
				http.NotFound(w, req)
				return
			}
			p = "/" + strings.TrimPrefix(p, r.prefix)
		}
		switch {
		case p == "/version.json":
			w.Write([]byte(r.info))
		case p == "/rel/v"+version+"/SHA256SUMS" && r.files != nil:
			for name, b := range r.files {
				sum := sha256.Sum256(b)
				h := hex.EncodeToString(sum[:])
				if name == r.badSum {
					h = strings.Repeat("0", 64)
				}
				fmt.Fprintf(w, "%s  %s\n", h, name)
			}
		case strings.HasPrefix(p, "/rel/v"+version+"/") && r.files != nil:
			if b, ok := r.files[strings.TrimPrefix(p, "/rel/v"+version+"/")]; ok {
				w.Write(b)
				return
			}
			http.NotFound(w, req)
		default:
			http.NotFound(w, req)
		}
	})
}

func assets(arch string) map[string][]byte {
	m := map[string][]byte{}
	for _, a := range Assets(arch) {
		m[a] = []byte("binary " + a)
	}
	return m
}

func TestSourceLatestAndChecksums(t *testing.T) {
	r := &release{info: `{"version":"v1.2.3","date":"2026-10-12","notes":"修复问题"}`}
	ts := httptest.NewServer(r.handler("1.2.3"))
	defer ts.Close()
	src := Source{VersionURL: ts.URL + "/version.json", ReleaseBase: ts.URL + "/rel/"}

	info, err := src.Latest(context.Background())
	if err != nil || info.Version != "1.2.3" || info.Notes != "修复问题" {
		t.Fatalf("Latest = %+v, %v", info, err)
	}
	if _, err := src.Checksums(context.Background(), "1.2.3"); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("未发布时应返回 ErrNotPublished：%v", err)
	}
	r.files = assets("amd64")
	sums, err := src.Checksums(context.Background(), "1.2.3")
	if err != nil || len(sums) != 3 {
		t.Fatalf("Checksums = %v, %v", sums, err)
	}

	r.info = `{"version":"1.2"}`
	if _, err := src.Latest(context.Background()); err == nil {
		t.Fatal("非法版本号应报错")
	}
	r.info = `<html>`
	if _, err := src.Latest(context.Background()); err == nil {
		t.Fatal("非 JSON 应报错")
	}
}

func TestSourceMirror(t *testing.T) {
	r := &release{info: `{"version":"1.2.3"}`, files: assets("arm64"), prefix: "/mirror/http://upstream/"}
	ts := httptest.NewServer(r.handler("1.2.3"))
	defer ts.Close()
	// 加速前缀拼接在原始地址之前，例如 https://mirror/https://github.com/...
	src := Source{VersionURL: "http://upstream/version.json", ReleaseBase: "http://upstream/rel", Mirror: ts.URL + "/mirror/"}
	if info, err := src.Latest(context.Background()); err != nil || info.Version != "1.2.3" {
		t.Fatalf("经加速前缀读取失败：%+v %v（%v）", info, err, r.requested)
	}
	if _, err := src.Checksums(context.Background(), "1.2.3"); err != nil {
		t.Fatalf("经加速前缀读取校验文件失败：%v（%v）", err, r.requested)
	}
}

func TestInstall(t *testing.T) {
	r := &release{info: `{"version":"1.2.3"}`, files: assets("arm64")}
	ts := httptest.NewServer(r.handler("1.2.3"))
	defer ts.Close()
	dir := t.TempDir()
	var verified string
	in := &Installer{DataDir: dir, Arch: "arm64", Source: Source{VersionURL: ts.URL + "/version.json", ReleaseBase: ts.URL + "/rel"},
		Verify: func(_ context.Context, path, version string) error { verified = path; return nil }}

	var steps []string
	path, err := in.Install(context.Background(), "1.2.3", func(s string, p int) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	// 自检在临时目录中进行，通过后才移入最终目录
	if path != BinaryPath(dir, "1.2.3") || verified == "" || verified == path || filepath.Base(verified) != "pve-forward" {
		t.Fatalf("path=%s verified=%s", path, verified)
	}
	for name, want := range map[string]string{"pve-forward": "binary pve-forward-linux-arm64",
		"pve-forward-agent-linux-amd64": "binary pve-forward-agent-linux-amd64", "pve-forward-agent-linux-arm64": "binary pve-forward-agent-linux-arm64"} {
		if b, err := os.ReadFile(filepath.Join(dir, "updates", "1.2.3", name)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", name, b, err)
		}
	}
	if len(steps) == 0 {
		t.Error("没有报告进度")
	}

	// 校验失败：不留下版本目录与临时文件
	r.badSum = "pve-forward-agent-linux-amd64"
	os.RemoveAll(filepath.Join(dir, "updates"))
	if _, err := in.Install(context.Background(), "1.2.3", nil); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("摘要不符应失败：%v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "updates")); len(entries) != 0 {
		t.Fatalf("失败后残留文件：%v", entries)
	}

	// 新程序自检失败也不安装
	r.badSum = ""
	in.Verify = func(context.Context, string, string) error { return errors.New("版本不一致") }
	if _, err := in.Install(context.Background(), "1.2.3", nil); err == nil {
		t.Fatal("自检失败应报错")
	}
	if _, err := os.Stat(BinaryPath(dir, "1.2.3")); err == nil {
		t.Fatal("自检失败不应安装")
	}

	// 缺少文件
	delete(r.files, "pve-forward-linux-arm64")
	if _, err := in.Install(context.Background(), "1.2.3", nil); err == nil {
		t.Fatal("缺少控制端文件应报错")
	}
	if _, err := in.Install(context.Background(), "../x", nil); err == nil {
		t.Fatal("非法版本号应报错")
	}
}

func fakeInstalled(t *testing.T, dataDir, version string) string {
	t.Helper()
	p := BinaryPath(dataDir, version)
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanAndRollback(t *testing.T) {
	dir := t.TempDir()
	// 无在线更新
	if got, err := plan(dir, "1.0.0"); got != "" || err != nil {
		t.Fatalf("无状态时 = %q %v", got, err)
	}

	// 刚激活的新版本：第一次启动尝试计数后切换
	p2 := fakeInstalled(t, dir, "1.0.2")
	WriteState(dir, &State{Current: "1.0.2", Pending: true})
	if got, _ := plan(dir, "1.0.0"); got != p2 {
		t.Fatalf("应切换到 %s，实际 %q", p2, got)
	}
	if st, _ := ReadState(dir); st.Attempts != 1 || !st.Pending {
		t.Fatalf("应记录启动尝试：%+v", st)
	}
	// 新版本未确认就再次启动（崩溃后被重启）：回退到程序本体
	if got, _ := plan(dir, "1.0.0"); got != "" {
		t.Fatalf("应回退到程序本体，实际 %q", got)
	}
	st, _ := ReadState(dir)
	if st.Current != "" || st.Failed != "1.0.2" || st.Pending {
		t.Fatalf("回退状态 %+v", st)
	}
	if _, err := os.Stat(p2); err == nil {
		t.Fatal("失败的版本目录应被清理")
	}

	// 从在线更新版本 1.0.2 更新到 1.0.3 失败：回退到 1.0.2
	p2 = fakeInstalled(t, dir, "1.0.2")
	fakeInstalled(t, dir, "1.0.3")
	WriteState(dir, &State{Current: "1.0.3", Previous: "1.0.2", Pending: true, Attempts: 1})
	if got, _ := plan(dir, "1.0.0"); got != p2 {
		t.Fatalf("应回退到 1.0.2，实际 %q", got)
	}

	// 确认正常运行后不再回退，并清理旧版本
	p3 := fakeInstalled(t, dir, "1.0.3")
	fakeInstalled(t, dir, "1.0.1")
	WriteState(dir, &State{Current: "1.0.3", Previous: "1.0.2", Pending: true, Attempts: 1})
	if err := Confirm(dir, "1.0.3"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got, _ := plan(dir, "1.0.0"); got != p3 {
			t.Fatalf("确认后应一直运行 1.0.3，实际 %q", got)
		}
	}
	if _, err := os.Stat(BinaryPath(dir, "1.0.1")); err == nil {
		t.Fatal("确认后应清理无关的旧版本")
	}

	// 程序本体（新拉取的镜像）不旧于在线更新版本：使用本体并清理
	if got, _ := plan(dir, "1.0.3"); got != "" {
		t.Fatalf("本体版本相同应使用本体，实际 %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "updates", "state.json")); err == nil {
		t.Fatal("应清理更新状态")
	}
	if _, err := os.Stat(p3); err == nil {
		t.Fatal("应清理在线更新版本")
	}

	// 程序文件丢失时使用本体
	WriteState(dir, &State{Current: "1.0.9"})
	if got, _ := plan(dir, "1.0.0"); got != "" {
		t.Fatalf("文件不存在应使用本体，实际 %q", got)
	}

	// 状态文件被篡改为路径穿越
	os.WriteFile(filepath.Join(dir, "updates", "state.json"), []byte(`{"current":"../../bin"}`), 0o600)
	if got, err := plan(dir, "1.0.0"); got != "" || err == nil {
		t.Fatalf("非法状态应拒绝：%q %v", got, err)
	}
}

func TestActivate(t *testing.T) {
	dir := t.TempDir()
	// 当前运行的是程序本体：失败时回退到本体
	Activate(dir, "1.0.2", "1.0.0", "/usr/local/bin/pve-forward")
	if st, _ := ReadState(dir); st.Current != "1.0.2" || st.Previous != "" || !st.Pending || st.Attempts != 1 {
		t.Fatalf("%+v", st)
	}
	// 当前运行的是在线更新版本：失败时回退到它
	Activate(dir, "1.0.3", "1.0.2", BinaryPath(dir, "1.0.2"))
	if st, _ := ReadState(dir); st.Previous != "1.0.2" {
		t.Fatalf("%+v", st)
	}
}

// 切换后的新进程不应再次执行 Launch 的切换与清理逻辑。
func TestLaunchSkipsAfterSwitch(t *testing.T) {
	dir := t.TempDir()
	fakeInstalled(t, dir, "1.0.2")
	WriteState(dir, &State{Current: "1.0.2", Pending: true, Attempts: 1})
	t.Setenv(envExeced, "1")
	Launch(dir, "1.0.2")
	if os.Getenv(envExeced) != "" {
		t.Fatal("Launch 应清除切换标记")
	}
	if st, _ := ReadState(dir); st.Current != "1.0.2" || !st.Pending {
		t.Fatalf("切换后的进程不应修改更新状态：%+v", st)
	}
}
