package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// startServer 启动一个只支持 exec 请求的测试 SSH 服务端（用本机 bash 执行命令）。
func startServer(t *testing.T, password string, authorized ssh.PublicKey) (port int, hostKey ssh.PublicKey) {
	t.Helper()
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(sk)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if password != "" && string(p) == password {
				return nil, nil
			}
			return nil, errors.New("bad password")
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && string(k.Marshal()) == string(authorized.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("bad key")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, _ := nc.Accept()
					go func() {
						for r := range creqs {
							if r.Type != "exec" {
								r.Reply(false, nil)
								continue
							}
							cmdline := string(r.Payload[4:])
							r.Reply(true, nil)
							cmd := exec.Command("bash", "-c", cmdline)
							cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
							code := 0
							if err := cmd.Run(); err != nil {
								code = 1
							}
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{uint32(code)}))
							ch.Close()
							return
						}
					}()
				}
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, signer.PublicKey()
}

func TestTOFUAndRun(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("需要 bash")
	}
	port, hk := startServer(t, "secret", nil)
	ctx := context.Background()
	cfg := Config{Host: "127.0.0.1", Port: port, User: "root", Password: "secret"}

	// 首次连接：要求确认指纹
	_, err := Dial(ctx, cfg)
	var uk *UnknownHostKeyError
	if !errors.As(err, &uk) || uk.Fingerprint != ssh.FingerprintSHA256(hk) {
		t.Fatalf("应返回待确认指纹，得到 %v", err)
	}
	r := Test(ctx, cfg)
	if !r.NeedTrust || r.Fingerprint != uk.Fingerprint || len(r.Steps) != 2 || !r.Steps[0].OK {
		t.Fatalf("Test 首次结果 %+v", r)
	}

	// 信任后可执行脚本（stdin 传入）
	cfg.HostKey = r.HostKey
	ex := NewExecutor(cfg)
	defer ex.Close()
	out, err := ex.Run(ctx, "echo hello\necho $((1+2))\n")
	if err != nil || strings.TrimSpace(string(out)) != "hello\n3" {
		t.Fatalf("Run: %q %v", out, err)
	}
	// 非零退出码带出 stderr
	_, err = ex.Run(ctx, "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("应返回 stderr，得到 %v", err)
	}

	// 密码错误
	bad := cfg
	bad.Password = "wrong"
	if _, err := Dial(ctx, bad); err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("应认证失败，得到 %v", err)
	}
	rr := Test(ctx, bad)
	if rr.OK || rr.Steps[len(rr.Steps)-1].Name != "身份认证" || rr.Steps[len(rr.Steps)-1].OK {
		t.Fatalf("Test 错误密码 %+v", rr)
	}

	// 主机指纹变化
	_, other := startServer(t, "secret", nil)
	mm := cfg
	mm.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(other)))
	if _, err := Dial(ctx, mm); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("应检测到指纹不一致，得到 %v", err)
	}
}

func TestKeyAuthAndGenerate(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("需要 bash")
	}
	priv, pub, err := GenerateKey("pve-forward")
	if err != nil {
		t.Fatal(err)
	}
	algo, fp, err := KeyInfo(priv, "")
	if err != nil || algo != "ED25519" || !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("KeyInfo %s %s %v", algo, fp, err)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.HasSuffix(pub, " pve-forward") || !strings.HasPrefix(pub, PublicKeyOf(priv, "")) {
		t.Fatalf("公钥 %q", pub)
	}
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(pub))
	port, hk := startServer(t, "", pk)
	cfg := Config{Host: "127.0.0.1", Port: port, User: "root", PrivateKey: priv,
		HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hk)))}
	out, err := NewExecutor(cfg).Run(context.Background(), "echo "+strconv.Itoa(42))
	if err != nil || strings.TrimSpace(string(out)) != "42" {
		t.Fatalf("密钥认证执行 %q %v", out, err)
	}
	if _, _, err := KeyInfo("not a key", ""); err == nil {
		t.Fatal("无效私钥应报错")
	}
}

// PlainKey：无口令的私钥原样返回；有口令时解密后重新编码为无口令私钥。
func TestPlainKey(t *testing.T) {
	priv, _, err := GenerateKey("t")
	if err != nil {
		t.Fatal(err)
	}
	if k, err := PlainKey(priv, ""); err != nil || !strings.Contains(k, "OPENSSH PRIVATE KEY") {
		t.Fatalf("%v", err)
	}
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	blk, _ := ssh.MarshalPrivateKeyWithPassphrase(sk, "t", []byte("pass"))
	enc := string(pem.EncodeToMemory(blk))
	if _, err := PlainKey(enc, ""); err == nil || !strings.Contains(err.Error(), "口令") {
		t.Fatalf("缺少口令 %v", err)
	}
	k, err := PlainKey(enc, "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssh.ParsePrivateKey([]byte(k)); err != nil {
		t.Fatalf("解密后应为无口令私钥: %v", err)
	}
	if _, err := PlainKey("garbage", ""); err == nil {
		t.Fatal("无效私钥应报错")
	}
}
