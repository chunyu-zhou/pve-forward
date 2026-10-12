package nodeops

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"pveforward/internal/proto"
)

const (
	Dir         = "/etc/pve-forward"
	RulesFile   = Dir + "/rules.nft"
	HostsFile   = Dir + "/dhcp-hosts"
	BootScript  = Dir + "/boot.sh"
	DnsmasqConf = "/etc/dnsmasq.d/pve-forward.conf"
	SysctlConf  = "/etc/sysctl.d/99-pve-forward.conf"
	UnitFile    = "/etc/systemd/system/pve-forward-rules.service"
	TableName   = "pve_forward"
)

var ifaceRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// ValidIface 校验网卡名，防止注入到 nft / shell。
func ValidIface(s string) bool { return ifaceRe.MatchString(s) }

func protos(p string) []string {
	switch p {
	case "tcp":
		return []string{"tcp"}
	case "udp":
		return []string{"udp"}
	default:
		return []string{"tcp", "udp"}
	}
}

// RenderNft 生成规则表。使用 “table; delete table; table {...}” 写法，整表原子替换且可重复执行。
func RenderNft(st *proto.DesiredState) (string, error) {
	n := st.Net
	if !ValidIface(n.WANIface) {
		return "", fmt.Errorf("公网网口名称无效: %q", n.WANIface)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\n", TableName, TableName)
	fmt.Fprintf(&b, "table inet %s {\n", TableName)

	rules := append([]proto.Rule(nil), st.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].Port < rules[j].Port })

	b.WriteString("\tchain prerouting {\n\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
	for _, r := range rules {
		for _, p := range protos(r.Proto) {
			if r.IPVer != "v6" && r.Target4 != "" {
				t, err := netip.ParseAddr(r.Target4)
				if err != nil || !t.Is4() {
					return "", fmt.Errorf("规则 %d 目标 IPv4 无效: %q", r.ID, r.Target4)
				}
				fmt.Fprintf(&b, "\t\tiifname %q meta nfproto ipv4 fib daddr type local %s dport %d counter dnat ip to %s:%d comment \"r%d\"\n",
					n.WANIface, p, r.Port, t, r.TargetPort, r.ID)
			}
			if r.IPVer != "v4" && r.Target6 != "" && n.CIDR6 != "" {
				t, err := netip.ParseAddr(r.Target6)
				if err != nil || !t.Is6() {
					return "", fmt.Errorf("规则 %d 目标 IPv6 无效: %q", r.ID, r.Target6)
				}
				fmt.Fprintf(&b, "\t\tiifname %q meta nfproto ipv6 fib daddr type local %s dport %d counter dnat ip6 to [%s]:%d comment \"r%d\"\n",
					n.WANIface, p, r.Port, t, r.TargetPort, r.ID)
			}
		}
	}
	b.WriteString("\t}\n")

	b.WriteString("\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	if n.CIDR4 != "" {
		p, err := netip.ParsePrefix(n.CIDR4)
		if err != nil || !p.Addr().Is4() {
			return "", fmt.Errorf("内网 IPv4 网段无效: %q", n.CIDR4)
		}
		fmt.Fprintf(&b, "\t\toifname %q ip saddr %s masquerade\n", n.WANIface, p.Masked())
	}
	if n.CIDR6 != "" {
		p, err := netip.ParsePrefix(n.CIDR6)
		if err != nil || !p.Addr().Is6() {
			return "", fmt.Errorf("内网 IPv6 网段无效: %q", n.CIDR6)
		}
		fmt.Fprintf(&b, "\t\toifname %q ip6 saddr %s masquerade\n", n.WANIface, p.Masked())
	}
	b.WriteString("\t}\n")

	// 流量统计：NAT 链只看到连接的首个包，字节数在 forward 链按连接跟踪统计
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n")
	for _, r := range rules {
		for _, p := range protos(r.Proto) {
			fmt.Fprintf(&b, "\t\tct status dnat ct original protocol %s ct original proto-dst %d ct direction original counter comment \"r%d-up\"\n", p, r.Port, r.ID)
			fmt.Fprintf(&b, "\t\tct status dnat ct original protocol %s ct original proto-dst %d ct direction reply counter comment \"r%d-down\"\n", p, r.Port, r.ID)
		}
	}
	b.WriteString("\t}\n}\n")
	return b.String(), nil
}

var hostNameRe = regexp.MustCompile(`[^A-Za-z0-9-]`)

// RenderHosts 生成 dnsmasq dhcp-hostsfile。
func RenderHosts(hosts []proto.DHCPHost) string {
	var b strings.Builder
	for _, h := range hosts {
		mac := strings.ToLower(h.MAC)
		if !macRe.MatchString(mac) || h.IP4 == "" {
			continue
		}
		line := mac + "," + h.IP4
		if h.IP6 != "" {
			line += ",[" + h.IP6 + "]"
		}
		if name := strings.Trim(hostNameRe.ReplaceAllString(h.Name, "-"), "-"); name != "" {
			line += "," + name
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

var macRe = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// Pool 计算网段内的地址范围：虚拟机分配池 .10-.99，DHCP 动态范围 .100-.250（按网段大小截断）。
type Pool struct{ Start, End netip.Addr }

func hostAt(p netip.Prefix, n int) (netip.Addr, bool) {
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	size := uint32(1) << (32 - p.Bits())
	if uint32(n) >= size-1 {
		return netip.Addr{}, false
	}
	v += uint32(n)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

func rangeIn(cidr string, from, to int) (Pool, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return Pool{}, fmt.Errorf("网段无效: %q", cidr)
	}
	s, ok := hostAt(p, from)
	if !ok {
		return Pool{}, fmt.Errorf("网段 %s 太小", cidr)
	}
	e, ok := hostAt(p, to)
	for !ok && to > from {
		to--
		e, ok = hostAt(p, to)
	}
	return Pool{s, e}, nil
}

func VMPool(cidr string) (Pool, error)    { return rangeIn(cidr, 10, 99) }
func DHCPRange(cidr string) (Pool, error) { return rangeIn(cidr, 100, 250) }

// Gateway 返回网段的 .1 地址（CIDR 形式）。
func Gateway(cidr string) (string, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", err
	}
	if p.Addr().Is4() {
		a, ok := hostAt(p, 1)
		if !ok {
			return "", fmt.Errorf("网段 %s 太小", cidr)
		}
		return fmt.Sprintf("%s/%d", a, p.Bits()), nil
	}
	a := p.Masked().Addr().As16()
	a[15] = 1
	return fmt.Sprintf("%s/%d", netip.AddrFrom16(a), p.Bits()), nil
}

// V6For 由 IPv4 主机号生成 IPv6 地址：172.16.0.10 → <prefix>::10。
func V6For(cidr6 string, ip4 string) string {
	if cidr6 == "" || ip4 == "" {
		return ""
	}
	p, err := netip.ParsePrefix(cidr6)
	a, err2 := netip.ParseAddr(ip4)
	if err != nil || err2 != nil || !a.Is4() {
		return ""
	}
	host := fmt.Sprint(a.As4()[3])
	base := strings.TrimSuffix(p.Masked().Addr().String(), "::")
	if strings.Contains(base, "::") {
		return ""
	}
	v, err := netip.ParseAddr(base + "::" + host)
	if err != nil || !p.Contains(v) {
		return ""
	}
	return v.String()
}

// RenderDnsmasq 生成 dnsmasq 配置（只提供 DHCP）。
func RenderDnsmasq(n proto.NetConfig) (string, error) {
	if !ValidIface(n.Bridge) {
		return "", fmt.Errorf("网桥名称无效: %q", n.Bridge)
	}
	p, err := netip.ParsePrefix(n.CIDR4)
	if err != nil {
		return "", fmt.Errorf("网段无效: %q", n.CIDR4)
	}
	r, err := DHCPRange(n.CIDR4)
	if err != nil {
		return "", err
	}
	gw, _ := Gateway(n.CIDR4)
	var b strings.Builder
	b.WriteString("# 由 PVE Forward 生成，请勿手动修改\n")
	fmt.Fprintf(&b, "interface=%s\nbind-dynamic\nexcept-interface=lo\nport=0\n", n.Bridge)
	fmt.Fprintf(&b, "dhcp-range=set:pvef,%s,%s,%s,12h\n", r.Start, r.End, maskString(p.Bits()))
	fmt.Fprintf(&b, "dhcp-option=tag:pvef,option:router,%s\n", strings.Split(gw, "/")[0])
	var dns4, dns6 []string
	for _, d := range n.DNS {
		if a, err := netip.ParseAddr(strings.TrimSpace(d)); err == nil {
			if a.Is4() {
				dns4 = append(dns4, a.String())
			} else {
				dns6 = append(dns6, "["+a.String()+"]")
			}
		}
	}
	if len(dns4) > 0 {
		fmt.Fprintf(&b, "dhcp-option=tag:pvef,option:dns-server,%s\n", strings.Join(dns4, ","))
	}
	if n.CIDR6 != "" {
		fmt.Fprintf(&b, "enable-ra\ndhcp-range=set:pvef6,::100,::250,constructor:%s,64,12h\n", n.Bridge)
		if len(dns6) > 0 {
			fmt.Fprintf(&b, "dhcp-option=tag:pvef6,option6:dns-server,%s\n", strings.Join(dns6, ","))
		}
	}
	fmt.Fprintf(&b, "dhcp-hostsfile=%s\n", HostsFile)
	return b.String(), nil
}

func maskString(bits int) string {
	m := uint32(0xffffffff) << (32 - bits)
	return fmt.Sprintf("%d.%d.%d.%d", byte(m>>24), byte(m>>16), byte(m>>8), byte(m))
}

// RenderBoot 生成开机脚本：开启转发、补充网桥地址（追加模式）、加载规则。
func RenderBoot(n proto.NetConfig) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n# 由 PVE Forward 生成，请勿手动修改\n")
	b.WriteString("sysctl -q -w net.ipv4.ip_forward=1\n")
	if n.CIDR6 != "" {
		b.WriteString("sysctl -q -w net.ipv6.conf.all.forwarding=1\n")
		// 开启转发后 PVE 公网网口仍需接收 RA，否则 IPv6 默认路由会丢失
		if ValidIface(n.WANIface) {
			fmt.Fprintf(&b, "sysctl -q -w net.ipv6.conf.%s.accept_ra=2\n", n.WANIface)
		}
	}
	if n.AddrMode == "append" && ValidIface(n.Bridge) {
		if n.GW4 != "" {
			fmt.Fprintf(&b, "ip addr replace %s dev %s\n", n.GW4, n.Bridge)
		}
	}
	if n.GW6 != "" && ValidIface(n.Bridge) && n.AddrMode != "new" && n.AddrMode != "replace" {
		fmt.Fprintf(&b, "ip -6 addr replace %s dev %s\n", n.GW6, n.Bridge)
	}
	fmt.Fprintf(&b, "[ -s %s ] && nft -f %s\n", RulesFile, RulesFile)
	b.WriteString("exit 0\n")
	return b.String()
}

const unitContent = `[Unit]
Description=PVE Forward port forwarding rules
After=network-online.target pve-firewall.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash ` + BootScript + `

[Install]
WantedBy=multi-user.target
`

func RenderSysctl(n proto.NetConfig) string {
	s := "net.ipv4.ip_forward=1\n"
	if n.CIDR6 != "" {
		s += "net.ipv6.conf.all.forwarding=1\n"
		if ValidIface(n.WANIface) {
			s += fmt.Sprintf("net.ipv6.conf.%s.accept_ra=2\n", n.WANIface)
		}
	}
	return s
}
