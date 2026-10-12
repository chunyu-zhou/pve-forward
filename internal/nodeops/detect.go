package nodeops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"pveforward/internal/proto"
)

// 读取 PVE 虚拟机配置（只取主配置，忽略快照段），输出 "#qemu 101" / "#lxc 102" 分隔
const vmConfScript = `for f in /etc/pve/qemu-server/*.conf; do [ -f "$f" ] || continue; echo "#qemu $(basename "$f" .conf)"; sed -n '/^\[/q;p' "$f"; done
for f in /etc/pve/lxc/*.conf; do [ -f "$f" ] || continue; echo "#lxc $(basename "$f" .conf)"; sed -n '/^\[/q;p' "$f"; done
`

const detectScript = `export LC_ALL=C
echo '##hostname'; hostname -s
echo '##uid'; id -u
echo '##pveversion'; pveversion 2>/dev/null || true
echo '##nft'; nft --version 2>/dev/null || true
echo '##addr'; ip -j addr show 2>/dev/null
echo '##link'; ip -j -d link show 2>/dev/null
echo '##route4'; ip -j route show default 2>/dev/null
echo '##route6'; ip -j -6 route show default 2>/dev/null
echo '##phys'; for d in /sys/class/net/*; do [ -e "$d/device" ] && echo "$(basename "$d") $(cat "$d/speed" 2>/dev/null)"; done
echo '##dhcp'; for s in dnsmasq isc-dhcp-server kea-dhcp4-server; do echo "$s $(systemctl is-active "$s" 2>/dev/null)"; done
echo '##ours'; [ -f ` + DnsmasqConf + ` ] && echo yes
echo '##confs'
` + vmConfScript + `
echo '##pub4'; curl -4 -fsS --max-time 3 https://api.ipify.org 2>/dev/null || true
echo
echo '##pub6'; curl -6 -fsS --max-time 3 https://api64.ipify.org 2>/dev/null || true
echo
`

type ipAddr struct {
	IfName   string   `json:"ifname"`
	Flags    []string `json:"flags"`
	Oper     string   `json:"operstate"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
	} `json:"addr_info"`
}

type ipLink struct {
	IfName   string `json:"ifname"`
	Master   string `json:"master"`
	LinkInfo struct {
		InfoKind string `json:"info_kind"`
	} `json:"linkinfo"`
}

type ipRoute struct {
	Dev string `json:"dev"`
}

// Detect 检测节点环境。
func Detect(ctx context.Context, ex Executor) (*proto.Detect, error) {
	out, err := ex.Run(ctx, detectScript)
	if err != nil {
		return nil, err
	}
	return parseDetect(sections(out))
}

var pveVerRe = regexp.MustCompile(`pve-manager/([0-9][0-9.]*)`)

func parseDetect(s map[string]string) (*proto.Detect, error) {
	d := &proto.Detect{Hostname: strings.TrimSpace(s["hostname"])}
	d.PVENode = d.Hostname
	d.Root = strings.TrimSpace(s["uid"]) == "0"
	if m := pveVerRe.FindStringSubmatch(s["pveversion"]); m != nil {
		d.PVEVersion = m[1]
	}
	d.NFTVersion = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s["nft"]), "nftables"))

	var addrs []ipAddr
	if err := json.Unmarshal([]byte(strings.TrimSpace(s["addr"])), &addrs); err != nil {
		return nil, fmt.Errorf("解析 ip addr 失败: %w", err)
	}
	var links []ipLink
	_ = json.Unmarshal([]byte(strings.TrimSpace(s["link"])), &links)
	var r4, r6 []ipRoute
	_ = json.Unmarshal([]byte(strings.TrimSpace(s["route4"])), &r4)
	_ = json.Unmarshal([]byte(strings.TrimSpace(s["route6"])), &r6)
	if len(r4) > 0 {
		d.DefaultIface = r4[0].Dev
	} else if len(r6) > 0 {
		d.DefaultIface = r6[0].Dev
	}

	phys := map[string]int{}
	for _, l := range strings.Split(s["phys"], "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		sp := 0
		if len(f) > 1 {
			sp, _ = strconv.Atoi(f[1])
		}
		if sp < 0 {
			sp = 0
		}
		phys[f[0]] = sp
	}
	kind := map[string]string{}
	ports := map[string][]string{}
	master := map[string]string{}
	for _, l := range links {
		kind[l.IfName] = l.LinkInfo.InfoKind
		master[l.IfName] = l.Master
		if l.Master != "" {
			ports[l.Master] = append(ports[l.Master], l.IfName)
		}
	}

	bridgeVMs := countBridgeVMs(s["confs"])
	d.VMCount = countVMs(s["confs"])

	for _, a := range addrs {
		if a.IfName == "lo" {
			continue
		}
		v4, v6 := []string{}, []string{}
		for _, ai := range a.AddrInfo {
			c := fmt.Sprintf("%s/%d", ai.Local, ai.PrefixLen)
			if ai.Family == "inet" {
				v4 = append(v4, c)
			} else if ai.Family == "inet6" && ai.Scope == "global" {
				v6 = append(v6, c)
			}
		}
		up := a.Oper == "UP" || (a.Oper == "UNKNOWN" && contains(a.Flags, "LOWER_UP"))
		if kind[a.IfName] == "bridge" {
			pp := []string{} // 非 nil，序列化为 []
			for _, p := range ports[a.IfName] {
				if _, ok := phys[p]; ok {
					pp = append(pp, p)
				}
			}
			d.Bridges = append(d.Bridges, proto.Bridge{Name: a.IfName, CIDRs: v4, Ports: pp, VMs: bridgeVMs[a.IfName]})
		}
		_, isPhys := phys[a.IfName]
		// 公网网口候选：物理网口或带地址的网桥 / 绑定 / VLAN；已加入网桥或绑定的端口不作为候选（流量从上层接口进入）
		if master[a.IfName] != "" {
			continue
		}
		if isPhys || len(v4)+len(v6) > 0 {
			if strings.HasPrefix(a.IfName, "tap") || strings.HasPrefix(a.IfName, "veth") || strings.HasPrefix(a.IfName, "fw") {
				continue
			}
			d.NICs = append(d.NICs, proto.NIC{Name: a.IfName, IP4: v4, IP6: v6, Up: up, Default: a.IfName == d.DefaultIface, SpeedMbps: phys[a.IfName]})
		}
	}
	for _, n := range d.NICs {
		if !n.Default {
			continue
		}
		if len(n.IP4) > 0 {
			d.PublicIP4 = strings.Split(n.IP4[0], "/")[0]
		}
		if len(n.IP6) > 0 {
			d.PublicIP6 = strings.Split(n.IP6[0], "/")[0]
		}
	}
	// 网口上是内网地址（云厂商 NAT）时，改用出口探测结果
	if a, err := netip.ParseAddr(d.PublicIP4); err != nil || a.IsPrivate() || isCGNAT(a) {
		if p, err := netip.ParseAddr(strings.TrimSpace(s["pub4"])); err == nil && p.Is4() {
			d.PublicIP4 = p.String()
		}
	}
	if d.PublicIP6 == "" {
		if p, err := netip.ParseAddr(strings.TrimSpace(s["pub6"])); err == nil && p.Is6() {
			d.PublicIP6 = p.String()
		}
	}

	if strings.TrimSpace(s["ours"]) != "yes" {
		for _, l := range strings.Split(s["dhcp"], "\n") {
			f := strings.Fields(l)
			if len(f) == 2 && f[1] == "active" {
				d.DHCP = f[0]
				break
			}
		}
	}
	return d, nil
}

func isCGNAT(a netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

var bridgeOptRe = regexp.MustCompile(`(?:^|,)bridge=([A-Za-z0-9_.-]+)`)

// vmConf 为解析后的虚拟机配置
type vmConf struct {
	Type  string
	VMID  int
	Name  string
	Cores int
	Nets  []vmNet
}

type vmNet struct{ MAC, Bridge string }

var qemuMacRe = regexp.MustCompile(`(?:^|,)(?:virtio|e1000e?|rtl8139|vmxnet3|ne2k_pci|pcnet|i82551|i82557b|i82559er|ne2k_isa)=([0-9A-Fa-f:]{17})`)
var lxcMacRe = regexp.MustCompile(`(?:^|,)hwaddr=([0-9A-Fa-f:]{17})`)
var netKeyRe = regexp.MustCompile(`^net\d+:\s*(.*)$`)

func parseConfs(text string) []vmConf {
	var res []vmConf
	var cur *vmConf
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#qemu ") || strings.HasPrefix(line, "#lxc ") {
			f := strings.Fields(line)
			id, _ := strconv.Atoi(f[1])
			res = append(res, vmConf{Type: strings.TrimPrefix(f[0], "#"), VMID: id})
			cur = &res[len(res)-1]
			continue
		}
		if cur == nil || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			cur.Name = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "hostname:"); ok && cur.Name == "" {
			cur.Name = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "cores:"); ok {
			cur.Cores, _ = strconv.Atoi(strings.TrimSpace(v))
		} else if m := netKeyRe.FindStringSubmatch(line); m != nil {
			var n vmNet
			if b := bridgeOptRe.FindStringSubmatch(m[1]); b != nil {
				n.Bridge = b[1]
			}
			re := qemuMacRe
			if cur.Type == "lxc" {
				re = lxcMacRe
			}
			if mm := re.FindStringSubmatch(m[1]); mm != nil {
				n.MAC = strings.ToLower(mm[1])
			}
			cur.Nets = append(cur.Nets, n)
		}
	}
	return res
}

func countBridgeVMs(text string) map[string]int {
	res := map[string]int{}
	for _, c := range parseConfs(text) {
		seen := map[string]bool{}
		for _, n := range c.Nets {
			if n.Bridge != "" && !seen[n.Bridge] {
				seen[n.Bridge] = true
				res[n.Bridge]++
			}
		}
	}
	return res
}

func countVMs(text string) int { return len(parseConfs(text)) }
