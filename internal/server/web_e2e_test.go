package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"pveforward/internal/dnsapi"
	"pveforward/internal/manager"
	"pveforward/internal/store"
	"pveforward/internal/webbundle"
)

var (
	sshWrapRe = regexp.MustCompile(`printf '%s' '([A-Za-z0-9+/=]+)' \| base64 -d \| ssh [^\n]* [a-z_][a-z0-9_.-]*@([0-9.]+) `)
	webFileRe = regexp.MustCompile(`printf '%s' '([A-Za-z0-9+/=]*)' \| base64 -d > "\$D\.tmp/([^"]+)"`)
)

// webRun 模拟 Web 转发脚本（检测、安装、下发、移除）。入口虚拟机的脚本经 ssh 包装，解开后按虚拟机处理。
func (f *fakeNode) webRun(script string) ([]byte, bool, error) {
	loc := "host"
	if m := sshWrapRe.FindStringSubmatch(script); m != nil {
		inner, _ := base64.StdEncoding.DecodeString(m[1])
		script, loc = string(inner), "vm:"+m[2]
	}
	const version = "##version\nnginx version: openresty/1.27.1.2\n"
	switch {
	case strings.Contains(script, "##managed"):
		f.webOps = append(f.webOps, "check "+loc)
		return []byte("##uid\n0\n##os\nDebian GNU/Linux 12 (bookworm)|debian|\n##arch\nx86_64\n##apt\nyes\n##systemd\nyes\n##openresty\n##managed\n##listen\n0.0.0.0:22 users:((\"sshd\",pid=1,fd=3))\n##repo\n200"), true, nil
	case strings.Contains(script, "base64 -d") && strings.Contains(script, "stat -c %s /tmp/pve-forward-openresty.tgz"):
		f.webOps = append(f.webOps, "upload "+loc)
		return nil, true, nil
	case strings.Contains(script, "NEW=binary") || strings.Contains(script, "NEW=docker"):
		f.webOps = append(f.webOps, "install "+loc)
		return []byte(version), true, nil
	case strings.Contains(script, `if ! or_test "$D/nginx.conf"`):
		if f.webFail != "" {
			return nil, true, fmt.Errorf("exit status 3: %s", f.webFail)
		}
		files := map[string]string{}
		for _, m := range webFileRe.FindAllStringSubmatch(script, -1) {
			b, _ := base64.StdEncoding.DecodeString(m[1])
			files[m[2]] = string(b)
		}
		if f.web == nil {
			f.web = map[string]map[string]string{}
		}
		f.web[loc] = files
		f.webOps = append(f.webOps, "apply "+loc)
		return []byte(version), true, nil
	case strings.Contains(script, "openresty-installed") && strings.Contains(script, "pvef-orig"):
		delete(f.web, loc)
		f.webOps = append(f.webOps, "remove "+loc)
		return nil, true, nil
	}
	return nil, false, nil
}

// webCollect 为采集脚本补充 OpenResty 统计与站点健康检查结果。
func (f *fakeNode) webCollect(script string) string {
	var b strings.Builder
	for _, m := range regexp.MustCompile(`echo 's(\d+) ok'`).FindAllStringSubmatch(script, -1) {
		b.WriteString("s" + m[1] + " ok\n")
	}
	if strings.Contains(script, "pvef-stats") {
		b.WriteString("##web\n")
		for id, c := range f.webStats {
			fmt.Fprintf(&b, "r%d %d\nu%d %d\nd%d %d\n", id, c[0], id, c[1], id, c[2])
		}
	}
	return b.String()
}

func (f *fakeNode) webConf(loc, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.web[loc][name]
}

func (f *fakeNode) ops() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.webOps, "; ")
}

// fakeCF 模拟 Cloudflare 的解析记录（域名 example.com）。
type fakeCF struct {
	mu   sync.Mutex
	next int
	recs map[string][3]string // id → 名称、类型、值
	note map[string]string    // id → 备注
}

func (c *fakeCF) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		res := func(v any) { b, _ := json.Marshal(map[string]any{"success": true, "result": v}); w.Write(b) }
		if r.Header.Get("Authorization") != "Bearer good" {
			w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`))
			return
		}
		var in struct{ Type, Name, Content, Comment string }
		json.NewDecoder(r.Body).Decode(&in)
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case r.URL.Path == "/zones":
			all := []map[string]string{{"id": "z1", "name": "example.com"}, {"id": "z2", "name": "example.net"}}
			if n := r.URL.Query().Get("name"); n != "" {
				var one []map[string]string
				for _, z := range all {
					if z["name"] == n {
						one = append(one, z)
					}
				}
				all = one
			}
			res(all)
		case r.Method == "GET":
			out := []map[string]string{}
			for id, x := range c.recs {
				if x[0] == r.URL.Query().Get("name") {
					out = append(out, map[string]string{"id": id, "name": x[0], "type": x[1], "content": x[2]})
				}
			}
			res(out)
		case r.Method == "POST":
			c.next++
			id := fmt.Sprint("rec", c.next)
			c.recs[id] = [3]string{in.Name, in.Type, in.Content}
			c.note[id] = in.Comment
			res(map[string]string{"id": id})
		case r.Method == "PUT":
			c.recs[id] = [3]string{in.Name, in.Type, in.Content}
			c.note[id] = in.Comment
			res(map[string]string{"id": id})
		case r.Method == "DELETE":
			delete(c.recs, id)
			res(map[string]string{"id": id})
		}
	})
}

// noteOf 返回域名解析记录的备注。
func (c *fakeCF) noteOf(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, x := range c.recs {
		if x[0] == name {
			return c.note[id]
		}
	}
	return ""
}

func (c *fakeCF) lookup(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.recs {
		if x[0] == name {
			return x[1] + " " + x[2]
		}
	}
	return ""
}

func genCert(t *testing.T, names ...string) (string, string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, Issuer: pkix.Name{CommonName: "Test"}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(20 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	kb, _ := x509.MarshalECPrivateKey(key)
	return pemEncode("CERTIFICATE", der), pemEncode("EC PRIVATE KEY", kb)
}

func pemEncode(typ string, b []byte) string {
	return "-----BEGIN " + typ + "-----\n" + base64.StdEncoding.EncodeToString(b) + "\n-----END " + typ + "-----\n"
}

// testWebForward 覆盖 Web 转发的完整流程：DNS 服务商、主域名、证书、Web 入口部署、站点、自动解析、统计。
func testWebForward(t *testing.T, srv *Server, mgr *manager.Manager, admin, alice, bob *client, fake *fakeNode, nid, vmWeb, vmVPN int64) {
	t.Helper()
	ctx := context.Background()
	cf := &fakeCF{recs: map[string][3]string{"pre": {"www.example.com", "A", "1.1.1.1"}}, note: map[string]string{}}
	cfs := httptest.NewServer(cf.handler())
	defer cfs.Close()
	dnsapi.Endpoints["cloudflare"] = cfs.URL
	defer delete(dnsapi.Endpoints, "cloudflare")

	// ---- DNS 服务商：保存时测试连接，记录状态；密钥不回显 ----
	var saved struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	admin.do("POST", "/api/dns-providers", map[string]any{"type": "cloudflare", "name": "CF", "fields": map[string]string{"token": "bad"}}, 200, &saved)
	if !strings.Contains(saved.Status, "Invalid access token") {
		t.Fatalf("错误 Token 状态 %+v", saved)
	}
	pid := saved.ID
	admin.do("PUT", fmt.Sprintf("/api/dns-providers/%d", pid), map[string]any{"name": "CF", "fields": map[string]string{"token": "good"}}, 200, &saved)
	if saved.Status != "" {
		t.Fatalf("正确 Token 状态 %+v", saved)
	}
	// 编辑时密钥留空沿用原值
	admin.do("PUT", fmt.Sprintf("/api/dns-providers/%d", pid), map[string]any{"name": "Cloudflare 主账号", "fields": map[string]string{"token": ""}}, 200, &saved)
	if saved.Status != "" {
		t.Fatalf("留空密钥 %+v", saved)
	}
	if body := admin.do("GET", "/api/dns-providers", nil, 200, nil); strings.Contains(body, "good") || !strings.Contains(body, "Cloudflare 主账号") {
		t.Fatalf("服务商列表不应包含密钥 %s", body)
	}

	// ---- 主域名：必须在服务商账号下 ----
	if body := admin.do("POST", "/api/web-domains", map[string]any{"name": "other.com", "providerId": pid}, 400, nil); !strings.Contains(body, "账号下没有域名") {
		t.Fatalf("主域名校验 %s", body)
	}
	// 主域名：从服务商获取域名列表，一次添加多个
	var zones []struct {
		Name  string
		Added bool
	}
	admin.do("GET", fmt.Sprintf("/api/dns-providers/%d/zones", pid), nil, 200, &zones)
	if len(zones) != 2 || zones[0].Name != "example.com" || zones[0].Added {
		t.Fatalf("服务商域名 %+v", zones)
	}
	var added struct{ IDs []int64 }
	admin.do("POST", "/api/web-domains", map[string]any{"names": []string{"Example.com.", "example.net", "example.com"}, "providerId": pid}, 200, &added)
	if len(added.IDs) != 2 {
		t.Fatalf("添加多个主域名 %+v", added)
	}
	admin.do("GET", fmt.Sprintf("/api/dns-providers/%d/zones", pid), nil, 200, &zones)
	if !zones[0].Added || !zones[1].Added {
		t.Fatalf("已添加标记 %+v", zones)
	}
	dom := struct{ ID int64 }{added.IDs[0]}
	admin.do("DELETE", fmt.Sprintf("/api/web-domains/%d", added.IDs[1]), nil, 200, nil)
	if body := admin.do("DELETE", fmt.Sprintf("/api/dns-providers/%d", pid), nil, 400, nil); !strings.Contains(body, "正在使用") {
		t.Fatalf("删除使用中的服务商 %s", body)
	}

	// ---- 证书 ----
	crt, key := genCert(t, "*.example.com", "example.com")
	_, otherKey := genCert(t, "x.org")
	if body := admin.do("POST", "/api/certs", map[string]string{"name": "ex", "cert": crt, "key": otherKey}, 400, nil); !strings.Contains(body, "不匹配") {
		t.Fatalf("私钥不匹配 %s", body)
	}
	var cert struct{ ID int64 }
	admin.do("POST", "/api/certs", map[string]string{"name": "example 通配符", "cert": crt, "key": key}, 200, &cert)
	var info struct{ Days int }
	admin.do("POST", "/api/certs/parse", map[string]string{"cert": crt}, 200, &info)
	if info.Days != 19 && info.Days != 20 {
		t.Fatalf("证书天数 %d", info.Days)
	}
	var full struct{ Key string }
	admin.do("GET", fmt.Sprintf("/api/certs/%d", cert.ID), nil, 200, &full)
	if strings.TrimSpace(full.Key) != strings.TrimSpace(key) {
		t.Fatal("管理员查看证书应返回私钥")
	}
	alice.do("GET", fmt.Sprintf("/api/certs/%d", cert.ID), nil, 403, nil)

	// 所有二进制下载均走本地 HTTP，不访问真实 Release 服务。
	bundle := []byte("fake openresty bundle")
	bundleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
			fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(bundle), webbundle.Name("amd64"))
			return
		}
		w.Write(bundle)
	}))
	defer bundleServer.Close()
	oldBase := webbundle.Base
	webbundle.Base = bundleServer.URL
	defer func() { webbundle.Base = oldBase }()

	// ---- Web 入口：端口不能与端口转发规则冲突；部署到宿主机 ----
	var me struct{ User store.User }
	admin.do("GET", "/api/me", nil, 200, &me)
	waf := map[string]any{"userId": me.User.ID, "nodeId": nid, "port": 80, "proto": "tcp", "ipVer": "v4", "vmId": vmVPN, "targetPort": 80, "remark": "waf"}
	admin.do("POST", "/api/rules", waf, 200, nil)
	webCfg := map[string]any{"enabled": true, "mode": "host", "deploy": "auto", "http": []int{80, 8080}, "https": []int{443}, "default": 80}
	if body := admin.do("POST", fmt.Sprintf("/api/nodes/%d/web", nid), webCfg, 400, nil); !strings.Contains(body, "正被端口转发规则「waf」使用") {
		t.Fatalf("端口冲突 %s", body)
	}
	var check struct{ Checks []map[string]any }
	admin.do("POST", fmt.Sprintf("/api/nodes/%d/web/check", nid), webCfg, 200, &check)
	if len(check.Checks) < 4 {
		t.Fatalf("检测结果 %+v", check)
	}
	var rules []ruleOut
	admin.do("GET", "/api/rules", nil, 200, &rules)
	// 确认接管：部署成功后删除 Web 端口上的端口转发规则
	webCfg["takeOver"] = true
	stream := admin.do("POST", fmt.Sprintf("/api/nodes/%d/web", nid), webCfg, 200, nil)
	delete(webCfg, "takeOver")
	if !strings.Contains(stream, "接管端口转发规则 80「waf」") {
		t.Fatalf("接管 %s", stream)
	}
	admin.do("GET", "/api/rules", nil, 200, &rules)
	for _, r := range rules {
		if r.Port == 80 {
			t.Fatal("接管后应删除 80 端口的规则")
		}
	}
	if !strings.Contains(stream, `"done":true`) || strings.Contains(stream, `"error"`) || !strings.Contains(stream, "安装 OpenResty") {
		t.Fatalf("部署 %s", stream)
	}
	if ops := fake.ops(); ops != "check host; check host; upload host; install host; apply host" {
		t.Fatalf("宿主机操作 %s", ops)
	}
	if conf := fake.webConf("host", "nginx.conf"); !strings.Contains(conf, "listen 8080 default_server;") || !strings.Contains(conf, "listen 127.0.0.1:61080;") {
		t.Fatalf("主配置 %s", conf)
	}
	// 部署枚举、镜像和加速配置必须由后端校验。
	for key, value := range map[string]string{"deploy": "bogus", "image": "x:y;id", "downloadMirror": "http://untrusted/"} {
		old := webCfg[key]
		webCfg[key] = value
		admin.do("POST", fmt.Sprintf("/api/nodes/%d/web/check", nid), webCfg, 400, nil)
		if old == nil { delete(webCfg, key) } else { webCfg[key] = old }
	}
	// 切换 Docker：带显式镜像，不下载/上传二进制包。
	webCfg["deploy"], webCfg["image"] = "docker", "registry.example.com:5000/openresty:test"
	before := fake.ops()
	stream = admin.do("POST", fmt.Sprintf("/api/nodes/%d/web", nid), webCfg, 200, nil)
	if !strings.Contains(stream, `"done":true`) || strings.Contains(stream, `"error"`) { t.Fatalf("Docker 部署 %s", stream) }
	if added := strings.TrimPrefix(fake.ops(), before); added != "; check host; install host; apply host" { t.Fatalf("Docker 操作 %s", added) }
	webCfg["deploy"] = "binary"
	delete(webCfg, "image")

	// Web 端口不能再用于端口转发，也不能分配给用户
	if body := admin.do("POST", "/api/rules", waf, 400, nil); !strings.Contains(body, "Web 转发端口") {
		t.Fatalf("Web 端口用于规则 %s", body)
	}

	// ---- 站点：域名检查、证书、自动解析 ----
	var opts struct {
		Nodes   []map[string]any `json:"nodes"`
		Domains []map[string]any `json:"domains"`
		Certs   []map[string]any `json:"certs"`
		VMs     []map[string]any `json:"vms"`
	}
	alice.do("GET", fmt.Sprintf("/api/site-options?node=%d", nid), nil, 200, &opts)
	if len(opts.Nodes) != 1 || len(opts.Domains) != 1 || len(opts.Certs) != 1 || len(opts.VMs) != 2 || opts.Nodes[0]["deployed"] != true {
		t.Fatalf("站点选项 %+v", opts)
	}
	checkHost := func(sub string) (bool, string) {
		var r struct {
			OK  bool
			Msg string
		}
		alice.do("POST", "/api/sites/check-host", map[string]any{"sub": sub, "domainId": dom.ID}, 200, &r)
		return r.OK, r.Msg
	}
	if ok, msg := checkHost("www"); ok || msg != "www.example.com 已被使用" {
		t.Fatalf("已解析的域名 %v %s", ok, msg)
	}
	if ok, msg := checkHost("dev"); !ok || msg != "dev.example.com 可以使用" {
		t.Fatalf("可用域名 %v %s", ok, msg)
	}
	if ok, _ := checkHost("a_b"); ok {
		t.Fatal("非法前缀应不可用")
	}
	site := map[string]any{"nodeId": nid, "port": 443, "vmId": vmWeb, "targetPort": 8080, "hosts": []map[string]any{{"sub": "dev", "domainId": dom.ID}},
		"opts": map[string]any{"ws": true, "realIp": true, "authUser": "u", "authPassword": "pw12", "custom": "return 418;"}}
	if body := alice.do("POST", "/api/sites", site, 400, nil); !strings.Contains(body, "需要选择证书") {
		t.Fatalf("HTTPS 缺少证书 %s", body)
	}
	site["certId"] = cert.ID
	site["hosts"] = []map[string]any{{"sub": "www", "domainId": dom.ID}}
	if body := alice.do("POST", "/api/sites", site, 400, nil); !strings.Contains(body, "已被使用") {
		t.Fatalf("已使用的域名 %s", body)
	}
	site["hosts"] = []map[string]any{{"sub": "dev", "domainId": dom.ID}}
	// 备注必填：写入解析记录的备注
	if body := alice.do("POST", "/api/sites", site, 400, nil); !strings.Contains(body, "请填写备注") {
		t.Fatalf("缺少备注 %s", body)
	}
	site["remark"] = "前端开发"
	var created struct {
		ID  int64
		DNS []dnsResult
	}
	alice.do("POST", "/api/sites", site, 200, &created)
	if len(created.DNS) != 1 || created.DNS[0].Error != "" || cf.lookup("dev.example.com") != "A 203.0.113.10" {
		t.Fatalf("自动解析 %+v %q", created, cf.lookup("dev.example.com"))
	}
	if n := cf.noteOf("dev.example.com"); n != "前端开发(alice)" {
		t.Fatalf("解析备注 %q", n)
	}
	sid := created.ID
	if err := mgr.RunOnce(ctx, nid); err != nil {
		t.Fatal(err)
	}
	conf := fake.webConf("host", fmt.Sprintf("sites/%d.conf", sid))
	for _, want := range []string{"listen 443 ssl;", "server_name dev.example.com;", "proxy_pass http://10.0.0.57:8080;", "auth_basic_user_file", "proxy_set_header Upgrade"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("站点配置缺少 %q\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "return 418") {
		t.Fatal("普通用户不能设置自定义配置")
	}
	if fake.webConf("host", fmt.Sprintf("certs/%d.key", cert.ID)) == "" || fake.webConf("host", fmt.Sprintf("htpasswd/%d", sid)) == "" {
		t.Fatal("证书私钥与访问密码文件应下发")
	}
	// bob 看不到、改不了 alice 的站点
	var bobSites []siteOut
	bob.do("GET", "/api/sites", nil, 200, &bobSites)
	if len(bobSites) != 0 {
		t.Fatal("bob 不应看到 alice 的站点")
	}
	bob.do("DELETE", fmt.Sprintf("/api/sites/%d", sid), nil, 404, nil)

	// ---- 统计：请求数与流量计入站点与用户 ----
	before, _ := srv.db.UserByName("alice")
	fake.mu.Lock()
	fake.webStats = map[int64][3]uint64{sid: {10, 1000, 50000}}
	fake.mu.Unlock()
	mgr.RunOnce(ctx, nid)
	fake.mu.Lock()
	fake.webStats = map[int64][3]uint64{sid: {15, 1500, 80000}}
	fake.mu.Unlock()
	mgr.RunOnce(ctx, nid)
	var sites []siteOut
	alice.do("GET", "/api/sites", nil, 200, &sites)
	after, _ := srv.db.UserByName("alice")
	if len(sites) != 1 || sites[0].Requests != 15 || sites[0].Req24 != 15 || sites[0].Down24 != 80000 || after.UsedBytes-before.UsedBytes != 81500 {
		t.Fatalf("站点统计 %+v 用户流量 +%d", sites, after.UsedBytes-before.UsedBytes)
	}
	if sites[0].Error != "" || !sites[0].HTTPS || sites[0].CertName != "example 通配符" || !sites[0].Auth || sites[0].Opts.AuthHash != "" {
		t.Fatalf("站点信息 %+v", sites[0])
	}

	// ---- 修改域名：新域名添加解析，旧域名删除解析；访问密码留空沿用 ----
	site["hosts"] = []map[string]any{{"sub": "dev2", "domainId": dom.ID}}
	site["opts"] = map[string]any{"authUser": "u"}
	alice.do("PUT", fmt.Sprintf("/api/sites/%d", sid), site, 200, nil)
	if cf.lookup("dev.example.com") != "" || cf.lookup("dev2.example.com") != "A 203.0.113.10" {
		t.Fatalf("修改域名后解析 dev=%q dev2=%q", cf.lookup("dev.example.com"), cf.lookup("dev2.example.com"))
	}
	mgr.RunOnce(ctx, nid)
	if conf := fake.webConf("host", fmt.Sprintf("sites/%d.conf", sid)); !strings.Contains(conf, "server_name dev2.example.com;") || !strings.Contains(conf, "auth_basic") {
		t.Fatalf("修改后配置 %s", conf)
	}
	// 停用：配置中移除
	alice.do("PUT", fmt.Sprintf("/api/sites/%d", sid), map[string]any{"enabled": false}, 200, nil)
	mgr.RunOnce(ctx, nid)
	if fake.webConf("host", fmt.Sprintf("sites/%d.conf", sid)) != "" {
		t.Fatal("停用的站点不应下发")
	}
	alice.do("PUT", fmt.Sprintf("/api/sites/%d", sid), map[string]any{"enabled": true}, 200, nil)

	// 下发失败：记录错误，站点显示异常；恢复后自动重试
	fake.mu.Lock()
	fake.webFail = `[emerg] unknown directive "x"`
	fake.mu.Unlock()
	mgr.RunOnce(ctx, nid)
	alice.do("GET", "/api/sites", nil, 200, &sites)
	if !strings.Contains(sites[0].Error, "Web 配置下发失败") {
		t.Fatalf("下发失败提示 %q", sites[0].Error)
	}
	fake.mu.Lock()
	fake.webFail = ""
	fake.mu.Unlock()
	if err := mgr.ApplyWeb(ctx, nid); err != nil {
		t.Fatal(err)
	}
	alice.do("GET", "/api/sites", nil, 200, &sites)
	if sites[0].Error != "" || fake.webConf("host", fmt.Sprintf("sites/%d.conf", sid)) == "" {
		t.Fatalf("恢复后 %q", sites[0].Error)
	}

	// ---- 改为入口虚拟机运行：宿主机转发 Web 端口到虚拟机，原位置停止 ----
	vmCfg := map[string]any{"enabled": true, "mode": "vm", "vmId": vmVPN, "deploy": "manual", "sshUser": "root", "sshAuth": "password", "http": []int{80, 8080}, "https": []int{443}, "default": 80}
	if body := admin.do("POST", fmt.Sprintf("/api/nodes/%d/web", nid), vmCfg, 400, nil); !strings.Contains(body, "SSH 密码") {
		t.Fatalf("缺少 SSH 密码 %s", body)
	}
	vmCfg["sshSecret"] = "vm-pass"
	stream = admin.do("POST", fmt.Sprintf("/api/nodes/%d/web", nid), vmCfg, 200, nil)
	if !strings.Contains(stream, `"done":true`) || strings.Contains(stream, `"error"`) || strings.Contains(stream, "安装 OpenResty") {
		t.Fatalf("迁移到虚拟机 %s", stream)
	}
	if conf := fake.webConf("vm:10.0.0.8", "nginx.conf"); !strings.Contains(conf, "listen 10.0.0.8:61080;") || !strings.Contains(conf, "allow 10.0.0.1;") {
		t.Fatalf("虚拟机主配置 %s", conf)
	}
	if fake.webConf("host", "nginx.conf") != "" || !strings.Contains(fake.ops(), "remove host") {
		t.Fatalf("原位置应停止 %s", fake.ops())
	}
	// 入口虚拟机模式不改动宿主机端口：不生成转发，Web 端口也可以用于端口转发
	if strings.Contains(fake.nft(), "dnat ip to 10.0.0.8:80") {
		t.Fatalf("入口虚拟机模式不应生成宿主机转发\n%s", fake.nft())
	}
	admin.do("POST", "/api/rules", map[string]any{"userId": me.User.ID, "nodeId": nid, "port": 80, "proto": "tcp", "ipVer": "v4", "vmId": vmVPN, "targetPort": 80, "remark": "入口"}, 200, nil)
	var entries []webEntryOut
	admin.do("GET", "/api/web-entries", nil, 200, &entries)
	if len(entries) != 1 || entries[0].State.Location != fmt.Sprintf("vm:%d", vmVPN) || entries[0].Web.SSHSecretEnc != nil || entries[0].State.Hash != "" {
		t.Fatalf("Web 入口 %+v", entries[0])
	}

	// ---- 删除站点：删除解析记录 ----
	alice.do("DELETE", fmt.Sprintf("/api/sites/%d", sid), nil, 200, nil)
	if cf.lookup("dev2.example.com") != "" || cf.lookup("www.example.com") == "" {
		t.Fatal("删除站点应只删除本系统添加的解析")
	}
	mgr.RunOnce(ctx, nid)
	if fake.webConf("vm:10.0.0.8", fmt.Sprintf("sites/%d.conf", sid)) != "" {
		t.Fatal("删除的站点不应下发")
	}
	if body := admin.do("DELETE", fmt.Sprintf("/api/certs/%d", cert.ID), nil, 200, nil); !strings.Contains(body, "ok") {
		t.Fatal(body)
	}

	// 停用 Web 转发：移除虚拟机上的配置；管理员自己添加的端口转发规则保留
	stream = admin.do("POST", fmt.Sprintf("/api/nodes/%d/web", nid), map[string]any{"enabled": false, "http": []int{80, 8080}, "https": []int{443}, "default": 80}, 200, nil)
	if !strings.Contains(stream, `"done":true`) || !strings.Contains(fake.ops(), "remove vm:10.0.0.8") || !strings.Contains(fake.nft(), "dnat ip to 10.0.0.8:80") {
		t.Fatalf("停用 %s\n%s", stream, fake.ops())
	}
	t.Logf("Web 转发流程完成：%s", fake.ops())
}

// testMonitorWS 实时监控：登录后通过 WebSocket 订阅，每次采集完成推送；未登录、跨站请求被拒绝。
func testMonitorWS(t *testing.T, base string, mgr *manager.Manager, admin *client, nid int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(base, "http") + "/api/monitor/ws?every=2"
	if _, _, err := websocket.Dial(ctx, u, nil); err == nil {
		t.Fatal("未登录应拒绝")
	}
	hdr := http.Header{}
	bu, _ := url.Parse(base)
	for _, c := range admin.hc.Jar.Cookies(bu) {
		hdr.Add("Cookie", c.Name+"="+c.Value)
	}
	hdr.Set("Origin", "https://evil.example")
	if _, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr}); err == nil {
		t.Fatal("跨站请求应拒绝")
	}
	hdr.Set("Origin", base)
	ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(4 << 20)
	var first, second struct {
		Nodes []monNode `json:"nodes"`
	}
	if err := wsjson.Read(ctx, ws, &first); err != nil || len(first.Nodes) != 1 {
		t.Fatalf("首次推送 %+v %v", first, err)
	}
	go mgr.RunOnce(context.Background(), nid)
	if err := wsjson.Read(ctx, ws, &second); err != nil || len(second.Nodes) != 1 {
		t.Fatalf("采集后推送 %+v %v", second, err)
	}
}
