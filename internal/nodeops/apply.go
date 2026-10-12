package nodeops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"pveforward/internal/proto"
)

// Apply 应用期望状态：生成规则文件，校验后原子替换，重载 DHCP 静态绑定。
// 返回替换前的计数器快照。
func Apply(ctx context.Context, ex Executor, st *proto.DesiredState) (*proto.ApplyResult, error) {
	nft, err := RenderNft(st)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("set -e\nexport LC_ALL=C\n")
	b.WriteString("echo '##before'; nft -j list table inet " + TableName + " 2>/dev/null || true\necho\n")
	b.WriteString(writeFileCmd(RulesFile+".new", []byte(nft), "600"))
	b.WriteString("if ! nft -c -f " + RulesFile + ".new >&2; then echo '规则校验失败' >&2; exit 3; fi\n")
	b.WriteString("mv -f " + RulesFile + ".new " + RulesFile + "\n")
	b.WriteString(writeFileCmd(BootScript, []byte(RenderBoot(st.Net)), "700"))
	b.WriteString(writeFileCmd(SysctlConf, []byte(RenderSysctl(st.Net)), "644"))
	b.WriteString(writeFileCmd(UnitFile+".new", []byte(unitContent), "644"))
	b.WriteString("if ! cmp -s " + UnitFile + ".new " + UnitFile + "; then mv -f " + UnitFile + ".new " + UnitFile +
		"; systemctl daemon-reload; systemctl enable -q pve-forward-rules.service; else rm -f " + UnitFile + ".new; fi\n")
	if st.Net.DHCP == "managed" {
		b.WriteString(writeFileCmd(HostsFile, []byte(RenderHosts(st.Hosts)), "644"))
		b.WriteString("systemctl is-active -q dnsmasq && systemctl kill -s HUP dnsmasq || true\n")
	}
	b.WriteString("bash " + BootScript + "\n")
	out, err := ex.Run(ctx, b.String())
	if err != nil {
		return nil, fmt.Errorf("应用规则失败: %w", err)
	}
	before, _ := parseNftCounters(sections(out)["before"])
	return &proto.ApplyResult{Version: st.Version, Before: before}, nil
}

// InitNetwork 初始化节点网络。任一步失败时恢复备份的配置文件。
func InitNetwork(ctx context.Context, ex Executor, in proto.NetInit, progress func(proto.Step)) error {
	n := in.Net
	if !ValidIface(n.Bridge) || !ValidIface(n.WANIface) {
		return fmt.Errorf("网口或网桥名称无效")
	}
	if progress == nil {
		progress = func(proto.Step) {}
	}
	backup := `B=` + Dir + `/backup-$(date +%Y%m%d%H%M%S); mkdir -p "$B"
for f in /etc/network/interfaces ` + DnsmasqConf + ` ` + SysctlConf + ` ` + BootScript + `; do [ -f "$f" ] && cp -a "$f" "$B/$(echo "$f" | tr / _)"; done
ln -sfn "$B" ` + Dir + `/backup-last
`
	restore := `B=` + Dir + `/backup-last
for f in /etc/network/interfaces ` + DnsmasqConf + ` ` + SysctlConf + ` ` + BootScript + `; do
  k="$B/$(echo "$f" | tr / _)"; if [ -f "$k" ]; then cp -a "$k" "$f"; else rm -f "$f"; fi
done
ifreload -a 2>/dev/null || true
systemctl is-active -q dnsmasq && systemctl restart dnsmasq || true
`
	type step struct{ name, script string }
	var steps []step
	steps = append(steps, step{"备份现有配置", backup})

	pkgs := []string{"nftables", "conntrack"}
	pre := ""
	if n.DHCP == "managed" {
		conf, err := RenderDnsmasq(n)
		if err != nil {
			return err
		}
		// 先写配置再安装，避免 dnsmasq 以默认配置启动
		pre = writeFileCmd(DnsmasqConf, []byte(conf), "644") + "touch " + HostsFile + "\n"
		if in.InstallDHCP {
			pkgs = append(pkgs, "dnsmasq")
		}
	}
	steps = append(steps, step{"安装依赖（" + strings.Join(pkgs, "、") + "）", pre + `export DEBIAN_FRONTEND=noninteractive
need=""; for p in ` + strings.Join(pkgs, " ") + `; do dpkg -s "$p" >/dev/null 2>&1 || need="$need $p"; done
if [ -n "$need" ]; then apt-get install -y -q $need >/dev/null 2>&1 || { apt-get update -q >/dev/null 2>&1; apt-get install -y -q $need >&2; }; fi
`})

	cidr6 := ""
	if n.GW6 != "" {
		cidr6 = " --cidr6 " + shq(n.GW6)
	}
	switch n.AddrMode {
	case "new":
		steps = append(steps, step{"创建网桥 " + n.Bridge, fmt.Sprintf(`N=$(hostname -s)
pvesh create /nodes/$N/network --iface %s --type bridge --autostart 1 --cidr %s%s --comments 'PVE Forward' >&2
pvesh set /nodes/$N/network >&2
`, shq(n.Bridge), shq(n.GW4), cidr6)})
	case "replace":
		steps = append(steps, step{"替换网桥 " + n.Bridge + " 地址", fmt.Sprintf(`N=$(hostname -s)
pvesh set /nodes/$N/network/%s --type bridge --cidr %s%s >&2
pvesh set /nodes/$N/network >&2
`, shq(n.Bridge), shq(n.GW4), cidr6)})
	case "append":
		steps = append(steps, step{"为网桥 " + n.Bridge + " 追加地址", fmt.Sprintf("ip addr replace %s dev %s\n", n.GW4, n.Bridge)})
	}

	steps = append(steps, step{"开启 IP 转发并安装开机服务",
		writeFileCmd(SysctlConf, []byte(RenderSysctl(n)), "644") +
			writeFileCmd(BootScript, []byte(RenderBoot(n)), "700") +
			writeFileCmd(UnitFile, []byte(unitContent), "644") +
			"systemctl daemon-reload\nsystemctl enable -q pve-forward-rules.service\nbash " + BootScript + "\n"})

	if n.DHCP == "managed" {
		steps = append(steps, step{"启动 DHCP 服务", "systemctl enable -q dnsmasq\nsystemctl restart dnsmasq\nsleep 1\nsystemctl is-active -q dnsmasq || { journalctl -u dnsmasq -n 5 --no-pager >&2; exit 1; }\n"})
	}

	for i, s := range steps {
		if _, err := ex.Run(ctx, "set -e\nexport LC_ALL=C\n"+s.script); err != nil {
			progress(proto.Step{Name: s.name, Error: err.Error()})
			if i > 0 {
				_, rerr := ex.Run(context.WithoutCancel(ctx), restore)
				msg := "已恢复原配置"
				if rerr != nil {
					msg = "恢复原配置失败: " + rerr.Error()
				}
				progress(proto.Step{Name: "回滚", OK: rerr == nil, Error: msg})
			}
			return fmt.Errorf("%s失败: %w", s.name, err)
		}
		progress(proto.Step{Name: s.name, OK: true})
	}
	return nil
}

// RRD 读取 PVE 自带的历史数据。
func RRD(ctx context.Context, ex Executor, req proto.RRDReq) ([]proto.RRDPoint, error) {
	tf := map[string]string{"hour": "hour", "day": "day", "week": "week"}[req.Timeframe]
	if tf == "" {
		tf = "hour"
	}
	path := "/nodes/$N/rrddata"
	switch req.Type {
	case "qemu", "lxc":
		if req.VMID <= 0 {
			return nil, fmt.Errorf("vmid 无效")
		}
		path = fmt.Sprintf("/nodes/$N/%s/%d/rrddata", req.Type, req.VMID)
	}
	out, err := ex.Run(ctx, "N=$(hostname -s)\npvesh get "+path+" --timeframe "+tf+" --cf AVERAGE --output-format json\n")
	if err != nil {
		return nil, err
	}
	var pts []proto.RRDPoint
	if err := json.Unmarshal(out, &pts); err != nil {
		return nil, fmt.Errorf("解析 rrddata 失败: %w", err)
	}
	return pts, nil
}

// Cleanup 删除节点上由本系统创建的规则与配置（删除节点时调用）。网桥地址与已安装的软件包保留。
func Cleanup(ctx context.Context, ex Executor) error {
	_, err := ex.Run(ctx, `nft delete table inet `+TableName+` 2>/dev/null || true
systemctl disable -q --now pve-forward-rules.service 2>/dev/null || true
rm -f `+UnitFile+` `+RulesFile+` `+BootScript+` `+HostsFile+` `+DnsmasqConf+`
systemctl daemon-reload
systemctl is-active -q dnsmasq && systemctl restart dnsmasq || true
`)
	return err
}
