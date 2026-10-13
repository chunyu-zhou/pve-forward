// Package dnsapi 通过各 DNS 服务商的 API 管理解析记录（用于 Web 转发自动解析）。
// 只使用标准库实现签名，不依赖各家 SDK。
package dnsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Record 为一条解析记录。Name 为主机记录（相对主域名，如 "dev-zcy"）。
type Record struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Provider 为一个 DNS 服务商账号。zone 为主域名（如 "example.com"），sub 为主机记录。
type Provider interface {
	// Zones 列出账号下的域名（测试连接用）。
	Zones(ctx context.Context) ([]string, error)
	// Records 返回主机记录为 sub 的全部解析（任意类型）。
	Records(ctx context.Context, zone, sub string) ([]Record, error)
	Create(ctx context.Context, zone, sub, typ, value string) (string, error)
	Update(ctx context.Context, zone, id, sub, typ, value string) error
	Delete(ctx context.Context, zone, id string) error
}

// Field 为服务商参数。
type Field struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Secret bool   `json:"secret,omitempty"`
	Opt    bool   `json:"optional,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

type Type struct {
	Type   string  `json:"type"`
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
}

// Types 为支持的服务商及其参数。
var Types = []Type{
	{"dnspod", "DNSPod", []Field{
		{Key: "id", Label: "ID", Hint: "DNSPod 控制台 → API 密钥 → DNSPod Token 的 ID"},
		{Key: "token", Label: "Token", Secret: true}}},
	{"aliyun", "阿里云 DNS", []Field{
		{Key: "ak", Label: "AccessKey ID"},
		{Key: "sk", Label: "AccessKey Secret", Secret: true, Hint: "建议使用仅有 AliyunDNSFullAccess 权限的 RAM 子账号"}}},
	{"tencent", "腾讯云 DNS", []Field{
		{Key: "ak", Label: "SecretId"},
		{Key: "sk", Label: "SecretKey", Secret: true, Hint: "建议使用仅有 DNSPod 权限的子用户密钥"}}},
	{"huawei", "华为云 DNS", []Field{
		{Key: "ak", Label: "Access Key (AK)"},
		{Key: "sk", Label: "Secret Key (SK)", Secret: true},
		{Key: "region", Label: "区域", Opt: true, Hint: "可不填，默认使用全局接入点；如 cn-south-1"}}},
	{"cloudflare", "Cloudflare", []Field{
		{Key: "token", Label: "API Token", Secret: true, Hint: "需要 Zone.Zone 读取与 Zone.DNS 编辑权限"}}},
}

// TypeOf 返回服务商类型定义。
func TypeOf(t string) (Type, bool) {
	for _, x := range Types {
		if x.Type == t {
			return x, true
		}
	}
	return Type{}, false
}

// Endpoints 可替换服务商的 API 地址（类型 → 地址），用于测试与本地调试：
// 环境变量 PVE_FORWARD_DEV_DNS 形如 "cloudflare=http://127.0.0.1:5091"。
var Endpoints = map[string]string{}

func init() {
	for _, kv := range strings.Split(os.Getenv("PVE_FORWARD_DEV_DNS"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			Endpoints[k] = v
		}
	}
}

// New 根据类型与参数创建服务商。
func New(typ string, p map[string]string) (Provider, error) {
	t, ok := TypeOf(typ)
	if !ok {
		return nil, fmt.Errorf("不支持的 DNS 服务商 %q", typ)
	}
	for _, f := range t.Fields {
		if !f.Opt && strings.TrimSpace(p[f.Key]) == "" {
			return nil, fmt.Errorf("请填写 %s", f.Label)
		}
	}
	hc := &http.Client{Timeout: 20 * time.Second}
	ep := func(def string) string {
		if e := Endpoints[typ]; e != "" {
			return e
		}
		return def
	}
	switch typ {
	case "dnspod":
		return &dnspod{hc: hc, base: ep("https://dnsapi.cn"), token: p["id"] + "," + p["token"]}, nil
	case "aliyun":
		return &aliyun{hc: hc, base: ep("https://alidns.aliyuncs.com"), ak: p["ak"], sk: p["sk"]}, nil
	case "tencent":
		return &tencent{hc: hc, base: ep("https://dnspod.tencentcloudapi.com"), ak: p["ak"], sk: p["sk"]}, nil
	case "huawei":
		base := "https://dns.myhuaweicloud.com"
		if r := strings.TrimSpace(p["region"]); r != "" {
			base = "https://dns." + r + ".myhuaweicloud.com"
		}
		return &huawei{hc: hc, base: ep(base), ak: p["ak"], sk: p["sk"]}, nil
	case "cloudflare":
		return &cloudflare{hc: hc, base: ep("https://api.cloudflare.com/client/v4"), token: p["token"]}, nil
	}
	return nil, fmt.Errorf("不支持的 DNS 服务商 %q", typ)
}

// Mask 返回参数的脱敏展示（用于列表）。
func Mask(typ string, p map[string]string) string {
	m := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) <= 8 {
			return "****"
		}
		return s[:4] + "****" + s[len(s)-4:]
	}
	switch typ {
	case "dnspod":
		return "ID " + strings.TrimSpace(p["id"]) + " · Token " + m(p["token"])
	case "cloudflare":
		return "Token " + m(p["token"])
	default:
		return m(p["ak"])
	}
}

// Find 返回 sub 下与 typ 相同的第一条记录。
func Find(rs []Record, typ string) *Record {
	for i := range rs {
		if strings.EqualFold(rs[i].Type, typ) {
			return &rs[i]
		}
	}
	return nil
}

// ---------------- 公共 HTTP ----------------

var ErrNotFound = errors.New("记录不存在")

// do 发送请求并读取 JSON 响应。非 2xx 时返回包含响应体的错误（由各服务商解析为友好信息）。
func do(hc *http.Client, req *http.Request) ([]byte, int, error) {
	req.Header.Set("User-Agent", "PVE-Forward/1.0")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("连接 DNS 服务商失败: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return b, resp.StatusCode, err
}

func jsonBody(v any) io.Reader {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// trimErr 截断过长的错误信息。
func trimErr(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}
