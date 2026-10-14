package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// 备注待写入或写入失败时解析本身已生效，不能当作解析失败。
func TestDNSState(t *testing.T) {
	for msg, want := range map[string]string{
		"":               "ok",
		DNSRemarkPending: "remark_pending",
		DNSRemarkFailPrefix + "阿里云 DNS：x":        "remark_failed",
		"a.example.com 已有指向 1.2.3.4 的解析记录，未自动修改": "failed",
	} {
		if got := DNSState(msg); got != want {
			t.Errorf("%q: %s，期望 %s", msg, got, want)
		}
	}
	b, err := json.Marshal(&SiteHost{ID: 1, Host: "a.example.com", RecordID: "secret-id", DNSError: DNSRemarkPending})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"dnsState":"remark_pending"`) || !strings.Contains(s, `"host":"a.example.com"`) || strings.Contains(s, "secret-id") {
		t.Fatal(s)
	}
}
