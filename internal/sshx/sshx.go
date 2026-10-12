// Package sshx 提供通过 SSH 在 PVE 节点上执行脚本的 Executor。
package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"pveforward/internal/nodeops"
)

type Config struct {
	Host       string
	Port       int
	User       string
	Password   string
	PrivateKey string
	Passphrase string
	HostKey    string // 已信任的主机公钥（authorized_keys 格式）；为空时首次连接返回 UnknownHostKeyError
}

// UnknownHostKeyError 表示需要管理员确认主机指纹。
type UnknownHostKeyError struct {
	Fingerprint string
	Algo        string
	Key         string
}

func (e *UnknownHostKeyError) Error() string {
	return "首次连接，需要确认主机指纹 " + e.Fingerprint
}

var ErrHostKeyMismatch = errors.New("主机指纹与已信任的不一致，可能存在中间人攻击；如确认主机已重装，请在节点配置中重新信任指纹")

func (c Config) addr() string {
	p := c.Port
	if p == 0 {
		p = 22
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(p))
}

func (c Config) clientConfig() (*ssh.ClientConfig, error) {
	var auths []ssh.AuthMethod
	if c.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if c.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(c.PrivateKey), []byte(c.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(c.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("私钥无效: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if c.Password != "" {
		pw := c.Password
		auths = append(auths, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range ans {
				ans[i] = pw
			}
			return ans, nil
		}))
	}
	if len(auths) == 0 {
		return nil, errors.New("未提供密码或私钥")
	}
	return &ssh.ClientConfig{
		User:    c.User,
		Auth:    auths,
		Timeout: 10 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if c.HostKey == "" {
				return &UnknownHostKeyError{Fingerprint: ssh.FingerprintSHA256(key), Algo: key.Type(), Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))}
			}
			want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
			if err != nil || !bytes.Equal(want.Marshal(), key.Marshal()) {
				return ErrHostKeyMismatch
			}
			return nil
		},
	}, nil
}

func Dial(ctx context.Context, c Config) (*ssh.Client, error) {
	cc, err := c.clientConfig()
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.addr())
	if err != nil {
		return nil, fmt.Errorf("无法连接 %s: %w", c.addr(), err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, c.addr(), cc)
	if err != nil {
		conn.Close()
		var uk *UnknownHostKeyError
		if errors.As(err, &uk) {
			return nil, uk
		}
		if errors.Is(err, ErrHostKeyMismatch) {
			return nil, ErrHostKeyMismatch
		}
		return nil, fmt.Errorf("SSH 认证失败: %w", err)
	}
	cl := ssh.NewClient(sc, chans, reqs)
	go keepalive(cl)
	return cl, nil
}

func keepalive(cl *ssh.Client) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		if _, _, err := cl.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			cl.Close()
			return
		}
	}
}

// Executor 复用一条 SSH 连接，断开时自动重连。
type Executor struct {
	cfg Config
	mu  sync.Mutex
	cl  *ssh.Client
}

func NewExecutor(c Config) *Executor { return &Executor{cfg: c} }

func (e *Executor) client(ctx context.Context) (*ssh.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cl != nil {
		return e.cl, nil
	}
	cl, err := Dial(ctx, e.cfg)
	if err != nil {
		return nil, err
	}
	e.cl = cl
	go func() {
		cl.Wait()
		e.mu.Lock()
		if e.cl == cl {
			e.cl = nil
		}
		e.mu.Unlock()
	}()
	return cl, nil
}

func (e *Executor) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cl != nil {
		e.cl.Close()
		e.cl = nil
	}
}

func (e *Executor) shell() string {
	if e.cfg.User == "root" {
		return "bash -s"
	}
	return "sudo -n bash -s"
}

func (e *Executor) Run(ctx context.Context, script string) ([]byte, error) {
	cl, err := e.client(ctx)
	if err != nil {
		return nil, err
	}
	s, err := cl.NewSession()
	if err != nil {
		e.Close()
		return nil, fmt.Errorf("SSH 会话失败: %w", err)
	}
	defer s.Close()
	var out, errb bytes.Buffer
	s.Stdin = strings.NewReader(script)
	s.Stdout, s.Stderr = &out, &errb
	done := make(chan error, 1)
	go func() { done <- s.Run(e.shell()) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		s.Signal(ssh.SIGKILL)
		s.Close()
		return out.Bytes(), ctx.Err()
	}
	if err != nil {
		return out.Bytes(), &nodeops.ExecError{Err: err, Stderr: errb.String()}
	}
	return out.Bytes(), nil
}

// ---------------- 连接测试 ----------------

type TestStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type TestResult struct {
	Steps       []TestStep `json:"steps"`
	NeedTrust   bool       `json:"needTrust"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	KeyAlgo     string     `json:"keyAlgo,omitempty"`
	HostKey     string     `json:"hostKey,omitempty"`
	OK          bool       `json:"ok"`
}

// Test 逐项测试连接：TCP、主机指纹、认证、权限、环境。
func Test(ctx context.Context, c Config) *TestResult {
	r := &TestResult{}
	add := func(name string, ok bool, detail string) { r.Steps = append(r.Steps, TestStep{name, ok, detail}) }

	start := time.Now()
	conn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", c.addr())
	if err != nil {
		add("连接 TCP 端口", false, err.Error())
		return r
	}
	conn.Close()
	add("连接 TCP 端口", true, fmt.Sprintf("已连通，延迟 %d ms", time.Since(start).Milliseconds()))

	cl, err := Dial(ctx, c)
	var uk *UnknownHostKeyError
	switch {
	case errors.As(err, &uk):
		add("校验主机指纹", false, "首次连接，请核对指纹")
		r.NeedTrust, r.Fingerprint, r.KeyAlgo, r.HostKey = true, uk.Fingerprint, uk.Algo, uk.Key
		return r
	case errors.Is(err, ErrHostKeyMismatch):
		add("校验主机指纹", false, err.Error())
		return r
	case err != nil:
		add("校验主机指纹", true, "已信任")
		add("身份认证", false, err.Error())
		return r
	}
	defer cl.Close()
	add("校验主机指纹", true, "已信任")
	add("身份认证", true, "认证成功（用户 "+c.User+"）")

	ex := &Executor{cfg: c, cl: cl}
	out, err := ex.Run(ctx, "id -u")
	if err != nil || strings.TrimSpace(string(out)) != "0" {
		msg := "需要 root 或免密 sudo 权限"
		if err != nil {
			msg += "：" + err.Error()
		}
		add("检查权限", false, msg)
		return r
	}
	add("检查权限", true, "具备 root 权限")

	out, err = ex.Run(ctx, "pveversion 2>/dev/null | head -1; nft --version 2>/dev/null || echo 'nft 未安装（初始化时会自动安装）'")
	if err != nil || !strings.Contains(string(out), "pve-manager") {
		add("检测环境", false, "未检测到 Proxmox VE（pveversion 不可用）")
		return r
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	add("检测环境", true, strings.Join(lines, " · "))
	r.OK = true
	return r
}

// ---------------- 密钥 ----------------

// GenerateKey 生成 ED25519 密钥对，返回 OpenSSH 私钥与 authorized_keys 公钥行。
func GenerateKey(comment string) (priv, pub string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	blk, err := ssh.MarshalPrivateKey(sk, comment)
	if err != nil {
		return "", "", err
	}
	spk, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", "", err
	}
	pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spk))) + " " + comment
	return string(pem.EncodeToMemory(blk)), pub, nil
}

// KeyInfo 校验私钥并返回算法与指纹。
func KeyInfo(priv, passphrase string) (algo, fp string, err error) {
	var s ssh.Signer
	if passphrase != "" {
		s, err = ssh.ParsePrivateKeyWithPassphrase([]byte(priv), []byte(passphrase))
	} else {
		s, err = ssh.ParsePrivateKey([]byte(priv))
	}
	if err != nil {
		var pm *ssh.PassphraseMissingError
		if errors.As(err, &pm) {
			return "", "", errors.New("私钥已加密，请填写私钥口令")
		}
		return "", "", fmt.Errorf("私钥无效: %w", err)
	}
	k := s.PublicKey()
	algo = strings.ToUpper(strings.TrimPrefix(k.Type(), "ssh-"))
	return algo, ssh.FingerprintSHA256(k), nil
}

// PublicKeyOf 返回私钥对应的 authorized_keys 公钥行。
func PublicKeyOf(priv, passphrase string) string {
	var s ssh.Signer
	var err error
	if passphrase != "" {
		s, err = ssh.ParsePrivateKeyWithPassphrase([]byte(priv), []byte(passphrase))
	} else {
		s, err = ssh.ParsePrivateKey([]byte(priv))
	}
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
}

// ValidHostKey 校验 authorized_keys 格式的主机公钥。
func ValidHostKey(s string) bool {
	_, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(s)))
	return err == nil
}
