// Package webcfg 生成 Web 转发（OpenResty）的配置，并处理证书与访问密码。
package webcfg

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CertInfo 为证书的识别结果。
type CertInfo struct {
	Domains  []string  `json:"domains"`
	Issuer   string    `json:"issuer"`
	NotAfter time.Time `json:"notAfter"`
	Days     int       `json:"days"`
}

// ParseCert 解析 PEM 证书（含中间证书）；keyPEM 不为空时同时校验私钥与证书是否匹配。
func ParseCert(certPEM, keyPEM string) (*CertInfo, error) {
	certPEM = strings.TrimSpace(certPEM)
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("不是 PEM 格式的证书（应以 -----BEGIN CERTIFICATE----- 开头）")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("证书无法解析: %w", err)
	}
	if strings.TrimSpace(keyPEM) != "" {
		if _, err := tls.X509KeyPair([]byte(certPEM), []byte(strings.TrimSpace(keyPEM))); err != nil {
			msg := err.Error()
			switch {
			case strings.Contains(msg, "does not match"):
				return nil, errors.New("私钥与证书不匹配")
			case strings.Contains(msg, "failed to find any PEM data in key"), strings.Contains(msg, "failed to parse private key"):
				return nil, errors.New("不是有效的 PEM 私钥")
			}
			return nil, fmt.Errorf("证书或私钥无效: %v", err)
		}
	}
	info := &CertInfo{Domains: leaf.DNSNames, NotAfter: leaf.NotAfter, Days: int(time.Until(leaf.NotAfter).Hours() / 24)}
	if len(info.Domains) == 0 && leaf.Subject.CommonName != "" {
		info.Domains = []string{leaf.Subject.CommonName}
	}
	for i, d := range info.Domains {
		info.Domains[i] = strings.ToLower(d)
	}
	info.Issuer = leaf.Issuer.CommonName
	if info.Issuer == "" && len(leaf.Issuer.Organization) > 0 {
		info.Issuer = leaf.Issuer.Organization[0]
	}
	if len(info.Domains) == 0 {
		return nil, errors.New("证书不包含任何域名")
	}
	return info, nil
}

// Covers 判断证书域名列表是否覆盖 host（通配符只匹配一级）。
func Covers(domains []string, host string) bool {
	host = strings.ToLower(host)
	for _, d := range domains {
		d = strings.ToLower(d)
		if d == host {
			return true
		}
		if rest, ok := strings.CutPrefix(d, "*."); ok {
			if i := strings.IndexByte(host, '.'); i > 0 && host[i+1:] == rest {
				return true
			}
		}
	}
	return false
}

// SSHA 生成 nginx auth_basic_user_file 支持的 {SSHA} 密码摘要。
func SSHA(password string) string {
	salt := make([]byte, 8)
	rand.Read(salt)
	h := sha1.Sum(append([]byte(password), salt...))
	return "{SSHA}" + base64.StdEncoding.EncodeToString(append(h[:], salt...))
}
