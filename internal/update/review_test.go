package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 新版本未确认前被正常停止（重启容器、重启主机）不应被当成启动失败。
func TestReleaseKeepsPendingVersion(t *testing.T) {
	dir := t.TempDir()
	p := fakeInstalled(t, dir, "1.0.2")
	Activate(dir, "1.0.2", "1.0.0", "/usr/local/bin/pve-forward")
	if err := Release(dir, "1.0.2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := plan(dir, "1.0.0"); got != p {
		t.Fatalf("正常停止后应再次尝试新版本，实际 %q", got)
	}
	// 之后再次未确认就退出（崩溃）才回退
	if got, _ := plan(dir, "1.0.0"); got != "" {
		t.Fatalf("崩溃后应回退，实际 %q", got)
	}
	// 其他版本的进程调用 Release 不影响状态
	fakeInstalled(t, dir, "1.0.3")
	WriteState(dir, &State{Current: "1.0.3", Pending: true, Attempts: 1})
	Release(dir, "1.0.0")
	if st, _ := ReadState(dir); st.Attempts != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestMarkFailed(t *testing.T) {
	dir := t.TempDir()
	fakeInstalled(t, dir, "1.0.2")
	fakeInstalled(t, dir, "1.0.3")
	WriteState(dir, &State{Current: "1.0.3", Previous: "1.0.2", Pending: true, Attempts: 1})
	if err := MarkFailed(dir, "1.0.3"); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadState(dir)
	if st.Current != "1.0.2" || st.Failed != "1.0.3" || st.Pending {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(BinaryPath(dir, "1.0.3")); err == nil {
		t.Fatal("失败的版本应被清理")
	}
}

// 数据目录为相对路径时，也能识别当前运行的是在线更新版本。
func TestActivateRelativeDataDir(t *testing.T) {
	base := t.TempDir()
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(base)
	running, _ := filepath.Abs(BinaryPath("data", "1.0.2"))
	if err := Activate("data", "1.0.3", "1.0.2", running); err != nil {
		t.Fatal(err)
	}
	if st, _ := ReadState("data"); st.Previous != "1.0.2" {
		t.Fatalf("%+v", st)
	}
}

// 配置加速前缀时校验文件优先直连 GitHub，加速服务无法同时伪造程序与校验和。
func TestChecksumsPreferDirect(t *testing.T) {
	good := strings.Repeat("a", 64)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  pve-forward-linux-amd64\n", good)
	}))
	defer gh.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  pve-forward-linux-amd64\n", strings.Repeat("b", 64))
	}))
	defer mirror.Close()
	src := Source{ReleaseBase: gh.URL + "/rel", Mirror: mirror.URL + "/"}
	sums, err := src.Checksums(context.Background(), "1.0.1")
	if err != nil || sums["pve-forward-linux-amd64"] != good {
		t.Fatalf("应使用直连的校验文件：%v %v", sums, err)
	}
	// 直连不可用时才经由加速前缀
	gh.Close()
	sums, err = src.Checksums(context.Background(), "1.0.1")
	if err != nil || sums["pve-forward-linux-amd64"] != strings.Repeat("b", 64) {
		t.Fatalf("直连失败应回退到加速前缀：%v %v", sums, err)
	}
}
