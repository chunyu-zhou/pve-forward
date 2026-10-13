// Package totp 实现两步验证使用的基于时间的一次性密码（RFC 6238，SHA1、6 位、30 秒），
// 与 Google Authenticator、Microsoft Authenticator、1Password 等应用兼容。
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"
)

const (
	period = 30
	digits = 6
	// skew 为允许的前后时间步数（容忍手机与服务器约 30 秒的时间误差）
	skew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret 生成 160 位随机密钥（Base32，不带填充）。
func NewSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

// URI 返回认证应用扫码使用的 otpauth 地址。
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {fmt.Sprint(digits)}, "period": {fmt.Sprint(period)}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// QR 返回 otpauth 地址的二维码（PNG data URL）。
func QR(uri string) (string, error) {
	c, err := qr.Encode(uri, qr.M)
	if err != nil {
		return "", err
	}
	c.Scale = 6
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(c.PNG()), nil
}

// code 计算某个时间步的验证码。
func code(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	h := m.Sum(nil)
	off := h[len(h)-1] & 0x0f
	v := binary.BigEndian.Uint32(h[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, v%1000000)
}

// Verify 校验验证码，返回匹配的时间步（用于拒绝重复使用）。last 为上次成功使用的时间步，
// 不大于它的时间步不再接受。
func Verify(secret, input string, now time.Time, last int64) (int64, bool) {
	input = strings.ReplaceAll(strings.TrimSpace(input), " ", "")
	if len(input) != digits {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return 0, false
	}
	cur := now.Unix() / period
	for d := int64(-skew); d <= skew; d++ {
		step := cur + d
		if step <= last {
			continue
		}
		if hmac.Equal([]byte(code(key, step)), []byte(input)) {
			return step, true
		}
	}
	return 0, false
}

// Code 返回当前时间的验证码（测试用）。
func Code(secret string, now time.Time) string {
	key, _ := b32.DecodeString(secret)
	return code(key, now.Unix()/period)
}
