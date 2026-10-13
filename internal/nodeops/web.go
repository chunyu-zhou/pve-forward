package nodeops

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"pveforward/internal/proto"
)

// Web 转发：在宿主机或入口虚拟机上检测环境、安装 OpenResty、下发配置。
// 入口虚拟机由宿主机通过 ssh 连接（虚拟机的 SSH 不需要对外开放），对 Agent 与 SSH 两种节点都适用。

const (
	webKnownHosts = Dir + "/web_known_hosts"
	// 由本系统安装 OpenResty 的标记，移除时停止服务（不能以 web- 开头：会被当作旧版本配置目录清理）
	webMarker = Dir + "/openresty-installed"
	webLogDir = "/var/log/pve-forward-web"
	// webNodeKey 为宿主机连接入口虚拟机的专用密钥（首次使用时生成），公钥需加入虚拟机的 authorized_keys
	webNodeKey = Dir + "/web_ed25519"
)

var (
	webDirRe  = regexp.MustCompile(`^web-[0-9a-f]{12}$`)
	webFileRe = regexp.MustCompile(`^(nginx\.conf|sites/\d+\.conf|certs/\d+\.(crt|key)|htpasswd/\d+)$`)
	sshUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
	// 统计接口地址：只允许 http://IPv4:端口/pvef-stats
	webStatusRe = regexp.MustCompile(`^http://[0-9.]+:\d+/pvef-stats$`)
)

// ensureNodeKey 生成宿主机的 Web 转发专用密钥（已存在则不变）。
const ensureNodeKey = `[ -f ` + webNodeKey + ` ] || { mkdir -p ` + Dir + ` && ssh-keygen -q -t ed25519 -N '' -C "pve-forward-web@$(hostname -s)" -f ` + webNodeKey + `; }
`

// Web 执行一次 Web 转发操作。
func Web(ctx context.Context, ex Executor, req *proto.WebReq) (*proto.WebResult, error) {
	if req.Op == "pubkey" { // 在宿主机上执行
		out, err := ex.Run(ctx, ensureNodeKey+"cat "+webNodeKey+".pub\n")
		if err != nil {
			return nil, fmt.Errorf("生成宿主机密钥失败: %w", err)
		}
		return &proto.WebResult{PublicKey: strings.TrimSpace(string(out))}, nil
	}
	tex, err := webExecutor(ex, req.Target)
	if err != nil {
		return nil, err
	}
	switch req.Op {
	case "check":
		return webCheck(ctx, tex, req)
	case "upload":
		return &proto.WebResult{}, webUpload(ctx, tex, req)
	case "install":
		return webInstall(ctx, tex, req)
	case "apply":
		return webApply(ctx, tex, req)
	case "remove":
		return &proto.WebResult{}, webRemove(ctx, tex)
	}
	return nil, fmt.Errorf("未知操作 %q", req.Op)
}

func webExecutor(ex Executor, t proto.WebTarget) (Executor, error) {
	if t.Mode != "vm" {
		return ex, nil
	}
	ip, err := netip.ParseAddr(t.VMIP)
	if err != nil || !ip.Is4() {
		return nil, fmt.Errorf("入口虚拟机地址无效: %q", t.VMIP)
	}
	if !sshUserRe.MatchString(t.SSHUser) {
		return nil, fmt.Errorf("SSH 用户名无效: %q", t.SSHUser)
	}
	if t.SSHPort < 0 || t.SSHPort > 65535 {
		return nil, fmt.Errorf("SSH 端口无效")
	}
	if (t.SSHAuth == "password" || t.SSHAuth == "key") && t.SSHSecret == "" {
		return nil, fmt.Errorf("没有入口虚拟机的 SSH 密码或私钥")
	}
	return &vmExecutor{ex: ex, t: t}, nil
}

// vmExecutor 在宿主机上用 ssh 把脚本交给入口虚拟机执行。
type vmExecutor struct {
	ex Executor
	t  proto.WebTarget
}

func (v *vmExecutor) Run(ctx context.Context, script string) ([]byte, error) {
	out, err := v.ex.Run(ctx, v.wrap(script))
	if err != nil && strings.Contains(err.Error(), "exit status 255") {
		msg := err.Error()
		if i := strings.Index(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
		if strings.Contains(msg, "Permission denied") && v.t.SSHAuth != "password" && v.t.SSHAuth != "key" {
			msg += "（请把宿主机的 Web 转发公钥加入虚拟机 " + v.t.SSHUser + " 用户的 ~/.ssh/authorized_keys）"
		}
		return out, fmt.Errorf("SSH 连接入口虚拟机 %s 失败: %s", v.t.VMIP, msg)
	}
	return out, err
}

func (v *vmExecutor) wrap(script string) string {
	t := v.t
	port := t.SSHPort
	if port == 0 {
		port = 22
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	var b strings.Builder
	b.WriteString("K=$(mktemp -d)\ntrap 'rm -rf \"$K\"' EXIT\n")
	opts := fmt.Sprintf("-p %d -o ConnectTimeout=10 -o ServerAliveInterval=15 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s -o LogLevel=ERROR", port, webKnownHosts)
	switch t.SSHAuth {
	case "key":
		fmt.Fprintf(&b, "printf '%%s' '%s' | base64 -d > \"$K/id\"\nchmod 600 \"$K/id\"\n", b64(strings.TrimSpace(t.SSHSecret)+"\n"))
		opts += ` -i "$K/id" -o IdentitiesOnly=yes -o BatchMode=yes`
	case "password":
		// 通过 SSH_ASKPASS 提供密码（OpenSSH 8.4 起支持 SSH_ASKPASS_REQUIRE）
		fmt.Fprintf(&b, "printf '%%s' '%s' | base64 -d > \"$K/pw\"\nchmod 600 \"$K/pw\"\n", b64(t.SSHSecret))
		b.WriteString("printf '#!/bin/sh\\ncat \"%s\"\\n' \"$K/pw\" > \"$K/ask\"\nchmod 700 \"$K/ask\"\n")
		b.WriteString("export SSH_ASKPASS=\"$K/ask\" SSH_ASKPASS_REQUIRE=force DISPLAY=none\n")
		opts += " -o PreferredAuthentications=password,keyboard-interactive -o PubkeyAuthentication=no -o NumberOfPasswordPrompts=1"
	default: // 宿主机的 Web 转发专用密钥
		b.WriteString(ensureNodeKey)
		opts += " -i " + webNodeKey + " -o IdentitiesOnly=yes -o BatchMode=yes"
	}
	remote := "bash -s"
	if t.SSHUser != "root" {
		remote = "sudo -n bash -s"
	}
	fmt.Fprintf(&b, "mkdir -p %s\nprintf '%%s' '%s' | base64 -d | ssh %s %s@%s %s\n", Dir, b64(script), opts, t.SSHUser, t.VMIP, shq(remote))
	return b.String()
}

// OpenResty 的三种运行方式（记录在 webRuntimeFile）：
//   - binary：控制端上传的二进制包，安装在 BinPrefix，由 pve-forward-openresty 服务运行
//   - docker：openresty 容器（host 网络），挂载配置目录
//   - system：已有的 OpenResty（自行安装或旧版本通过 apt 安装），主配置 include 本系统的配置
const (
	BinPrefix      = "/usr/local/pve-forward-openresty"
	webRuntimeFile = Dir + "/openresty-runtime"
	webImageFile   = Dir + "/openresty-image"
	webUnit        = "pve-forward-openresty"
	webContainer   = "pve-forward-openresty"
	webBundleTmp   = "/tmp/pve-forward-openresty.tgz"
)

// webLocate 定义当前运行方式的 OpenResty 操作（RT、or_cmd、or_ok、or_test、or_reload、or_running）。
const webLocate = `RT=$(cat ` + webRuntimeFile + ` 2>/dev/null || true)
MAIN=` + Dir + `/web/nginx.conf
DV="-v ` + Dir + `:` + Dir + `:ro -v /var/log/pve-forward-web:/var/log/pve-forward-web -v /var/cache/pve-forward-web:/var/cache/pve-forward-web"
IMG=$(cat ` + webImageFile + ` 2>/dev/null || true)
BIN=$(command -v openresty 2>/dev/null || true)
[ -n "$BIN" ] || BIN=/usr/local/openresty/bin/openresty
if [ -z "$RT" ]; then
  if [ -x ` + BinPrefix + `/bin/openresty ]; then RT=binary; elif [ -x "$BIN" ]; then RT=system; fi
fi
[ "$RT" = binary ] && BIN=` + BinPrefix + `/bin/openresty
CONF=""
[ "$RT" = system ] && CONF=$("$BIN" -V 2>&1 | sed -n 's/.*--conf-path=\([^ ]*\).*/\1/p')
[ -n "$CONF" ] || CONF=/usr/local/openresty/nginx/conf/nginx.conf
has_systemd() { command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; }
or_cmd() { if [ "$RT" = docker ]; then docker exec ` + webContainer + ` openresty "$@"; else "$BIN" "$@"; fi; }
or_ok() {
  case "$RT" in
    docker) command -v docker >/dev/null 2>&1 && [ -n "$IMG" ] && docker image inspect "$IMG" >/dev/null 2>&1 ;;
    binary|system) [ -x "$BIN" ] ;;
    *) return 1 ;;
  esac
}
or_version() { if [ "$RT" = docker ]; then docker run --rm "$IMG" openresty -v 2>&1; else "$BIN" -v 2>&1; fi; }
or_test() { if [ "$RT" = docker ]; then docker run --rm --network none $DV "$IMG" openresty -t -q -c "$1"; else "$BIN" -t -q -c "$1"; fi; }
or_running() {
  case "$RT" in
    docker) [ "$(docker inspect -f '{{.State.Running}}' ` + webContainer + ` 2>/dev/null)" = true ] ;;
    binary) if has_systemd; then systemctl is-active -q ` + webUnit + `; else [ -s ` + BinPrefix + `/nginx/logs/nginx.pid ] && kill -0 "$(cat ` + BinPrefix + `/nginx/logs/nginx.pid)" 2>/dev/null; fi ;;
    *) if has_systemd && systemctl cat openresty >/dev/null 2>&1; then systemctl is-active -q openresty; else return 0; fi ;;
  esac
}
or_reload() {
  case "$RT" in
    docker)
      if or_running; then docker exec ` + webContainer + ` openresty -c "$MAIN" -s reload
      elif docker inspect ` + webContainer + ` >/dev/null 2>&1; then docker start ` + webContainer + ` >/dev/null
      else docker run -d --name ` + webContainer + ` --restart unless-stopped --network host $DV "$IMG" openresty -g 'daemon off;' -c "$MAIN" >/dev/null; fi ;;
    binary)
      if has_systemd; then systemctl enable -q ` + webUnit + ` 2>/dev/null || true; if or_running; then systemctl reload ` + webUnit + `; else systemctl restart ` + webUnit + `; fi
      elif or_running; then "$BIN" -c "$MAIN" -s reload; else "$BIN" -c "$MAIN"; fi ;;
    *)
      if has_systemd && systemctl cat openresty >/dev/null 2>&1; then
        systemctl enable -q openresty 2>/dev/null || true
        if systemctl is-active -q openresty; then systemctl reload openresty; else systemctl restart openresty; fi
      else "$BIN" -s reload 2>/dev/null || "$BIN"; fi ;;
  esac
}
`

func webCheck(ctx context.Context, ex Executor, req *proto.WebReq) (*proto.WebResult, error) {
	res := &proto.WebResult{}
	add := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, proto.WebCheck{Name: name, OK: ok, Detail: detail})
	}
	out, err := ex.Run(ctx, "export LC_ALL=C\n"+webLocate+`echo '##uid'; id -u
echo '##os'; ( . /etc/os-release 2>/dev/null; echo "$PRETTY_NAME|$ID|$ID_LIKE" )
echo '##arch'; uname -m
echo '##apt'; command -v apt-get >/dev/null 2>&1 && echo yes
echo '##systemd'; command -v systemctl >/dev/null 2>&1 && echo yes
echo '##openresty'; or_ok && or_version
echo '##runtime'; or_ok && echo "$RT"
echo '##docker'; command -v docker >/dev/null 2>&1 && docker version --format '{{.Server.Version}}' 2>/dev/null
echo '##managed'; [ -L `+Dir+`/web ] && echo yes
echo '##listen'; ss -Hltnp 2>/dev/null | awk '{print $4, $6}'
`)
	if req.Target.Mode == "vm" {
		if err != nil {
			name := "SSH 连接"
			if strings.Contains(err.Error(), "sudo") {
				name = "root 权限"
			}
			add(name, false, err.Error())
			return res, nil
		}
		port := req.Target.SSHPort
		if port == 0 {
			port = 22
		}
		add("SSH 连接", true, fmt.Sprintf("%s@%s:%d", req.Target.SSHUser, req.Target.VMIP, port))
	} else if err != nil {
		return nil, err
	}
	s := sections(out)
	uid := strings.TrimSpace(s["uid"])
	add("root 权限", uid == "0", map[bool]string{true: "具备 root 权限", false: "需要 root 或免密 sudo"}[uid == "0"])

	osf := strings.SplitN(strings.TrimSpace(s["os"]), "|", 3)
	pretty, arch := osf[0], strings.TrimSpace(s["arch"])
	apt := strings.TrimSpace(s["apt"]) == "yes"
	version := strings.TrimSpace(s["openresty"])
	if !strings.Contains(version, "openresty") {
		version = ""
	}
	res.Version = version
	res.Arch, res.Runtime = arch, strings.TrimSpace(s["runtime"])
	docker := strings.TrimSpace(s["docker"])
	sysOK, sysDetail := true, strings.TrimSpace(pretty+" · "+arch)
	switch {
	case req.Deploy == "binary" && arch != "x86_64" && arch != "aarch64":
		sysOK, sysDetail = false, sysDetail+"（二进制安装只支持 x86_64 与 aarch64）"
	case req.Deploy == "docker" && docker == "" && !apt:
		sysOK, sysDetail = false, sysDetail+"（未安装 Docker，自动安装 Docker 仅支持 Debian / Ubuntu）"
	}
	add("系统支持", sysOK, sysDetail)
	if strings.TrimSpace(s["systemd"]) != "yes" {
		// 不影响部署（直接启动 OpenResty），只是不会开机自启
		add("systemd", true, "未检测到 systemd：OpenResty 将直接启动，系统重启后需要手动启动")
	}

	// 端口：被其他进程占用则不可用；已由本系统部署的 OpenResty 占用的不算
	managed := strings.TrimSpace(s["managed"]) == "yes"
	want := map[int]bool{}
	for _, p := range req.Ports {
		want[p] = true
	}
	busy := map[int]string{}
	for _, l := range strings.Split(s["listen"], "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		i := strings.LastIndex(f[0], ":")
		p, err := strconv.Atoi(f[0][i+1:])
		if err != nil || !want[p] {
			continue
		}
		proc := "其他进程"
		if len(f) > 1 {
			if m := regexp.MustCompile(`\(\("([^"]+)"`).FindStringSubmatch(f[1]); m != nil {
				proc = m[1]
			}
		}
		if managed && (proc == "nginx" || proc == "openresty" || proc == "docker-proxy") {
			continue
		}
		busy[p] = proc
	}
	ports := append([]int(nil), req.Ports...)
	sort.Ints(ports)
	var names []string
	for _, p := range ports {
		names = append(names, strconv.Itoa(p))
	}
	if len(busy) == 0 {
		add("端口 "+strings.Join(names, " / ")+" 未被占用", true, "")
	} else {
		var d []string
		for _, p := range ports {
			if busy[p] != "" {
				d = append(d, fmt.Sprintf("%d 被 %s 占用", p, busy[p]))
			}
		}
		add("端口 "+strings.Join(names, " / ")+" 未被占用", false, strings.Join(d, "，"))
	}

	ver := strings.TrimPrefix(version, "nginx version: ")
	rtName := map[string]string{"binary": "二进制", "docker": "Docker", "system": "系统安装"}
	switch req.Deploy {
	case "manual":
		if version != "" {
			add("OpenResty", true, "已安装 "+ver+"（"+rtName[res.Runtime]+"）")
		} else {
			add("OpenResty", false, "未安装 OpenResty，请先安装或选择二进制 / Docker 安装")
		}
	case "docker":
		switch {
		case docker != "":
			add("Docker", true, "已安装 Docker "+docker)
		case apt:
			add("Docker", true, "未安装，将通过 apt 安装 Docker（docker.io）")
		default:
			add("Docker", false, "未安装 Docker")
		}
		if res.Runtime == "docker" {
			add("OpenResty", true, "已安装 "+ver+"（Docker）")
		} else {
			add("OpenResty", true, "将拉取 OpenResty 镜像运行")
		}
	default:
		if res.Runtime == "binary" {
			add("OpenResty", true, "已安装 "+ver+"（二进制，部署时更新到当前版本）")
		} else {
			add("OpenResty", true, "将由控制端上传二进制版 OpenResty（目标机器不需要访问外网）")
		}
	}
	return res, nil
}

// webUpload 把二进制包的一段写入目标机器的临时文件。
func webUpload(ctx context.Context, ex Executor, req *proto.WebReq) error {
	op := ">>"
	if req.Offset == 0 {
		op = ">"
	}
	_, err := ex.Run(ctx, fmt.Sprintf("set -e\nprintf '%%s' '%s' | base64 -d %s %s\n[ \"$(stat -c %%s %s)\" = %d ]\n",
		base64.StdEncoding.EncodeToString(req.Data), op, webBundleTmp, webBundleTmp, req.Offset+int64(len(req.Data))))
	if err != nil {
		return fmt.Errorf("上传 OpenResty 失败: %w", err)
	}
	return nil
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

const DefaultWebImage = "openresty/openresty:1.31.1.1-0-bookworm-fat"

var imageRe = regexp.MustCompile(`^(?:[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]{1,5})?/)?[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[0-9a-f]{64})$`)

// ValidWebImage 要求显式版本或摘要，禁止 shell 元字符与不确定的隐式 latest。
func ValidWebImage(image string) bool { return len(image) <= 512 && imageRe.MatchString(image) }

// webInstallGuard 保存旧运行方式；安装失败时恢复文件并重新启动旧配置。
const webInstallGuard = webLocate + `
OLD_RT=$RT; OLD_IMG=$IMG; OLD_BIN=$BIN
SWAPPED=0; STOPPED=0; SWITCHED=0; HAD_BINARY=0
[ ! -d ` + BinPrefix + ` ] || HAD_BINARY=1
stop_current() {
  case "$RT" in
    docker) docker stop -t 10 ` + webContainer + ` >/dev/null ;;
    binary) if has_systemd; then systemctl stop ` + webUnit + `; else
      PID=$(cat ` + BinPrefix + `/nginx/logs/nginx.pid 2>/dev/null || true)
      if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
        "$BIN" -c "$MAIN" -s quit
        for i in $(seq 1 50); do kill -0 "$PID" 2>/dev/null || break; sleep 0.2; done
        if kill -0 "$PID" 2>/dev/null; then echo '旧 OpenResty 尚未退出' >&2; return 1; fi
      fi
    fi ;;
    system) if has_systemd && systemctl cat openresty >/dev/null 2>&1; then systemctl stop openresty; else "$BIN" -s quit; fi ;;
  esac
}
rollback_install() {
  code=$?
  trap - EXIT
  if [ "$code" != 0 ]; then
    if [ "$NEW" = binary ] && [ "$SWITCHED" = 1 ]; then stop_current || true; fi
    if [ "$NEW" = docker ] && [ "$SWITCHED" = 1 ]; then docker rm -f ` + webContainer + ` >/dev/null 2>&1 || true; fi
    if [ "$SWAPPED" = 1 ]; then
      rm -rf ` + BinPrefix + `
      [ "$HAD_BINARY" = 0 ] || mv ` + BinPrefix + `.old ` + BinPrefix + `
    fi
    if [ "$OLD_RT" = docker ] && docker inspect ` + webContainer + `-previous >/dev/null 2>&1; then docker rename ` + webContainer + `-previous ` + webContainer + ` || true; fi
    printf '%s\n' "$OLD_RT" > ` + webRuntimeFile + `
    printf '%s\n' "$OLD_IMG" > ` + webImageFile + `
    RT=$OLD_RT; IMG=$OLD_IMG; BIN=$OLD_BIN
    [ "$RT" != binary ] || BIN=` + BinPrefix + `/bin/openresty
    if [ "$STOPPED" = 1 ] && [ -f "$MAIN" ]; then or_reload || true; fi
  fi
  exit "$code"
}
mkdir -p ` + Dir + `
trap rollback_install EXIT
`

// 切换前保留旧容器，失败可恢复；没有 systemd 时也停止旧二进制。
const webStopOthers = `if [ -f "$MAIN" ] && or_ok; then
  STOPPED=1
  stop_current
  if [ "$OLD_RT" = docker ]; then docker rename ` + webContainer + ` ` + webContainer + `-previous; fi
fi
`

const webInstallFinish = `
if [ -f "$MAIN" ]; then
  or_test "$MAIN"
  or_reload
  sleep 1
  or_running
fi
trap - EXIT
if has_systemd; then
  if [ "$OLD_RT" = binary ] && [ "$NEW" != binary ]; then systemctl disable -q ` + webUnit + ` 2>/dev/null || true; fi
  if [ "$OLD_RT" = system ] && [ "$NEW" != system ]; then systemctl disable -q openresty 2>/dev/null || true; fi
fi
rm -rf ` + BinPrefix + `.old
if command -v docker >/dev/null 2>&1; then docker rm -f ` + webContainer + `-previous >/dev/null 2>&1 || true; fi
`

const webUnitFile = `[Unit]
Description=PVE Forward OpenResty（Web 转发）
After=network-online.target
Wants=network-online.target

[Service]
Type=forking
PIDFile=` + BinPrefix + `/nginx/logs/nginx.pid
ExecStartPre=` + BinPrefix + `/bin/openresty -t -q -c ` + Dir + `/web/nginx.conf
ExecStart=` + BinPrefix + `/bin/openresty -c ` + Dir + `/web/nginx.conf
ExecReload=` + BinPrefix + `/bin/openresty -c ` + Dir + `/web/nginx.conf -s reload
ExecStop=-` + BinPrefix + `/bin/openresty -c ` + Dir + `/web/nginx.conf -s quit
TimeoutStopSec=10
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`

func webInstall(ctx context.Context, ex Executor, req *proto.WebReq) (*proto.WebResult, error) {
	var script string
	switch req.Deploy {
	case "binary":
		if !sha256Re.MatchString(req.SHA256) {
			return nil, fmt.Errorf("二进制包摘要无效")
		}
		script = `set -e
NEW=binary
` + webInstallGuard + `echo '` + req.SHA256 + `  ` + webBundleTmp + `' | sha256sum -c - >/dev/null 2>&1 || { rm -f ` + webBundleTmp + `; echo "OpenResty 二进制包校验失败" >&2; exit 2; }
rm -rf ` + BinPrefix + `.new && mkdir -p ` + BinPrefix + `.new
tar xzf ` + webBundleTmp + ` -C ` + BinPrefix + `.new
rm -f ` + webBundleTmp + `
[ -x ` + BinPrefix + `.new/pve-forward-openresty/bin/openresty ]
` + webStopOthers + `# 必须重启 master 才会使用新二进制；不能仅 reload 后删除旧包。
rm -rf ` + BinPrefix + `.old
if [ -d ` + BinPrefix + ` ]; then mv ` + BinPrefix + ` ` + BinPrefix + `.old; fi
SWAPPED=1
mv ` + BinPrefix + `.new/pve-forward-openresty ` + BinPrefix + `
rm -rf ` + BinPrefix + `.new
` + BinPrefix + `/bin/openresty -v 2>&1 | grep -q openresty || { echo "OpenResty 无法在该系统上运行" >&2; exit 3; }
mkdir -p ` + Dir + `
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
` + writeFileCmd("/etc/systemd/system/"+webUnit+".service", []byte(webUnitFile), "644") + `  systemctl daemon-reload
fi
echo binary > ` + webRuntimeFile + `
`
	case "docker":
		if !ValidWebImage(req.Image) {
			return nil, fmt.Errorf("镜像地址无效: %q", req.Image)
		}
		script = `set -e
export DEBIAN_FRONTEND=noninteractive LC_ALL=C
NEW=docker
` + webInstallGuard + `if ! command -v docker >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || { echo "未安装 Docker：自动安装 Docker 仅支持 Debian / Ubuntu，请先手动安装 Docker" >&2; exit 2; }
  apt-get install -y -q docker.io >/dev/null 2>&1 || { apt-get update -q >/dev/null 2>&1; apt-get install -y -q docker.io >&2; }
fi
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then systemctl enable -q --now docker 2>/dev/null || true; fi
docker pull -q ` + shq(req.Image) + ` >/dev/null
mkdir -p ` + Dir + `
` + webStopOthers + `echo ` + shq(req.Image) + ` > ` + webImageFile + `
echo docker > ` + webRuntimeFile + `
`
	default:
		return nil, fmt.Errorf("不支持的安装方式 %q", req.Deploy)
	}
	out, err := ex.Run(ctx, script+"SWITCHED=1\n"+webLocate+webInstallFinish+"echo '##version'; or_version\n")
	if err != nil {
		return nil, fmt.Errorf("安装 OpenResty 失败: %w", err)
	}
	return &proto.WebResult{Version: strings.TrimSpace(sections(out)["version"])}, nil
}

// webApply 写入新版本目录 → openresty -t 校验 → 切换链接并重载；失败时保留原配置。
func webApply(ctx context.Context, ex Executor, req *proto.WebReq) (*proto.WebResult, error) {
	if !webDirRe.MatchString(req.Dir) {
		return nil, fmt.Errorf("配置目录名无效: %q", req.Dir)
	}
	if _, ok := req.Files["nginx.conf"]; !ok {
		return nil, fmt.Errorf("缺少 nginx.conf")
	}
	D := Dir + "/" + req.Dir
	var b strings.Builder
	b.WriteString("set -e\nexport LC_ALL=C\n" + webLocate)
	b.WriteString(`or_ok || { echo "未安装 OpenResty" >&2; exit 4; }` + "\n")
	fmt.Fprintf(&b, "D=%s\nrm -rf \"$D.tmp\"\nmkdir -p \"$D.tmp/sites\" \"$D.tmp/certs\" \"$D.tmp/htpasswd\" %s %s\nchmod 700 \"$D.tmp/certs\"\n", shq(D), webLogDir, "/var/cache/pve-forward-web")
	names := make([]string, 0, len(req.Files))
	for name := range req.Files {
		if !webFileRe.MatchString(name) {
			return nil, fmt.Errorf("配置文件名无效: %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mode := "644"
		if strings.HasSuffix(name, ".key") {
			mode = "600"
		}
		fmt.Fprintf(&b, "printf '%%s' '%s' | base64 -d > \"$D.tmp/%s\"\nchmod %s \"$D.tmp/%s\"\n", base64.StdEncoding.EncodeToString([]byte(req.Files[name])), name, mode, name)
	}
	b.WriteString(`rm -rf "$D"; mv "$D.tmp" "$D"
if ! or_test "$D/nginx.conf" 2>"$D.err"; then
  grep -v '^nginx: configuration file .* test failed' "$D.err" | sed 's/^nginx: //' | tail -5 >&2
  rm -rf "$D" "$D.err"; exit 3
fi
rm -f "$D.err"
# 系统安装的 OpenResty：主配置只 include 当前版本（首次接管时备份原配置）；二进制与 Docker 直接使用本系统的主配置
if [ "$RT" = system ]; then
  if [ -f "$CONF" ] && ! grep -q 'PVE Forward' "$CONF" && [ ! -f "$CONF.pvef-orig" ]; then cp -a "$CONF" "$CONF.pvef-orig"; fi
  printf '# 由 PVE Forward 管理，原配置备份为 nginx.conf.pvef-orig\ninclude ` + Dir + `/web/nginx.conf;\n' > "$CONF.tmp"
  mv -f "$CONF.tmp" "$CONF"
fi
P=$(readlink ` + Dir + `/web 2>/dev/null || true)
ln -sfn "$(basename "$D")" ` + Dir + `/web.lnk && mv -Tf ` + Dir + `/web.lnk ` + Dir + `/web
L1=` + webLogDir + `/error.log; L2="$(dirname "$CONF")/../logs/error.log"
S1=$(stat -c %s "$L1" 2>/dev/null || echo 0); S2=$(stat -c %s "$L2" 2>/dev/null || echo 0)
RELOAD=1; or_reload || RELOAD=0
sleep 1
# 重载时监听端口失败等错误只记录在日志中：检查新增的 emerg 日志，失败则切回原版本
E=$( (tail -c +$((S1+1)) "$L1" 2>/dev/null; tail -c +$((S2+1)) "$L2" 2>/dev/null) | grep '\[emerg\]' | tail -3 || true)
RUN=1; or_running || RUN=0
if [ -n "$E" ] || [ "$RUN" = 0 ] || [ "$RELOAD" = 0 ]; then
  [ -n "$E" ] && echo "$E" | sed 's/^.*\[emerg\] [0-9#]*: //' | awk '!seen[$0]++' >&2
  if [ "$RUN" = 0 ]; then
    if [ "$RT" = docker ]; then docker logs --tail 5 ` + webContainer + ` >&2 2>&1 || true
    elif [ "$RT" = binary ]; then journalctl -u ` + webUnit + ` -n 5 --no-pager -o cat >&2 2>/dev/null || true
    else journalctl -u openresty -n 5 --no-pager -o cat >&2 2>/dev/null || true; fi
  fi
  if [ -n "$P" ] && [ -d "` + Dir + `/$P" ]; then
    ln -sfn "$P" ` + Dir + `/web.lnk && mv -Tf ` + Dir + `/web.lnk ` + Dir + `/web
    or_reload 2>/dev/null || true
  fi
  rm -rf "$D"; exit 5
fi
for x in ` + Dir + `/web-*; do [ "$x" = "$D" ] || rm -rf "$x"; done
echo '##version'; or_version
`)
	out, err := ex.Run(ctx, b.String())
	if err != nil {
		return nil, fmt.Errorf("下发 Web 配置失败: %w", err)
	}
	return &proto.WebResult{Version: strings.TrimSpace(sections(out)["version"])}, nil
}

// webRemove 停用本系统的 Web 配置：恢复 OpenResty 原配置；若由本系统安装则停止服务。
func webRemove(ctx context.Context, ex Executor) error {
	_, err := ex.Run(ctx, "export LC_ALL=C\n"+webLocate+`case "$RT" in
  docker) docker rm -f `+webContainer+` >/dev/null 2>&1 || true ;;
  binary)
    if has_systemd; then systemctl disable -q --now `+webUnit+` 2>/dev/null || true
    else "$BIN" -c "$MAIN" -s quit 2>/dev/null || true; fi
    rm -f /etc/systemd/system/`+webUnit+`.service; has_systemd && systemctl daemon-reload || true
    rm -rf `+BinPrefix+` ;;
  *)
    if [ -f "$CONF.pvef-orig" ]; then mv -f "$CONF.pvef-orig" "$CONF"; fi
    if [ -f `+webMarker+` ]; then
      PIDF=$("$BIN" -V 2>&1 | sed -n 's/.*--pid-path=\([^ ]*\).*/\1/p'); [ -n "$PIDF" ] || PIDF=/usr/local/openresty/nginx/logs/nginx.pid
      PID=$(cat "$PIDF" 2>/dev/null || true)
      systemctl disable -q --now openresty 2>/dev/null || "$BIN" -s stop 2>/dev/null || true
      # 停止是异步的：等待主进程退出（最多 10 秒）
      if [ -n "$PID" ]; then for i in $(seq 1 50); do kill -0 "$PID" 2>/dev/null || break; sleep 0.2; done; fi
      rm -f `+webMarker+`
    elif [ -x "$BIN" ]; then
      systemctl reload openresty 2>/dev/null || "$BIN" -s reload 2>/dev/null || true
    fi ;;
esac
rm -rf `+Dir+`/web `+Dir+`/web.lnk `+Dir+`/web-* `+webRuntimeFile+` `+webImageFile+`
`)
	return err
}

// webStatusScript 读取 OpenResty 统计接口（采集时执行）。
func webStatusScript(url string) string {
	if !webStatusRe.MatchString(url) {
		return ""
	}
	return "echo '##web'; curl -sS -m 3 " + shq(url) + " 2>&1 || echo '#down'\n"
}

// parseWebStats 解析统计接口输出（每行 "r<站点ID> 数值"，r / u / d 分别为请求数、上行、下行字节）。
func parseWebStats(text string) *proto.WebReport {
	rep := &proto.WebReport{Stats: map[int64]proto.WebCounter{}}
	text = strings.TrimSpace(text)
	if strings.Contains(text, "#down") || strings.HasPrefix(text, "curl:") || strings.Contains(text, "<html") {
		msg := strings.TrimSpace(strings.Split(text, "\n")[0])
		if msg == "#down" || msg == "" {
			msg = "无响应"
		}
		rep.Error = "OpenResty 未运行（统计接口" + strings.TrimPrefix(msg, "curl: ") + "）"
		return rep
	}
	rep.Up = true
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || len(f[0]) < 2 {
			continue
		}
		id, err1 := strconv.ParseInt(f[0][1:], 10, 64)
		v, err2 := strconv.ParseUint(f[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		c := rep.Stats[id]
		switch f[0][0] {
		case 'r':
			c.Requests = v
		case 'u':
			c.Up = v
		case 'd':
			c.Down = v
		}
		rep.Stats[id] = c
	}
	return rep
}
