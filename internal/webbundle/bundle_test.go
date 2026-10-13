package webbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFetch(t *testing.T) {
	data := []byte("bundle-content")
	h := sha256.Sum256(data)
	sum := hex.EncodeToString(h[:])
	var downloads, mirrored atomic.Int32
	tamper := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if p, ok := strings.CutPrefix(path, "/mirror/"); ok { // 加速前缀：https://mirror/<原地址>
			mirrored.Add(1)
			path = "/" + p[strings.Index(p, "/rel/"):][1:]
		}
		switch path {
		case "/rel/SHA256SUMS":
			w.Write([]byte(sum + "  " + Name("amd64") + "\n"))
		case "/rel/" + Name("amd64"):
			downloads.Add(1)
			if tamper {
				w.Write([]byte("evil"))
				return
			}
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	old := Base
	Base = ts.URL + "/rel"
	defer func() { Base = old }()
	dir := t.TempDir()

	p, err := Fetch(context.Background(), dir, "amd64", "")
	if b, _ := os.ReadFile(p); err != nil || string(b) != "bundle-content" {
		t.Fatalf("%v %q", err, b)
	}
	// 已缓存且校验一致：不再下载
	if _, err := Fetch(context.Background(), dir, "amd64", ""); err != nil || downloads.Load() != 1 {
		t.Fatalf("缓存 %v %d", err, downloads.Load())
	}
	// 经加速前缀下载；内容被篡改时拒绝
	os.Remove(p)
	tamper = true
	if _, err := Fetch(context.Background(), dir, "amd64", ts.URL+"/mirror/"); err == nil || !strings.Contains(err.Error(), "校验失败") || mirrored.Load() == 0 {
		t.Fatalf("篡改应失败 %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("校验失败不应保留文件")
	}
	if _, err := Arch("riscv64"); err == nil {
		t.Fatal("不支持的架构应报错")
	}
}
