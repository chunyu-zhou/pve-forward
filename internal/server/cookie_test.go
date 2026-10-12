package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// HTTP 与 HTTPS 使用不同的会话 Cookie：HTTP 下忽略旧的 HTTPS 会话 Cookie（浏览器也不会发送它），
// 新会话写入 pf_session_http，不与浏览器中残留的 Secure Cookie 同名。
func TestSessionCookieByScheme(t *testing.T) {
	plain := httptest.NewRequest("GET", "/", nil)
	plain.AddCookie(&http.Cookie{Name: cookieName, Value: "old-https"})
	if sessionCookieName(plain) != cookieNameHTTP || sessionToken(plain) != "" {
		t.Fatalf("HTTP：%s %q", sessionCookieName(plain), sessionToken(plain))
	}
	plain.AddCookie(&http.Cookie{Name: cookieNameHTTP, Value: "new"})
	if sessionToken(plain) != "new" {
		t.Fatal("HTTP 应读取 pf_session_http")
	}
	secure := httptest.NewRequest("GET", "/", nil)
	secure.TLS = &tls.ConnectionState{}
	secure.AddCookie(&http.Cookie{Name: cookieName, Value: "s"})
	if sessionCookieName(secure) != cookieName || sessionToken(secure) != "s" {
		t.Fatal("HTTPS 应使用 pf_session")
	}
	proxied := httptest.NewRequest("GET", "/", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if sessionCookieName(proxied) != cookieName {
		t.Fatal("反向代理的 HTTPS 请求应使用 pf_session")
	}
}
