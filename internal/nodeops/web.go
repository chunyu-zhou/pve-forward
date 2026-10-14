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
	if req.Op != "install" {
		ctx = WithOutputLog(ctx, nil)
	}
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
	case "free", "restore":
		return &proto.WebResult{}, webFree(ctx, tex, req.Op, req.Stop)
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

// OpenResty 的运行方式（记录在 webRuntimeFile）：
//   - binary：控制端上传的二进制包，安装在 BinPrefix，由 pve-forward-openresty 服务运行
//   - docker：openresty 容器（host 网络），挂载配置目录；可以是本系统拉取的镜像，也可以是接管的已有容器的镜像
//   - system：已有的 OpenResty（apt、官方包或源码编译），主配置 include 本系统的配置
const (
	BinPrefix      = "/usr/local/pve-forward-openresty"
	webRuntimeFile = Dir + "/openresty-runtime"
	webImageFile   = Dir + "/openresty-image"
	// 接管的已有 OpenResty 容器（"容器名|原重启策略"），停用 Web 转发时恢复
	webAdoptedFile = Dir + "/openresty-adopted"
	webUnit        = "pve-forward-openresty"
	webContainer   = "pve-forward-openresty"
	webBundleTmp   = "/tmp/pve-forward-openresty.tgz"
)

// webLocate 定义当前运行方式的 OpenResty 操作（RT、or_cmd、or_ok、or_test、or_reload、or_running）。
// 记录的运行方式失效时（例如安装失败后残留）重新检测，自行安装的 OpenResty 也能识别。
const webLocate = `PATH="$PATH:/usr/local/openresty/bin:/usr/local/openresty/nginx/sbin:/usr/local/sbin:/usr/sbin:/sbin"
MAIN=` + Dir + `/web/nginx.conf
DV="-v ` + Dir + `:` + Dir + `:ro -v /var/log/pve-forward-web:/var/log/pve-forward-web -v /var/cache/pve-forward-web:/var/cache/pve-forward-web"
RT=$(cat ` + webRuntimeFile + ` 2>/dev/null || true)
IMG=$(cat ` + webImageFile + ` 2>/dev/null || true)
BIN=""
has_systemd() { command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; }
# 与本脚本在同一命名空间（不在容器内）运行的 OpenResty 主进程
or_masters() {
  for p in $(pgrep -x nginx 2>/dev/null) $(pgrep -x openresty 2>/dev/null); do
    [ "$(readlink /proc/$p/ns/mnt 2>/dev/null)" = "$(readlink /proc/self/ns/mnt)" ] || continue
    tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null | grep -q 'master process' && echo "$p"
  done
}
# 自行安装的 OpenResty（apt、官方包、源码编译）：PATH 中的命令、常见安装路径或正在运行的主进程
find_system() {
  for b in "$(command -v openresty 2>/dev/null)" /usr/local/openresty/bin/openresty /usr/local/openresty/nginx/sbin/nginx /opt/openresty/bin/openresty /opt/openresty/nginx/sbin/nginx; do
    [ -n "$b" ] && [ -x "$b" ] && "$b" -v 2>&1 | grep -qi openresty && { echo "$b"; return 0; }
  done
  for p in $(or_masters); do
    b=$(readlink /proc/$p/exe 2>/dev/null) || continue
    case "$b" in ` + BinPrefix + `/*) continue ;; esac
    [ -x "$b" ] && "$b" -v 2>&1 | grep -qi openresty && { echo "$b"; return 0; }
  done
  return 1
}
pick_rt() {
  if [ -x ` + BinPrefix + `/bin/openresty ]; then RT=binary
  elif [ -n "$IMG" ] && command -v docker >/dev/null 2>&1 && docker inspect ` + webContainer + ` >/dev/null 2>&1; then RT=docker
  elif find_system >/dev/null; then RT=system
  else RT=""; fi
}
set_bin() {
  case "$RT" in
    binary) BIN=` + BinPrefix + `/bin/openresty ;;
    system) BIN=$(find_system) || BIN=/usr/local/openresty/bin/openresty ;;
    *) BIN=$(find_system) || BIN=/usr/local/openresty/bin/openresty ;;
  esac
}
or_cmd() { if [ "$RT" = docker ]; then docker exec ` + webContainer + ` openresty "$@"; else "$BIN" "$@"; fi; }
or_ok() {
  case "$RT" in
    docker) command -v docker >/dev/null 2>&1 && [ -n "$IMG" ] && docker image inspect "$IMG" >/dev/null 2>&1 ;;
    binary|system) [ -x "$BIN" ] ;;
    *) return 1 ;;
  esac
}
[ -n "$RT" ] || pick_rt
set_bin
or_ok || { pick_rt; set_bin; }
CONF=""
if [ "$RT" = system ]; then
  # 正在运行的主进程以 -c 指定的主配置为准
  for p in $(or_masters); do
    [ "$(readlink /proc/$p/exe 2>/dev/null)" = "$BIN" ] || continue
    C=$(tr '\0' '\n' < /proc/$p/cmdline | sed -n '/^-c$/{n;p;q}')
    case "$C" in /*) [ "$C" = "$MAIN" ] || CONF=$C ;; esac
  done
  [ -n "$CONF" ] || CONF=$("$BIN" -V 2>&1 | sed -n 's/.*--conf-path=\([^ ]*\).*/\1/p')
fi
[ -n "$CONF" ] || CONF=/usr/local/openresty/nginx/conf/nginx.conf
# Docker：以 openresty 为入口运行镜像（兼容自带入口脚本的镜像）
or_version() { if [ "$RT" = docker ]; then docker run --rm --entrypoint openresty "$IMG" -v 2>&1; else "$BIN" -v 2>&1; fi; }
or_test() { if [ "$RT" = docker ]; then docker run --rm --network none $DV --entrypoint openresty "$IMG" -t -q -c "$1"; else "$BIN" -t -q -c "$1"; fi; }
or_running() {
  case "$RT" in
    docker) [ "$(docker inspect -f '{{.State.Status}}' ` + webContainer + ` 2>/dev/null)" = running ] ;;
    binary) if has_systemd; then systemctl is-active -q ` + webUnit + `; else [ -s ` + BinPrefix + `/nginx/logs/nginx.pid ] && kill -0 "$(cat ` + BinPrefix + `/nginx/logs/nginx.pid)" 2>/dev/null; fi ;;
    *) if has_systemd && systemctl cat openresty >/dev/null 2>&1; then systemctl is-active -q openresty; else return 0; fi ;;
  esac
}
or_reload() {
  case "$RT" in
    docker)
      # 运行中且镜像未变时重载；否则（未运行、反复重启、镜像已更换）删除后按当前镜像与挂载重新创建
      if or_running && [ "$(docker inspect -f '{{.Config.Image}}' ` + webContainer + ` 2>/dev/null)" = "$IMG" ]; then docker exec ` + webContainer + ` openresty -c "$MAIN" -s reload
      else
        docker rm -f ` + webContainer + ` >/dev/null 2>&1 || true
        docker run -d --name ` + webContainer + ` --restart unless-stopped --network host $DV --entrypoint openresty "$IMG" -g 'daemon off;' -c "$MAIN" >/dev/null
      fi ;;
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

// webDetectScript 输出环境信息、已安装的 OpenResty 与监听端口的归属。
//   - ##containers：OpenResty 容器（运行中的按容器内的 OpenResty 主进程识别，不一定是 1 号进程，如 Nginx Proxy Manager；
//     已停止的按镜像名识别），每行「容器|镜像|镜像ID|网络|运行中|镜像中 openresty 命令的版本（无则为空）|发布的端口」
//   - ##listen：每行「地址 进程 归属」，归属 or 为本机的 OpenResty 进程，ct:<容器名> 为容器（含端口映射），other 为其他进程
const webDetectScript = `echo '##uid'; id -u
echo '##os'; ( . /etc/os-release 2>/dev/null; echo "$PRETTY_NAME|$ID|$ID_LIKE" )
echo '##arch'; uname -m
echo '##apt'; command -v apt-get >/dev/null 2>&1 && echo yes
echo '##rpm'; { command -v dnf || command -v yum; } >/dev/null 2>&1 && echo yes
echo '##systemd'; command -v systemctl >/dev/null 2>&1 && echo yes
echo '##openresty'; or_ok && or_version
echo '##runtime'; or_ok && echo "$RT"
echo '##bin'; if [ "$RT" = docker ]; then echo "$IMG"; else echo "$BIN"; fi
echo '##docker'; command -v docker >/dev/null 2>&1 && docker version --format '{{.Server.Version}}' 2>/dev/null
SELF=$(readlink /proc/self/ns/mnt)
CT=""; DK=""; IDS=""
ct_of() { id=$(grep -o '[0-9a-f]\{64\}' /proc/$1/cgroup 2>/dev/null | head -1); [ -n "$id" ] && printf '%s\n' "$IDS" | awk -v id="$id" '$1==id {print $2; exit}'; }
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  IDS=$(docker ps --no-trunc --format '{{.ID}} {{.Names}}')
  DK=$(docker ps --format '{{.Names}}' | while read -r C; do docker port "$C" 2>/dev/null | sed -n "s/.*:\([0-9][0-9]*\)$/\1 $C/p"; done | sort -u)
  RUN=$(for p in $(pgrep -x nginx 2>/dev/null) $(pgrep -x openresty 2>/dev/null); do
    [ "$(readlink /proc/$p/ns/mnt 2>/dev/null)" != "$SELF" ] || continue
    tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null | grep -q 'master process' || continue
    grep -qi 'openresty\|luajit' /proc/$p/maps 2>/dev/null || continue
    ct_of "$p"
  done | sort -u)
  EXITED=$(docker ps -a --filter status=exited --filter status=created --format '{{.Names}}|{{.Image}}' | awk -F'|' 'tolower($2) ~ /openresty/ {print $1}')
  for C in $RUN $EXITED; do
    case "$C" in ` + webContainer + `|` + webContainer + `-previous) continue ;; esac
    I=$(docker inspect -f '{{.Config.Image}}|{{.Image}}|{{.HostConfig.NetworkMode}}|{{.State.Running}}' "$C" 2>/dev/null) || continue
    V=$(docker run --rm --entrypoint openresty "$(echo "$I" | cut -d'|' -f2)" -v 2>&1 | grep -i 'version: openresty/' | head -1)
    PS=$(printf '%s\n' "$DK" | awk -v c="$C" '$2==c {print $1}' | tr '\n' ',')
    CT="$CT$C|$I|$V|$PS
"
  done
fi
echo '##containers'; printf '%s' "$CT"
echo '##listen'
ss -Hltnp 2>/dev/null | awk '{print $4, $6}' | while read -r A U; do
  P=$(printf '%s' "$U" | sed -n 's/.*pid=\([0-9]*\).*/\1/p')
  O=other
  if [ -n "$P" ]; then
    case "$U" in
      *'"docker-proxy"'*) N=$(printf '%s\n' "$DK" | awk -v p="${A##*:}" '$1==p {print $2; exit}') ;;
      *) N=""; [ "$(readlink /proc/$P/ns/mnt 2>/dev/null)" = "$SELF" ] || N=$(ct_of "$P") ;;
    esac
    if [ -n "$N" ]; then O="ct:$N"
    elif [ "$(readlink /proc/$P/ns/mnt 2>/dev/null)" = "$SELF" ] && grep -qi 'openresty\|luajit' /proc/$P/maps 2>/dev/null; then O=or; fi
  fi
  echo "$A $U $O"
done
`

func webCheck(ctx context.Context, ex Executor, req *proto.WebReq) (*proto.WebResult, error) {
	res := &proto.WebResult{}
	add := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, proto.WebCheck{Name: name, OK: ok, Detail: detail})
	}
	out, err := ex.Run(ctx, "export LC_ALL=C\n"+webLocate+webDetectScript)
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
	apt, rpm := strings.TrimSpace(s["apt"]) == "yes", strings.TrimSpace(s["rpm"]) == "yes"
	version := strings.TrimSpace(s["openresty"])
	if !strings.Contains(version, "openresty") {
		version = ""
	}
	res.Version, res.OS = version, pretty
	res.Arch, res.Runtime = arch, strings.TrimSpace(s["runtime"])
	docker := strings.TrimSpace(s["docker"])
	res.Docker = docker
	short := func(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "nginx version: ") }

	// 已安装的 OpenResty：本系统部署的或自行安装的优先，其次是 Docker 容器的镜像（运行中的优先）
	for _, l := range strings.Split(strings.TrimSpace(s["containers"]), "\n") {
		f := strings.Split(l, "|")
		if len(f) < 7 || !containerNameRe.MatchString(f[0]) || !imageIDRe.MatchString(f[2]) {
			continue
		}
		c := proto.WebContainer{Name: f[0], Image: f[1], ImageID: f[2], Network: f[3], Running: f[4] == "true", Version: short(f[5])}
		for _, p := range strings.Split(f[6], ",") {
			if n, err := strconv.Atoi(p); err == nil {
				c.Ports = append(c.Ports, n)
			}
		}
		res.Containers = append(res.Containers, c)
	}
	sort.SliceStable(res.Containers, func(i, j int) bool { return res.Containers[i].Running && !res.Containers[j].Running })
	if version != "" && res.Runtime != "" {
		res.Existing = &proto.WebExisting{Kind: res.Runtime, Version: short(version), Path: strings.TrimSpace(s["bin"])}
	} else {
		for _, c := range res.Containers {
			if c.Version != "" {
				res.Existing = &proto.WebExisting{Kind: "container", Container: c.Name, Image: c.Image, ImageID: c.ImageID, Version: c.Version}
				break
			}
		}
	}
	existing := res.Existing

	install := existing == nil
	sysOK, sysDetail := true, strings.TrimSpace(pretty+" · "+arch)
	switch {
	case install && req.Deploy == "binary" && arch != "x86_64" && arch != "aarch64":
		sysOK, sysDetail = false, sysDetail+"（二进制安装只支持 x86_64 与 aarch64）"
	case install && req.Deploy == "docker" && docker == "" && req.DockerSource == DockerSourceDistro && !apt:
		sysOK, sysDetail = false, sysDetail+"（通过系统软件源安装 Docker 仅支持 Debian / Ubuntu）"
	case install && req.Deploy == "docker" && docker == "" && !apt && !rpm:
		sysOK, sysDetail = false, sysDetail+"（未安装 Docker，且不支持自动安装，请先手动安装 Docker）"
	}
	add("系统支持", sysOK, sysDetail)
	if strings.TrimSpace(s["systemd"]) != "yes" {
		// 不影响部署（直接启动 OpenResty），只是不会开机自启
		add("systemd", true, "未检测到 systemd：OpenResty 将直接启动，系统重启后需要手动启动")
	}

	switch {
	case existing == nil:
		switch req.Deploy {
		case "manual":
			add("OpenResty", false, "未检测到已安装的 OpenResty，请选择二进制或 Docker 安装")
		case "docker":
			add("OpenResty", true, "未安装，将拉取 OpenResty 镜像运行")
			switch {
			case docker != "":
				add("Docker", true, "已安装 Docker "+docker)
			case req.DockerSource == DockerSourceDistro:
				add("Docker", apt, "未安装，将通过系统软件源安装 Docker（docker.io）")
			case apt || rpm:
				add("Docker", true, "未安装，将从 "+dockerSourceLabel(req.DockerSource)+" 安装 Docker CE")
			default:
				add("Docker", false, "未安装 Docker")
			}
		default:
			add("OpenResty", true, "未安装，将由控制端上传二进制版 OpenResty（目标机器不需要访问外网）")
		}
	case existing.Kind == "binary":
		add("OpenResty", true, "已安装 "+existing.Version+"（本系统安装的二进制版），直接使用，无需重新安装")
	case existing.Kind == "docker":
		add("OpenResty", true, "已安装 "+existing.Version+"（Docker 镜像 "+existing.Path+"），直接使用，无需重新安装")
	case existing.Kind == "system":
		add("OpenResty", true, "已安装 "+existing.Version+"（"+existing.Path+"），直接使用，无需重新安装；部署时原主配置备份为 nginx.conf.pvef-orig")
	default:
		add("OpenResty", true, fmt.Sprintf("已安装 %s（Docker 容器 %s 的镜像 %s），直接使用该镜像运行，无需重新安装", existing.Version, existing.Container, existing.Image))
	}

	// 端口：被其他进程占用则不可用；将要使用的 OpenResty 与将要停止的容器占用的不算
	stop := map[string]bool{webContainer: true}
	for _, c := range req.Stop {
		stop[c] = true
	}
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
		if err != nil || !want[p] || busy[p] != "" {
			continue
		}
		owner := ""
		if len(f) > 2 {
			owner = f[2]
		}
		proc := "其他进程"
		if len(f) > 1 {
			if m := procRe.FindStringSubmatch(f[1]); m != nil {
				proc = m[1]
			}
		}
		b := proto.WebBusy{Port: p, Process: proc}
		switch name, isCT := strings.CutPrefix(owner, "ct:"); {
		case owner == "or" && existing != nil && (existing.Kind == "binary" || existing.Kind == "system"):
			continue
		case owner == "or":
			b.Process = "另一个 OpenResty（" + proc + "）"
		case isCT && stop[name]:
			continue
		case isCT && containerNameRe.MatchString(name):
			b.Process, b.Container = "Docker 容器 "+name, name
		}
		busy[p] = b.Process
		res.Busy = append(res.Busy, b)
	}
	sort.Slice(res.Busy, func(i, j int) bool { return res.Busy[i].Port < res.Busy[j].Port })
	ports := append([]int(nil), req.Ports...)
	sort.Ints(ports)
	var names []string
	for _, p := range ports {
		names = append(names, strconv.Itoa(p))
	}
	if len(busy) == 0 {
		add("端口 "+strings.Join(names, " / ")+" 未被占用", true, "")
	} else {
		groups := map[string][]string{}
		var processes []string
		for _, p := range ports {
			if proc := busy[p]; proc != "" {
				if len(groups[proc]) == 0 {
					processes = append(processes, proc)
				}
				groups[proc] = append(groups[proc], strconv.Itoa(p))
			}
		}
		var details []string
		for _, proc := range processes {
			details = append(details, strings.Join(groups[proc], "、")+" 被 "+proc+" 占用")
		}
		state := "部分被占用"
		if len(busy) == len(want) {
			state = "全部被占用"
		}
		add("端口 "+strings.Join(names, " / ")+" "+state, false, strings.Join(details, "，"))
	}
	return res, nil
}

var procRe = regexp.MustCompile(`\(\("([^"]+)"`)

func dockerSourceLabel(v string) string {
	for _, m := range DockerCESources {
		if m.Value == v {
			return m.Label
		}
	}
	return v
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

// webRestoreAdopted 恢复为空出 Web 端口而停止的容器（原重启策略并启动）。带参数时只恢复指定的容器。
const webRestoreAdopted = `restore_adopted() {
  [ -f ` + webAdoptedFile + ` ] || return 0
  rm -f ` + webAdoptedFile + `.keep
  while IFS='|' read -r AC AP; do
    [ -n "$AC" ] || continue
    if [ $# -gt 0 ]; then case " $* " in *" $AC "*) ;; *) printf '%s|%s\n' "$AC" "$AP" >> ` + webAdoptedFile + `.keep; continue ;; esac; fi
    if docker inspect "$AC" >/dev/null 2>&1; then
      docker update --restart="${AP:-no}" "$AC" >/dev/null 2>&1 || true
      docker start "$AC" >/dev/null 2>&1 || true
    fi
  done < ` + webAdoptedFile + `
  if [ -f ` + webAdoptedFile + `.keep ]; then mv -f ` + webAdoptedFile + `.keep ` + webAdoptedFile + `; else rm -f ` + webAdoptedFile + `; fi
}
`

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
echo '正在校验并切换 OpenResty 运行环境…'
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

// webInstallDocker 安装 Docker（参考 LinuxMirrors）：从所选软件源安装 Docker CE，并校验软件源提供的
// GPG 密钥是否为 Docker 官方密钥（加速源无法替换签名）；或通过系统软件源安装 docker.io。$SRC 为软件源。
const webInstallDocker = `install_docker() {
  . /etc/os-release
  if [ "$SRC" = ` + DockerSourceDistro + ` ]; then
    command -v apt-get >/dev/null 2>&1 || { echo "通过系统软件源安装 Docker 仅支持 Debian / Ubuntu，请选择 Docker CE 软件源" >&2; exit 2; }
    echo '正在通过系统软件源安装 Docker（docker.io）…'
    apt-get update -q
    apt-get install -y -q docker.io
    return
  fi
  echo "正在从 $SRC 安装 Docker CE…"
  K=$(mktemp)
  if command -v apt-get >/dev/null 2>&1; then
    CODE=$VERSION_CODENAME
    case "$ID" in
      ubuntu|debian|raspbian) BR=$ID ;;
      *) case " $ID_LIKE " in
           *" ubuntu "*) BR=ubuntu; CODE=${UBUNTU_CODENAME:-$VERSION_CODENAME} ;;
           *" debian "*) BR=debian; CODE=${DEBIAN_CODENAME:-$VERSION_CODENAME} ;;
           *) echo "不支持自动安装 Docker 的系统：$PRETTY_NAME" >&2; exit 2 ;;
         esac ;;
    esac
    [ -n "$CODE" ] || { echo "无法识别系统版本代号，请手动安装 Docker" >&2; exit 2; }
    apt-get update -q
    apt-get install -y -q ca-certificates curl gnupg
    curl -fsSL "https://$SRC/linux/$BR/gpg" -o "$K"
    gpg --show-keys --with-colons "$K" 2>/dev/null | grep -q '^fpr:*9DC858229FC7DD38854AE2D88D81803C0EBFCD88:' || { rm -f "$K"; echo "软件源提供的 GPG 密钥不是 Docker 官方密钥，已停止安装" >&2; exit 2; }
    install -m 0755 -d /etc/apt/keyrings
    install -m 0644 "$K" /etc/apt/keyrings/docker.asc
    rm -f "$K"
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://$SRC/linux/$BR $CODE stable" > /etc/apt/sources.list.d/docker.list
    apt-get update -q
    apt-get install -y -q docker-ce docker-ce-cli containerd.io
  elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
    PM=$(command -v dnf || command -v yum)
    REL=${VERSION_ID%%.*}
    case "$ID" in
      fedora) BR=fedora; REL='$releasever' ;;
      rhel) BR=rhel ;;
      *) BR=centos; case "$REL" in 7|8|9|10) ;; *) REL=9 ;; esac ;;
    esac
    curl -fsSL "https://$SRC/linux/$BR/gpg" -o "$K"
    gpg --show-keys --with-colons "$K" 2>/dev/null | grep -q '^fpr:*060A61C51B558A7F742B77AAC52FEB6B621E9F35:' || { rm -f "$K"; echo "软件源提供的 GPG 密钥不是 Docker 官方密钥，已停止安装" >&2; exit 2; }
    install -m 0644 "$K" /etc/pki/rpm-gpg/docker-ce.gpg
    rm -f "$K"
    printf '[docker-ce-stable]\nname=Docker CE Stable\nbaseurl=https://%s/linux/%s/%s/$basearch/stable\nenabled=1\ngpgcheck=1\ngpgkey=file:///etc/pki/rpm-gpg/docker-ce.gpg\n' "$SRC" "$BR" "$REL" > /etc/yum.repos.d/docker-ce.repo
    "$PM" install -y docker-ce docker-ce-cli containerd.io
  else
    rm -f "$K"
    echo "不支持自动安装 Docker 的系统：$PRETTY_NAME，请手动安装 Docker" >&2; exit 2
  fi
}
`

var containerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var imageIDRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

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
echo '正在解压 OpenResty 安装包…'
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
		if req.DockerSource == "" {
			req.DockerSource = DockerSourceOfficial
		}
		if !ValidDockerSource(req.DockerSource) {
			return nil, fmt.Errorf("Docker 软件源无效: %q", req.DockerSource)
		}
		script = `set -e
export DEBIAN_FRONTEND=noninteractive LC_ALL=C
NEW=docker
SRC=` + shq(req.DockerSource) + `
` + webInstallGuard + webInstallDocker + `if ! command -v docker >/dev/null 2>&1; then
  install_docker
fi
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then systemctl enable -q --now docker 2>/dev/null || true; fi
echo '正在拉取 OpenResty 镜像 ` + req.Image + `…'
docker pull ` + shq(req.Image) + `
mkdir -p ` + Dir + `
` + webStopOthers + `echo ` + shq(req.Image) + ` > ` + webImageFile + `
echo docker > ` + webRuntimeFile + `
`
	case "adopt":
		// 使用已有 OpenResty 容器的镜像运行本系统管理的容器（不重新下载；原容器不受影响）
		if !imageIDRe.MatchString(req.Image) {
			return nil, fmt.Errorf("镜像 ID 无效: %q", req.Image)
		}
		script = `set -e
export LC_ALL=C
NEW=docker
` + webInstallGuard + `docker image inspect ` + req.Image + ` >/dev/null
docker run --rm --entrypoint openresty ` + req.Image + ` -v 2>&1 | grep -qi openresty || { echo "镜像中没有 openresty 命令，不能直接使用" >&2; exit 2; }
mkdir -p ` + Dir + `
` + webStopOthers + `echo ` + req.Image + ` > ` + webImageFile + `
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

// webFree 停止占用 Web 端口的容器（free）：保留容器，改为不自动重启并记录原重启策略，停用 Web 转发时恢复；
// restore 恢复本次停止的容器（部署失败时）。
func webFree(ctx context.Context, ex Executor, op string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	var list []string
	for _, n := range names {
		if !containerNameRe.MatchString(n) || n == webContainer {
			return fmt.Errorf("容器名无效: %q", n)
		}
		list = append(list, shq(n))
	}
	if op == "restore" {
		// 本系统的容器未正常运行时先删除，避免其自动重启后与恢复的容器争抢端口
		_, err := ex.Run(ctx, "export LC_ALL=C\n"+webRestoreAdopted+
			`[ "$(docker inspect -f '{{.State.Status}}' `+webContainer+` 2>/dev/null)" = running ] || docker rm -f `+webContainer+" >/dev/null 2>&1 || true\n"+
			"restore_adopted "+strings.Join(list, " ")+"\n")
		return err
	}
	_, err := ex.Run(ctx, `set -e
export LC_ALL=C
mkdir -p `+Dir+`
for C in `+strings.Join(list, " ")+`; do
  [ "$(docker inspect -f '{{.State.Running}}' "$C" 2>/dev/null)" = true ] || continue
  awk -F'|' -v c="$C" '$1==c {f=1} END {exit !f}' `+webAdoptedFile+` 2>/dev/null ||
    printf '%s|%s\n' "$C" "$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$C")" >> `+webAdoptedFile+`
  echo "正在停止容器 $C（保留容器，停用 Web 转发时恢复）…"
  docker update --restart=no "$C" >/dev/null
  docker stop -t 10 "$C" >/dev/null
done
`)
	if err != nil {
		return fmt.Errorf("停止容器失败: %w", err)
	}
	return nil
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
  else
    # 没有可恢复的旧版本：不留下指向已删除目录的链接，也不留下反复重启、争抢端口的容器
    rm -f ` + Dir + `/web
    [ "$RT" != docker ] || docker rm -f ` + webContainer + ` >/dev/null 2>&1 || true
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
	_, err := ex.Run(ctx, "export LC_ALL=C\n"+webLocate+webRestoreAdopted+`case "$RT" in
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
# 恢复为空出 Web 端口而停止的容器
if command -v docker >/dev/null 2>&1; then restore_adopted; fi
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
