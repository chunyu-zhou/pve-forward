package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

// fakeCloudflare 模拟 Cloudflare DNS API，供本地调试 Web 转发的自动解析（Token 为 dev-token）。
// 控制端设置环境变量 PVE_FORWARD_DEV_DNS=cloudflare=http://127.0.0.1:<端口> 后使用。
type fakeCloudflare struct {
	mu   sync.Mutex
	next int
	recs map[string][3]string // id → 名称、类型、值
}

var fakeZones = []string{"weincloud.com", "example.com"}

func serveFakeDNS(addr string) {
	f := &fakeCloudflare{recs: map[string][3]string{
		"r1": {"www.weincloud.com", "A", "1.1.1.1"},
		"r2": {"mail.weincloud.com", "MX", "mx.weincloud.com"},
	}}
	log.Printf("模拟 Cloudflare DNS：http://%s（Token dev-token）", addr)
	go func() { log.Println(http.ListenAndServe(addr, f)) }()
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := func(v any) {
		b, _ := json.Marshal(map[string]any{"success": true, "errors": []any{}, "result": v})
		w.Write(b)
	}
	if r.Header.Get("Authorization") != "Bearer dev-token" {
		w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`))
		return
	}
	var in struct{ Type, Name, Content string }
	json.NewDecoder(r.Body).Decode(&in)
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	switch {
	case r.URL.Path == "/zones":
		out := []map[string]string{}
		for _, z := range fakeZones {
			if n := r.URL.Query().Get("name"); n == "" || n == z {
				out = append(out, map[string]string{"id": z, "name": z})
			}
		}
		res(out)
	case r.Method == "GET":
		out := []map[string]string{}
		for id, x := range f.recs {
			if x[0] == r.URL.Query().Get("name") {
				out = append(out, map[string]string{"id": id, "name": x[0], "type": x[1], "content": x[2]})
			}
		}
		res(out)
	case r.Method == "POST":
		f.next++
		id := fmt.Sprintf("dev%d", f.next)
		f.recs[id] = [3]string{in.Name, in.Type, in.Content}
		log.Printf("DNS 添加 %s %s %s", in.Name, in.Type, in.Content)
		res(map[string]string{"id": id})
	case r.Method == "PUT":
		f.recs[id] = [3]string{in.Name, in.Type, in.Content}
		res(map[string]string{"id": id})
	case r.Method == "DELETE":
		log.Printf("DNS 删除 %s", f.recs[id][0])
		delete(f.recs, id)
		res(map[string]string{"id": id})
	}
}
