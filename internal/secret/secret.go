// Package secret 使用 AES-256-GCM 加密保存密码、私钥等敏感数据。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Box struct{ aead cipher.AEAD }

// Open 读取主密钥文件，不存在时生成（权限 600）。
func Open(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥文件 %s 长度不正确", path)
	}
	return New(key)
}

func New(key []byte) (*Box, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Box{g}, nil
}

func (b *Box) Seal(plain string) []byte {
	if plain == "" {
		return nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	rand.Read(nonce)
	return b.aead.Seal(nonce, nonce, []byte(plain), nil)
}

func (b *Box) Open(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	ns := b.aead.NonceSize()
	if len(data) < ns {
		return "", errors.New("密文损坏")
	}
	p, err := b.aead.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return "", errors.New("解密失败（主密钥可能已变更）")
	}
	return string(p), nil
}

// RandomToken 生成随机令牌（十六进制）。
func RandomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Hash 返回令牌的 SHA-256 十六进制摘要，用于数据库中保存令牌。
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ReadableToken 生成 XXXX-XXXX-XXXX 形式的令牌（去掉易混淆字符）。
func ReadableToken() string {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 12)
	rand.Read(b)
	out := make([]byte, 0, 14)
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, alpha[int(c)%len(alpha)])
	}
	return string(out)
}
