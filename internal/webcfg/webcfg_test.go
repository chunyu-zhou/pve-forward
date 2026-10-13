package webcfg

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"pveforward/internal/store"
)

// testCert 生成自签名证书与私钥（PEM）。
func testCert(t *testing.T, days int, names ...string) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, Issuer: pkix.Name{CommonName: "Test CA"},
		DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Duration(days)*24*time.Hour + time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func TestParseCert(t *testing.T) {
	c, k := testCert(t, 89, "*.Example.com", "example.com")
	info, err := ParseCert(c, k)
	if err != nil || info.Days != 89 || len(info.Domains) != 2 || info.Domains[0] != "*.example.com" || info.Issuer == "" {
		t.Fatalf("%+v %v", info, err)
	}
	_, k2 := testCert(t, 10, "other.com")
	if _, err := ParseCert(c, k2); err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("私钥不匹配 %v", err)
	}
	if _, err := ParseCert("hello", ""); err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("非 PEM %v", err)
	}
	if _, err := ParseCert(c, "hello"); err == nil || !strings.Contains(err.Error(), "私钥") {
		t.Fatalf("无效私钥 %v", err)
	}
}

func TestCovers(t *testing.T) {
	d := []string{"*.example.com", "example.com"}
	for host, want := range map[string]bool{"a.example.com": true, "EXAMPLE.com": true, "a.b.example.com": false, "example.org": false, "xexample.com": false} {
		if Covers(d, host) != want {
			t.Errorf("%s 应为 %v", host, want)
		}
	}
}

func TestSSHA(t *testing.T) {
	a, b := SSHA("pw"), SSHA("pw")
	if !strings.HasPrefix(a, "{SSHA}") || a == b {
		t.Fatalf("%s %s", a, b)
	}
}

func baseConfig() Config {
	return Config{HTTP: []int{8080, 80}, HTTPS: []int{443, 8443}, StatusListen: "127.0.0.1", StatusAllow: "127.0.0.1", Version: "openresty/1.27.1.2",
		Sites: []Site{
			{ID: 3, Hosts: []string{"dev.example.com", "zcy.example.com"}, Port: 443, HTTPS: true, Scheme: "http", Target: "10.0.0.5", TargetPort: 8080, CertID: 1,
				Opts: store.SiteOpts{WS: true, RealIP: true, Block: true, Cache: true, ForceHTTPS: true, AuthUser: "u", AuthHash: "{SSHA}x", Timeout: 120, BodyMax: 20,
					Headers: "X-From: pvef\nbad line", Custom: "location /api/ { return 204; }"}},
			{ID: 4, Hosts: []string{"api.example.com"}, Port: 8080, Scheme: "https", Target: "10.0.0.6", TargetPort: 3000, Opts: store.SiteOpts{}},
		},
		Certs: map[int64]Cert{1: {"CERT", "KEY"}, 2: {"UNUSED", "UNUSED"}}}
}

func TestRender(t *testing.T) {
	r := Render(baseConfig())
	if !strings.HasPrefix(r.Dir, "web-") || len(r.Dir) != 16 {
		t.Fatalf("目录 %s", r.Dir)
	}
	main := r.Files["nginx.conf"]
	for _, want := range []string{
		"listen 127.0.0.1:61080;", "allow 127.0.0.1;",
		"listen 80 default_server;\n        listen 8080 default_server;",
		"listen 443 ssl default_server;", "ssl_reject_handshake on;",
		"include /etc/pve-forward/" + r.Dir + "/sites/*.conf;",
		`d:incr("r" .. id, 1, 0)`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("主配置缺少 %q\n%s", want, main)
		}
	}
	s3 := r.Files["sites/3.conf"]
	for _, want := range []string{
		"listen 80;\n    server_name dev.example.com zcy.example.com;\n    set $pvef_site 3;\n    return 301 https://$host$request_uri;", // 跳转 HTTPS
		"listen 443 ssl;\n    http2 on;",
		"ssl_certificate /etc/pve-forward/" + r.Dir + "/certs/1.crt;",
		"auth_basic_user_file /etc/pve-forward/" + r.Dir + "/htpasswd/3;",
		"client_max_body_size 20m;", `if ($request_uri ~* "(\.\./`,
		"proxy_cache pvef_cache;", "proxy_pass http://10.0.0.5:8080;",
		"proxy_set_header Connection $connection_upgrade;", "proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;",
		"proxy_read_timeout 120s;", "proxy_connect_timeout 75s;", `proxy_set_header X-From "pvef";`,
		"    location /api/ { return 204; }", "Strict-Transport-Security",
	} {
		if !strings.Contains(s3, want) {
			t.Errorf("站点 3 缺少 %q\n%s", want, s3)
		}
	}
	if strings.Contains(s3, "bad line") {
		t.Error("格式错误的请求头不应写入")
	}
	s4 := r.Files["sites/4.conf"]
	for _, want := range []string{"listen 8080;", "proxy_pass https://10.0.0.6:3000;", "proxy_ssl_server_name on;", `proxy_set_header Connection "";`, "client_max_body_size 100m;", "proxy_read_timeout 60s;"} {
		if !strings.Contains(s4, want) {
			t.Errorf("站点 4 缺少 %q\n%s", want, s4)
		}
	}
	for _, no := range []string{"ssl", "auth_basic", "X-Real-IP", "proxy_cache", "$request_uri ~*"} {
		if strings.Contains(s4, no) && !(no == "ssl" && strings.Contains(s4, "proxy_ssl")) {
			t.Errorf("站点 4 不应包含 %q", no)
		}
	}
	if r.Files["certs/1.key"] != "KEY\n" || r.Files["htpasswd/3"] != "u:{SSHA}x\n" {
		t.Fatalf("证书 / 密码文件 %q %q", r.Files["certs/1.key"], r.Files["htpasswd/3"])
	}
	if _, ok := r.Files["certs/2.crt"]; ok {
		t.Error("未使用的证书不应下发")
	}

	// 内容不变则摘要不变；任何变化都会改变摘要与目录
	if Render(baseConfig()).Hash != r.Hash {
		t.Fatal("相同配置摘要应相同")
	}
	c := baseConfig()
	c.Sites[1].TargetPort = 3001
	if r2 := Render(c); r2.Hash == r.Hash || r2.Dir == r.Dir {
		t.Fatal("配置变化后摘要应变化")
	}
	// 旧版本使用 listen ... http2
	c = baseConfig()
	c.Version = "openresty/1.21.4.3"
	if s := Render(c).Files["sites/3.conf"]; !strings.Contains(s, "listen 443 ssl http2;") || strings.Contains(s, "http2 on") {
		t.Fatalf("旧版本 HTTP/2 写法\n%s", s)
	}
}

func TestCheckHeaders(t *testing.T) {
	if err := CheckHeaders("X-A: 1\n\nX-B:two words"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"no colon", "X A: 1", `X-A: "q"`, "Host: x"} {
		if CheckHeaders(bad) == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}
