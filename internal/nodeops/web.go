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
)

var (
	webDirRe  = regexp.MustCompile(`^web-[0-9a-f]{12}$`)
	webFileRe = regexp.MustCompile(`^(nginx\.conf|sites/\d+\.conf|certs/\d+\.(crt|key)|htpasswd/\d+)$`)
	sshUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
	// 统计接口地址：只允许 http://IPv4:端口/pvef-stats
	webStatusRe = regexp.MustCompile(`^http://[0-9.]+:\d+/pvef-stats$`)
)

// Web 执行一次 Web 转发操作。
func Web(ctx context.Context, ex Executor, req *proto.WebReq) (*proto.WebResult, error) {
	tex, err := webExecutor(ex, req.Target)
	if err != nil {
		return nil, err
	}
	switch req.Op {
	case "check":
		return webCheck(ctx, tex, req)
	case "install":
		return webInstall(ctx, tex)
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
		return nil, fmt.Errorf("缺少 SSH 密码或私钥")
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
	default: // 使用宿主机 root 的密钥
		opts += " -o BatchMode=yes"
	}
	remote := "bash -s"
	if t.SSHUser != "root" {
		remote = "sudo -n bash -s"
	}
	fmt.Fprintf(&b, "mkdir -p %s\nprintf '%%s' '%s' | base64 -d | ssh %s %s@%s %s\n", Dir, b64(script), opts, t.SSHUser, t.VMIP, shq(remote))
	return b.String()
}

// openresty 可执行文件与主配置文件的位置
const webLocate = `BIN=$(command -v openresty 2>/dev/null || true)
[ -n "$BIN" ] || BIN=/usr/local/openresty/bin/openresty
CONF=""
[ -x "$BIN" ] && CONF=$("$BIN" -V 2>&1 | sed -n 's/.*--conf-path=\([^ ]*\).*/\1/p')
[ -n "$CONF" ] || CONF=/usr/local/openresty/nginx/conf/nginx.conf
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
echo '##openresty'; [ -x "$BIN" ] && "$BIN" -v 2>&1
echo '##managed'; [ -L `+Dir+`/web ] && echo yes
echo '##listen'; ss -Hltnp 2>/dev/null | awk '{print $4, $6}'
echo '##repo'; curl -sS -o /dev/null -m 8 -w '%{http_code}' https://openresty.org/package/pubkey.gpg 2>&1 || true
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
	sysOK, sysDetail := true, strings.TrimSpace(pretty+" · "+arch)
	if req.Deploy != "manual" && version == "" {
		if !apt {
			sysOK, sysDetail = false, sysDetail+"（自动安装仅支持 Debian / Ubuntu，请手动安装 OpenResty 后选择「已自行安装」）"
		} else if arch != "x86_64" && arch != "aarch64" {
			sysOK, sysDetail = false, sysDetail+"（OpenResty 官方软件源只支持 x86_64 与 aarch64）"
		}
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
		if managed && (proc == "nginx" || proc == "openresty") {
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

	switch {
	case version != "":
		add("OpenResty", true, "已安装 "+strings.TrimPrefix(version, "nginx version: "))
	case req.Deploy == "manual":
		add("OpenResty", false, "未安装 OpenResty，请先安装或选择「自动安装」")
	default:
		code := strings.TrimSpace(s["repo"])
		add("OpenResty", code == "200", map[bool]string{true: "未安装，可从 openresty.org 官方软件源安装", false: "无法访问 openresty.org 官方软件源（" + code + "）"}[code == "200"])
	}
	return res, nil
}

const webInstallScript = `set -e
export DEBIAN_FRONTEND=noninteractive LC_ALL=C
` + webLocate + `if [ -x "$BIN" ]; then echo '##version'; "$BIN" -v 2>&1; exit 0; fi
command -v apt-get >/dev/null 2>&1 || { echo "自动安装仅支持 Debian / Ubuntu，请手动安装 OpenResty 后选择「已自行安装」" >&2; exit 2; }
. /etc/os-release
need=""; for p in curl gnupg ca-certificates; do dpkg -s "$p" >/dev/null 2>&1 || need="$need $p"; done
if [ -n "$need" ]; then apt-get install -y -q $need >/dev/null 2>&1 || { apt-get update -q >/dev/null 2>&1; apt-get install -y -q $need >&2; }; fi
case "$ID" in ubuntu) D=ubuntu; C=main ;; *) D=debian; C=openresty ;; esac
A=""; [ "$(uname -m)" = aarch64 ] && A=arm64/
U="https://openresty.org/package/${A}${D}"
CODE="$VERSION_CODENAME"
if ! curl -fsS -m 10 -o /dev/null "$U/dists/$CODE/Release"; then
  # 较新的发行版可能还没有对应的软件源，使用最近的版本
  for c in bookworm bullseye noble jammy focal; do
    if curl -fsS -m 10 -o /dev/null "$U/dists/$c/Release"; then CODE=$c; break; fi
  done
fi
curl -fsSL -m 30 https://openresty.org/package/pubkey.gpg | gpg --dearmor --yes -o /usr/share/keyrings/openresty.gpg
echo "deb [signed-by=/usr/share/keyrings/openresty.gpg] $U $CODE $C" > /etc/apt/sources.list.d/openresty.list
apt-get update -q -o Dir::Etc::sourcelist=sources.list.d/openresty.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 >/dev/null
apt-get install -y -q --no-install-recommends openresty >/dev/null
mkdir -p ` + Dir + ` && touch ` + webMarker + `
# 安装包会以默认配置启动并占用 80 端口：先停止，下发配置后再启动
systemctl stop openresty 2>/dev/null || true
` + webLocate + `echo '##version'; "$BIN" -v 2>&1
`

func webInstall(ctx context.Context, ex Executor) (*proto.WebResult, error) {
	out, err := ex.Run(ctx, webInstallScript)
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
	b.WriteString(`[ -x "$BIN" ] || { echo "未安装 OpenResty" >&2; exit 4; }` + "\n")
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
if ! "$BIN" -t -q -c "$D/nginx.conf" 2>"$D.err"; then
  grep -v '^nginx: configuration file .* test failed' "$D.err" | sed 's/^nginx: //' | tail -5 >&2
  rm -rf "$D" "$D.err"; exit 3
fi
rm -f "$D.err"
# 主配置只 include 当前版本（首次接管时备份原配置）
if [ -f "$CONF" ] && ! grep -q 'PVE Forward' "$CONF" && [ ! -f "$CONF.pvef-orig" ]; then cp -a "$CONF" "$CONF.pvef-orig"; fi
printf '# 由 PVE Forward 管理，原配置备份为 nginx.conf.pvef-orig\ninclude ` + Dir + `/web/nginx.conf;\n' > "$CONF.tmp"
mv -f "$CONF.tmp" "$CONF"
P=$(readlink ` + Dir + `/web 2>/dev/null || true)
ln -sfn "$(basename "$D")" ` + Dir + `/web.lnk && mv -Tf ` + Dir + `/web.lnk ` + Dir + `/web
L1=` + webLogDir + `/error.log; L2="$(dirname "$CONF")/../logs/error.log"
S1=$(stat -c %s "$L1" 2>/dev/null || echo 0); S2=$(stat -c %s "$L2" 2>/dev/null || echo 0)
if command -v systemctl >/dev/null 2>&1 && systemctl cat openresty >/dev/null 2>&1; then
  systemctl enable -q openresty 2>/dev/null || true
  if systemctl is-active -q openresty; then systemctl reload openresty || true; else systemctl restart openresty || true; fi
else
  "$BIN" -s reload 2>/dev/null || "$BIN" || true
fi
sleep 1
# 重载时监听端口失败等错误只记录在日志中：检查新增的 emerg 日志，失败则切回原版本
E=$( (tail -c +$((S1+1)) "$L1" 2>/dev/null; tail -c +$((S2+1)) "$L2" 2>/dev/null) | grep '\[emerg\]' | tail -3 || true)
RUN=1; if command -v systemctl >/dev/null 2>&1 && systemctl cat openresty >/dev/null 2>&1; then systemctl is-active -q openresty || RUN=0; fi
if [ -n "$E" ] || [ "$RUN" = 0 ]; then
  [ -n "$E" ] && echo "$E" | sed 's/^.*\[emerg\] [0-9#]*: //' | awk '!seen[$0]++' >&2
  [ "$RUN" = 0 ] && journalctl -u openresty -n 5 --no-pager -o cat >&2
  if [ -n "$P" ] && [ -d "` + Dir + `/$P" ]; then
    ln -sfn "$P" ` + Dir + `/web.lnk && mv -Tf ` + Dir + `/web.lnk ` + Dir + `/web
    systemctl reload openresty 2>/dev/null || systemctl restart openresty 2>/dev/null || "$BIN" -s reload 2>/dev/null || true
  fi
  rm -rf "$D"; exit 5
fi
for x in ` + Dir + `/web-*; do [ "$x" = "$D" ] || rm -rf "$x"; done
echo '##version'; "$BIN" -v 2>&1
`)
	out, err := ex.Run(ctx, b.String())
	if err != nil {
		return nil, fmt.Errorf("下发 Web 配置失败: %w", err)
	}
	return &proto.WebResult{Version: strings.TrimSpace(sections(out)["version"])}, nil
}

// webRemove 停用本系统的 Web 配置：恢复 OpenResty 原配置；若由本系统安装则停止服务。
func webRemove(ctx context.Context, ex Executor) error {
	_, err := ex.Run(ctx, "export LC_ALL=C\n"+webLocate+`if [ -f "$CONF.pvef-orig" ]; then mv -f "$CONF.pvef-orig" "$CONF"; fi
if [ -f `+webMarker+` ]; then
  PIDF=$("$BIN" -V 2>&1 | sed -n 's/.*--pid-path=\([^ ]*\).*/\1/p'); [ -n "$PIDF" ] || PIDF=/usr/local/openresty/nginx/logs/nginx.pid
  PID=$(cat "$PIDF" 2>/dev/null || true)
  systemctl disable -q --now openresty 2>/dev/null || "$BIN" -s stop 2>/dev/null || true
  # 停止是异步的：等待主进程退出（最多 10 秒）
  if [ -n "$PID" ]; then for i in $(seq 1 50); do kill -0 "$PID" 2>/dev/null || break; sleep 0.2; done; fi
  rm -f `+webMarker+`
elif [ -x "$BIN" ]; then
  systemctl reload openresty 2>/dev/null || "$BIN" -s reload 2>/dev/null || true
fi
rm -rf `+Dir+`/web `+Dir+`/web.lnk `+Dir+`/web-*
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
