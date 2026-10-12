package nodeops

import (
	"bufio"
	"encoding/json"
	"net/netip"
	"regexp"
	"strings"
)

// 识别虚拟机当前实际使用的内网地址，使已有虚拟机沿用原来的 IP，而不是重新分配。
// 来源（优先级从高到低）：LXC 配置中的静态地址、DHCP 租约（dnsmasq / ISC DHCP / Kea）、宿主机网桥上的邻居表。

// vmIPScript 输出识别所需的数据；bridge 为内网网桥（已校验）。
func vmIPScript(bridge string) string {
	return `echo '##neigh'; ip -j neigh show dev ` + bridge + ` 2>/dev/null
echo '##leases'; cat /var/lib/misc/dnsmasq.leases 2>/dev/null
echo '##dhcpd'; tail -c 262144 /var/lib/dhcp/dhcpd.leases 2>/dev/null
echo '##kea'; tail -n 4000 /var/lib/kea/kea-leases4.csv 2>/dev/null
`
}

var (
	lxcIP4Re = regexp.MustCompile(`(?:^|,)ip=([0-9.]+)/\d+`)
	lxcIP6Re = regexp.MustCompile(`(?:^|,)ip6=([0-9a-fA-F:]+)/\d+`)
)

// vmAddrs 为按 MAC 地址（小写）索引的地址。
type vmAddrs struct{ v4, v6 map[string]string }

// parseVMAddrs 从租约与邻居表中得到 MAC → 地址；cidr4 / cidr6 不为空时只接受内网网段内的地址。
func parseVMAddrs(s map[string]string, cidr4, cidr6 string) vmAddrs {
	res := vmAddrs{v4: map[string]string{}, v6: map[string]string{}}
	p4, err4 := netip.ParsePrefix(cidr4)
	p6, err6 := netip.ParsePrefix(cidr6)
	accept := func(ip string) (netip.Addr, bool) {
		a, err := netip.ParseAddr(ip)
		if err != nil || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
			return a, false
		}
		if a.Is4() && err4 == nil && !p4.Contains(a) {
			return a, false
		}
		if a.Is6() && (err6 != nil || !p6.Contains(a)) && cidr6 != "" {
			return a, false
		}
		return a, true
	}
	set := func(mac, ip string, override bool) {
		mac = strings.ToLower(strings.TrimSpace(mac))
		if !macRe.MatchString(mac) {
			return
		}
		a, ok := accept(strings.TrimSpace(ip))
		if !ok {
			return
		}
		m := res.v6
		if a.Is4() {
			m = res.v4
		}
		if _, exists := m[mac]; !exists || override {
			m[mac] = a.String()
		}
	}

	// 邻居表（优先级最低，先写入，后面的租约覆盖）
	var neigh []struct {
		Dst    string   `json:"dst"`
		LLAddr string   `json:"lladdr"`
		State  []string `json:"state"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(s["neigh"])), &neigh)
	for _, n := range neigh {
		failed := false
		for _, st := range n.State {
			if st == "FAILED" || st == "INCOMPLETE" {
				failed = true
			}
		}
		if !failed {
			set(n.LLAddr, n.Dst, false)
		}
	}

	// ISC DHCP：lease 块按时间追加，后出现的覆盖前面的
	var ip, mac, state string
	sc := bufio.NewScanner(strings.NewReader(s["dhcpd"]))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(sc.Text()), ";"))
		switch {
		case len(f) >= 2 && f[0] == "lease":
			ip, mac, state = f[1], "", ""
		case len(f) >= 3 && f[0] == "hardware" && f[1] == "ethernet":
			mac = f[2]
		case len(f) >= 3 && f[0] == "binding" && f[1] == "state":
			state = f[2]
		case len(f) == 1 && f[0] == "}":
			if ip != "" && mac != "" && (state == "" || state == "active") {
				set(mac, ip, true)
			}
			ip = ""
		}
	}
	// Kea：address,hwaddr,...,state（第 10 列，0 为有效）
	for _, l := range strings.Split(s["kea"], "\n") {
		f := strings.Split(strings.TrimSpace(l), ",")
		if len(f) >= 10 && f[0] != "address" && strings.TrimSpace(f[9]) == "0" {
			set(f[1], f[0], true)
		}
	}
	// dnsmasq：expiry mac ip hostname clientid
	for _, l := range strings.Split(s["leases"], "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 {
			set(f[1], f[2], true)
		}
	}
	return res
}

// staticAddrs 返回 LXC 配置中接在 bridge 上的网卡的静态地址。
func staticAddrs(c vmConf, bridge string) (v4, v6 string) {
	for _, n := range c.Nets {
		if bridge == "" || n.Bridge == bridge {
			return n.IP4, n.IP6
		}
	}
	return "", ""
}
