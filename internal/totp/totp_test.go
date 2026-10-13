package totp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA1 测试向量（取 8 位结果的后 6 位）
func TestRFC6238(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for ts, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		if got := Code(secret, time.Unix(ts, 0)); got != want {
			t.Errorf("%d: %s，期望 %s", ts, got, want)
		}
	}
}

func TestVerify(t *testing.T) {
	s := NewSecret()
	now := time.Unix(1_800_000_000, 0)
	c := Code(s, now)
	step, ok := Verify(s, c[:3]+" "+c[3:], now, 0)
	if !ok {
		t.Fatal("当前验证码应通过")
	}
	if _, ok := Verify(s, c, now, step); ok {
		t.Fatal("同一验证码不能重复使用")
	}
	if _, ok := Verify(s, Code(s, now.Add(-30*time.Second)), now, 0); !ok {
		t.Fatal("应容忍前一个时间步")
	}
	if _, ok := Verify(s, Code(s, now.Add(-90*time.Second)), now, 0); ok {
		t.Fatal("过期的验证码应拒绝")
	}
	if _, ok := Verify(s, "abc", now, 0); ok {
		t.Fatal("格式错误应拒绝")
	}
	u := URI("PVE Forward", "alice", s)
	if !strings.HasPrefix(u, "otpauth://totp/PVE%20Forward:alice?") || !strings.Contains(u, "secret="+s) {
		t.Fatal(u)
	}
	if q, err := QR(u); err != nil || !strings.HasPrefix(q, "data:image/png;base64,") {
		t.Fatal(err)
	}
}
