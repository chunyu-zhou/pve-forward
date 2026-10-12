package nodeops

import (
	"strings"
	"testing"
	"time"
)

func TestParseVMAddrs(t *testing.T) {
	s := map[string]string{
		"neigh": `[{"dst":"172.16.0.21","dev":"vmbr1","lladdr":"bc:24:11:00:00:01","state":["STALE"]},
			{"dst":"172.16.0.99","dev":"vmbr1","lladdr":"bc:24:11:00:00:02","state":["FAILED"]},
			{"dst":"192.168.1.5","dev":"vmbr1","lladdr":"bc:24:11:00:00:03","state":["REACHABLE"]},
			{"dst":"fe80::1","dev":"vmbr1","lladdr":"bc:24:11:00:00:01","state":["REACHABLE"]},
			{"dst":"fd00:16::21","dev":"vmbr1","lladdr":"BC:24:11:00:00:01","state":["REACHABLE"]},
			{"dst":"172.16.0.40","dev":"vmbr1","lladdr":"bc:24:11:00:00:04","state":["REACHABLE"]}]`,
		// dnsmasq 租约覆盖邻居表
		"leases": "1760000000 bc:24:11:00:00:04 172.16.0.41 web *\n",
		// ISC DHCP：同一地址后出现的块生效；free 的租约忽略
		"dhcpd": `lease 172.16.0.50 {
  binding state active;
  hardware ethernet bc:24:11:00:00:05;
}
lease 172.16.0.51 {
  binding state free;
  hardware ethernet bc:24:11:00:00:06;
}
`,
		"kea": "address,hwaddr,client_id,valid_lifetime,expire,subnet_id,fqdn_fwd,fqdn_rev,hostname,state,user_context\n" +
			"172.16.0.60,bc:24:11:00:00:07,,3600,1760000000,1,0,0,,0,\n" +
			"172.16.0.61,bc:24:11:00:00:08,,3600,1760000000,1,0,0,,2,\n",
	}
	a := parseVMAddrs(s, "172.16.0.0/24", "fd00:16::/64")
	want4 := map[string]string{
		"bc:24:11:00:00:01": "172.16.0.21", // 邻居表（STALE 仍可用）
		"bc:24:11:00:00:04": "172.16.0.41", // 租约优先于邻居表
		"bc:24:11:00:00:05": "172.16.0.50", // ISC DHCP
		"bc:24:11:00:00:07": "172.16.0.60", // Kea
	}
	for mac, ip := range want4 {
		if a.v4[mac] != ip {
			t.Errorf("v4[%s] = %q，期望 %q", mac, a.v4[mac], ip)
		}
	}
	for _, mac := range []string{"bc:24:11:00:00:02", "bc:24:11:00:00:03", "bc:24:11:00:00:06", "bc:24:11:00:00:08"} {
		if ip, ok := a.v4[mac]; ok {
			t.Errorf("%s 不应识别（失败条目、网段外、已释放的租约），得到 %s", mac, ip)
		}
	}
	if a.v6["bc:24:11:00:00:01"] != "fd00:16::21" {
		t.Errorf("v6 %v（应忽略 fe80:: 链路本地地址）", a.v6)
	}
}

// 采集结果中带上虚拟机当前地址：LXC 静态地址优先，其次租约 / 邻居表。
func TestCollectVMAddresses(t *testing.T) {
	st := testState()
	out := collectOut(1000, 100, 1000)
	out = strings.Replace(out, "##health", "##neigh\n"+
		`[{"dst":"172.16.0.33","dev":"vmbr1","lladdr":"bc:24:11:3a:01:9f","state":["REACHABLE"]}]`+"\n##health", 1)
	rep, _ := parseCollect(sections([]byte(out)), nil, time.Unix(1000, 0), st)
	if len(rep.VMs) != 1 || rep.VMs[0].IP4 != "172.16.0.33" {
		t.Fatalf("%+v", rep.VMs)
	}

	c := parseConfs("#lxc 102\nhostname: ct\nnet0: name=eth0,bridge=vmbr1,hwaddr=BC:24:11:7E:22:10,ip=172.16.0.8/24,gw=172.16.0.1,ip6=fd00:16::8/64,type=veth\n")
	if v4, v6 := staticAddrs(c[0], "vmbr1"); v4 != "172.16.0.8" || v6 != "fd00:16::8" {
		t.Fatalf("静态地址 %q %q", v4, v6)
	}
	if v4, _ := staticAddrs(c[0], "vmbr9"); v4 != "" {
		t.Fatal("其他网桥的网卡不应使用")
	}
	if !strings.Contains(collectScript(st), "ip -j neigh show dev vmbr1") {
		t.Fatal("采集脚本应读取内网网桥的邻居表")
	}
}
