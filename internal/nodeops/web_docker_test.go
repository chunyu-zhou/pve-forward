//go:build dockertest

// 用 Docker 容器验证 Web 转发的真实脚本：安装 OpenResty、环境检测、下发配置、校验失败回滚、
// 经宿主机 ssh 操作入口虚拟机。运行：go test -tags dockertest -run Docker -v ./internal/nodeops
package nodeops

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"pveforward/internal/proto"
	"pveforward/internal/store"
	"pveforward/internal/webcfg"
)

// dockerExec 在容器内以 root 执行脚本。
type dockerExec struct{ ctr string }

func (d dockerExec) Run(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", d.ctr, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), &ExecError{Err: err, Stderr: errb.String()}
	}
	return out.Bytes(), nil
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func run(t *testing.T, ex Executor, script string) string {
	t.Helper()
	out, err := ex.Run(context.Background(), script)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

func selfCert(t *testing.T, names ...string) (string, string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	kb, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func TestDockerWeb(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	exec.Command("docker", "rm", "-f", "pvef-host", "pvef-vm").Run()
	exec.Command("docker", "network", "rm", "pvef-net").Run()
	sh(t, "network", "create", "pvef-net")
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("PVEF_KEEP") != "" {
			return // 保留容器便于排查
		}
		exec.Command("docker", "rm", "-f", "pvef-host", "pvef-vm").Run()
		exec.Command("docker", "network", "rm", "pvef-net").Run()
	})
	for _, c := range []string{"pvef-host", "pvef-vm"} {
		sh(t, "run", "-d", "--name", c, "--hostname", c, "--network", "pvef-net", "debian:bookworm", "sleep", "infinity")
	}
	host, vmc := dockerExec{"pvef-host"}, dockerExec{"pvef-vm"}
	// 宿主机：ssh 客户端、ss、curl、后端服务；虚拟机：sshd（root 密码登录）与后端服务
	prep := "export DEBIAN_FRONTEND=noninteractive; apt-get update -q >/dev/null && apt-get install -y -q curl iproute2 busybox %s >/dev/null\n" +
		"mkdir -p /srv && echo backend-$(hostname) > /srv/index.html && (busybox httpd -p 9000 -h /srv &)\n"
	run(t, host, fmt.Sprintf(prep, "openssh-client"))
	run(t, vmc, fmt.Sprintf(prep, "openssh-server sudo")+`echo 'root:Vm-Pass-1' | chpasswd
sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
useradd -m -s /bin/bash ops && echo 'ops ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/ops
mkdir -p /run/sshd && /usr/sbin/sshd
`)
	vmIP := sh(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "pvef-vm")
	hostIP := sh(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "pvef-host")

	// ---------------- 宿主机模式 ----------------
	res, err := Web(ctx, host, &proto.WebReq{Op: "check", Target: proto.WebTarget{Mode: "host"}, Ports: []int{80, 443}, Deploy: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("检测（安装前）: %+v", res.Checks)
	for _, c := range res.Checks {
		if !c.OK {
			t.Fatalf("检测失败 %+v", res.Checks)
		}
	}
	res, err = Web(ctx, host, &proto.WebReq{Op: "install", Target: proto.WebTarget{Mode: "host"}})
	if err != nil || !strings.Contains(res.Version, "openresty") {
		t.Fatalf("安装 %+v %v", res, err)
	}
	t.Logf("已安装 %s", res.Version)

	crt, key := selfCert(t, "*.example.com")
	cfg := webcfg.Config{HTTP: []int{80, 8080}, HTTPS: []int{443}, StatusListen: "127.0.0.1", StatusAllow: "127.0.0.1", Version: res.Version,
		Sites: []webcfg.Site{
			{ID: 1, Hosts: []string{"a.example.com"}, Port: 80, Scheme: "http", Target: "127.0.0.1", TargetPort: 9000, Opts: store.SiteOpts{RealIP: true, WS: true, Block: true}},
			{ID: 2, Hosts: []string{"b.example.com"}, Port: 443, HTTPS: true, Scheme: "http", Target: vmIP, TargetPort: 9000, CertID: 7,
				Opts: store.SiteOpts{ForceHTTPS: true, Cache: true, AuthUser: "u1", AuthHash: webcfg.SSHA("pw1"), Headers: "X-From: pvef"}},
		},
		Certs: map[int64]webcfg.Cert{7: {Cert: crt, Key: key}}}
	r := webcfg.Render(cfg)
	if res, err = Web(ctx, host, &proto.WebReq{Op: "apply", Target: proto.WebTarget{Mode: "host"}, Dir: r.Dir, Files: r.Files}); err != nil {
		t.Fatalf("下发 %v", err)
	}
	out := run(t, host, `set -e
curl -sS -H 'Host: a.example.com' http://127.0.0.1/
curl -sS -o /dev/null -w 'unknown=%{http_code}\n' -H 'Host: x.example.com' http://127.0.0.1/ || echo unknown=closed
curl -sS -o /dev/null -w 'attack=%{http_code}\n' -H 'Host: a.example.com' 'http://127.0.0.1/?q=union%20select%201'
curl -sS -o /dev/null -w 'attack2=%{http_code}\n' -H 'Host: a.example.com' 'http://127.0.0.1/?id=1+UNION+ALL+SELECT+2'
curl -sS -o /dev/null -w 'attack3=%{http_code}\n' -H 'Host: a.example.com' 'http://127.0.0.1/download?file=..%2f..%2fetc%2fpasswd'
curl -sS -o /dev/null -w 'normal=%{http_code}\n' -H 'Host: a.example.com' 'http://127.0.0.1/?q=reunion+selection'
curl -sS -o /dev/null -w 'redirect=%{http_code} %{redirect_url}\n' -H 'Host: b.example.com' http://127.0.0.1/p?x=1
curl -sSk -o /dev/null -w 'noauth=%{http_code}\n' --resolve b.example.com:443:127.0.0.1 https://b.example.com/
curl -sSk -u u1:pw1 --resolve b.example.com:443:127.0.0.1 https://b.example.com/
curl -sSk -o /dev/null -w 'h2=%{http_version}\n' -u u1:pw1 --resolve b.example.com:443:127.0.0.1 https://b.example.com/
curl -sS http://127.0.0.1:61080/pvef-stats | sort
`)
	t.Logf("宿主机模式:\n%s", out)
	for _, want := range []string{"backend-pvef-host", "unknown=closed", "attack=403", "attack2=403", "attack3=403", "normal=200", "redirect=301 https://b.example.com/p?x=1", "noauth=401", "backend-pvef-vm", "h2=2", "r1 ", "r2 ", "d1 "} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q", want)
		}
	}

	// 自定义配置错误：校验失败，原配置继续生效
	bad := cfg
	bad.Sites = append([]webcfg.Site(nil), cfg.Sites...)
	bad.Sites[0].Opts.Custom = "this_is_not_a_directive on;"
	rb := webcfg.Render(bad)
	_, err = Web(ctx, host, &proto.WebReq{Op: "apply", Target: proto.WebTarget{Mode: "host"}, Dir: rb.Dir, Files: rb.Files})
	if err == nil || !strings.Contains(err.Error(), "this_is_not_a_directive") {
		t.Fatalf("错误配置应校验失败: %v", err)
	}
	t.Logf("校验失败信息: %v", err)
	if out := run(t, host, "readlink /etc/pve-forward/web; ls /etc/pve-forward; curl -sS -H 'Host: a.example.com' http://127.0.0.1/"); !strings.Contains(out, r.Dir) || strings.Contains(out, rb.Dir) || !strings.Contains(out, "backend-pvef-host") {
		t.Fatalf("回滚后 %s", out)
	}

	// 端口被占用：重载失败时切回原版本
	run(t, host, "(busybox httpd -p 8081 -h /srv &) ; sleep 0.5")
	busy := cfg
	busy.HTTP = []int{80, 8080, 8081}
	busy.Sites = append(append([]webcfg.Site(nil), cfg.Sites...), webcfg.Site{ID: 3, Hosts: []string{"c.example.com"}, Port: 8081, Scheme: "http", Target: "127.0.0.1", TargetPort: 9000})
	rbusy := webcfg.Render(busy)
	_, err = Web(ctx, host, &proto.WebReq{Op: "apply", Target: proto.WebTarget{Mode: "host"}, Dir: rbusy.Dir, Files: rbusy.Files})
	t.Logf("端口占用: %v", err)
	if err == nil || !strings.Contains(err.Error(), "8081") {
		t.Fatalf("端口被占用应失败: %v", err)
	}
	if out := run(t, host, "readlink /etc/pve-forward/web; curl -sS -H 'Host: a.example.com' http://127.0.0.1/"); !strings.Contains(out, r.Dir) || !strings.Contains(out, "backend-pvef-host") {
		t.Fatalf("端口占用回滚后 %s", out)
	}

	// 采集脚本读取统计
	st := &proto.DesiredState{WebStatus: fmt.Sprintf("http://127.0.0.1:%d/pvef-stats", webcfg.StatusPort)}
	stats := parseWebStats(sections([]byte(run(t, host, webStatusScript(st.WebStatus))))["web"])
	if !stats.Up || stats.Stats[1].Requests == 0 || stats.Stats[2].Down == 0 {
		t.Fatalf("统计 %+v", stats)
	}

	// 移除：恢复原配置并停止由本系统安装的 OpenResty
	if _, err := Web(ctx, host, &proto.WebReq{Op: "remove", Target: proto.WebTarget{Mode: "host"}}); err != nil {
		t.Fatal(err)
	}
	if out := run(t, host, "ls /etc/pve-forward; ls /usr/local/openresty/nginx/conf/; curl -sS -m 2 http://127.0.0.1:61080/pvef-stats || echo stopped"); !strings.Contains(out, "stopped") || strings.Contains(out, "web-") || strings.Contains(out, "pvef-orig") {
		t.Fatalf("移除后 %s", out)
	}
	if down := parseWebStats(sections([]byte(run(t, host, webStatusScript(st.WebStatus))))["web"]); down.Up || down.Error == "" {
		t.Fatalf("停止后统计 %+v", down)
	}

	// ---------------- 入口虚拟机模式（宿主机经 ssh 操作） ----------------
	pw := proto.WebTarget{Mode: "vm", VMIP: vmIP, SSHUser: "root", SSHPort: 22, SSHAuth: "password", SSHSecret: "Wrong"}
	res, err = Web(ctx, host, &proto.WebReq{Op: "check", Target: pw, Ports: []int{80, 9000}})
	if err != nil || len(res.Checks) != 1 || res.Checks[0].OK {
		t.Fatalf("错误密码 %+v %v", res, err)
	}
	t.Logf("错误密码: %+v", res.Checks)
	pw.SSHSecret = "Vm-Pass-1"
	res, err = Web(ctx, host, &proto.WebReq{Op: "check", Target: pw, Ports: []int{80, 9000}, Deploy: "auto"})
	t.Logf("检测虚拟机: %+v", res.Checks)
	if err != nil || len(res.Checks) < 5 || !res.Checks[0].OK || res.Checks[len(res.Checks)-2].OK || !strings.Contains(res.Checks[len(res.Checks)-2].Detail, "9000 被 busybox 占用") {
		t.Fatalf("检测虚拟机 %+v %v", res, err)
	}

	// 宿主机专用密钥：生成公钥，加入虚拟机后用 node 方式登录
	if _, err := Web(ctx, host, &proto.WebReq{Op: "check", Target: proto.WebTarget{Mode: "vm", VMIP: vmIP, SSHUser: "root", SSHAuth: "node"}, Ports: []int{80}}); err != nil {
		t.Fatal(err)
	}
	pk, err := Web(ctx, host, &proto.WebReq{Op: "pubkey"})
	if err != nil || !strings.HasPrefix(pk.PublicKey, "ssh-ed25519 ") {
		t.Fatalf("宿主机公钥 %+v %v", pk, err)
	}
	res, _ = Web(ctx, host, &proto.WebReq{Op: "check", Target: proto.WebTarget{Mode: "vm", VMIP: vmIP, SSHUser: "root", SSHAuth: "node"}, Ports: []int{80}})
	if res.Checks[0].OK || !strings.Contains(res.Checks[0].Detail, "authorized_keys") {
		t.Fatalf("未授权时应提示加入公钥 %+v", res.Checks)
	}
	run(t, vmc, "mkdir -p /root/.ssh && echo '"+pk.PublicKey+"' >> /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys")
	res, err = Web(ctx, host, &proto.WebReq{Op: "check", Target: proto.WebTarget{Mode: "vm", VMIP: vmIP, SSHUser: "root", SSHAuth: "node"}, Ports: []int{80}})
	if err != nil || !res.Checks[0].OK {
		t.Fatalf("宿主机密钥登录 %+v %v", res, err)
	}
	if again, _ := Web(ctx, host, &proto.WebReq{Op: "pubkey"}); again.PublicKey != pk.PublicKey {
		t.Fatal("公钥应保持不变")
	}

	// 私钥 + 普通用户 sudo
	priv, pub := mustKey(t)
	run(t, vmc, "mkdir -p /home/ops/.ssh && echo '"+pub+"' > /home/ops/.ssh/authorized_keys && chown -R ops /home/ops/.ssh && chmod 600 /home/ops/.ssh/authorized_keys")
	kt := proto.WebTarget{Mode: "vm", VMIP: vmIP, SSHUser: "ops", SSHAuth: "key", SSHSecret: priv}
	if res, err = Web(ctx, host, &proto.WebReq{Op: "install", Target: kt}); err != nil || !strings.Contains(res.Version, "openresty") {
		t.Fatalf("虚拟机安装 %+v %v", res, err)
	}
	vcfg := webcfg.Config{HTTP: []int{80}, StatusListen: vmIP, StatusAllow: hostIP, Version: res.Version,
		Sites: []webcfg.Site{{ID: 5, Hosts: []string{"v.example.com"}, Port: 80, Scheme: "http", Target: vmIP, TargetPort: 9000, Opts: store.SiteOpts{RealIP: true}}}}
	rv := webcfg.Render(vcfg)
	if _, err := Web(ctx, host, &proto.WebReq{Op: "apply", Target: kt, Dir: rv.Dir, Files: rv.Files}); err != nil {
		t.Fatalf("虚拟机下发 %v", err)
	}
	out = run(t, host, fmt.Sprintf("curl -sS -H 'Host: v.example.com' http://%s/; curl -sS http://%s:%d/pvef-stats", vmIP, vmIP, webcfg.StatusPort))
	t.Logf("虚拟机模式:\n%s", out)
	if !strings.Contains(out, "backend-pvef-vm") || !strings.Contains(out, "r5 1") {
		t.Fatalf("虚拟机模式 %s", out)
	}
	// 统计接口只允许宿主机访问
	if out := run(t, vmc, fmt.Sprintf("curl -sS -o /dev/null -w '%%{http_code}' http://%s:%d/pvef-stats", vmIP, webcfg.StatusPort)); out != "403" {
		t.Fatalf("统计接口应拒绝其他地址: %s", out)
	}
}

// mustKey 生成 ED25519 密钥对（不能引用 sshx：它依赖本包）。
func mustKey(t *testing.T) (string, string) {
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(sk, "pvef-test")
	if err != nil {
		t.Fatal(err)
	}
	spk, _ := ssh.NewPublicKey(pk)
	return string(pem.EncodeToMemory(blk)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spk)))
}
