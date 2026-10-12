package nodeops

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"pveforward/internal/proto"
)

func testState() *proto.DesiredState {
	return &proto.DesiredState{
		Version: 3,
		Net: proto.NetConfig{WANIface: "enp3s0f1", Bridge: "vmbr1", CIDR4: "172.16.0.0/24", CIDR6: "fd00:16::/64",
			GW4: "172.16.0.1/24", GW6: "fd00:16::1/64", AddrMode: "append", DHCP: "managed", DNS: []string{"223.5.5.5", "2400:3200::1"}},
		Rules: []proto.Rule{
			{ID: 2, Port: 20001, Proto: "both", IPVer: "both", Target4: "172.16.0.10", Target6: "fd00:16::10", TargetPort: 443},
			{ID: 1, Port: 20000, Proto: "tcp", IPVer: "v4", Target4: "172.16.0.10", TargetPort: 22},
		},
		Hosts: []proto.DHCPHost{{MAC: "BC:24:11:3A:01:9F", IP4: "172.16.0.10", IP6: "fd00:16::10", Name: "web 01"}, {MAC: "bad", IP4: "1.1.1.1"}},
	}
}

func TestRenderNft(t *testing.T) {
	out, err := RenderNft(testState())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table inet pve_forward\ndelete table inet pve_forward\n",
		`iifname "enp3s0f1" meta nfproto ipv4 fib daddr type local tcp dport 20000 counter dnat ip to 172.16.0.10:22 comment "r1"`,
		`udp dport 20001 counter dnat ip to 172.16.0.10:443`,
		`meta nfproto ipv6 fib daddr type local tcp dport 20001 counter dnat ip6 to [fd00:16::10]:443 comment "r2"`,
		`oifname "enp3s0f1" ip saddr 172.16.0.0/24 masquerade`,
		`oifname "enp3s0f1" ip6 saddr fd00:16::/64 masquerade`,
		`ct status dnat ct original protocol udp ct original proto-dst 20001 ct direction reply counter comment "r2-down"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少: %s\n---\n%s", want, out)
		}
	}
	// 端口排序：20000 在 20001 之前
	if strings.Index(out, "dport 20000") > strings.Index(out, "dport 20001") {
		t.Error("规则未按端口排序")
	}
	bad := testState()
	bad.Net.WANIface = `eth0"; flush ruleset; "`
	if _, err := RenderNft(bad); err == nil {
		t.Error("非法网口名应报错")
	}
	bad = testState()
	bad.Rules[0].Target4 = "172.16.0.10; drop"
	if _, err := RenderNft(bad); err == nil {
		t.Error("非法目标地址应报错")
	}
}

func TestRenderHostsAndDnsmasq(t *testing.T) {
	h := RenderHosts(testState().Hosts)
	if h != "bc:24:11:3a:01:9f,172.16.0.10,[fd00:16::10],web-01\n" {
		t.Errorf("hosts = %q", h)
	}
	c, err := RenderDnsmasq(testState().Net)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"interface=vmbr1", "port=0", "dhcp-range=set:pvef,172.16.0.100,172.16.0.250,255.255.255.0,12h",
		"option:router,172.16.0.1", "option:dns-server,223.5.5.5", "enable-ra", "option6:dns-server,[2400:3200::1]", "dhcp-hostsfile=" + HostsFile} {
		if !strings.Contains(c, want) {
			t.Errorf("dnsmasq 缺少 %s\n%s", want, c)
		}
	}
}

func TestPoolsAndV6(t *testing.T) {
	p, _ := VMPool("10.0.0.0/24")
	if p.Start.String() != "10.0.0.10" || p.End.String() != "10.0.0.99" {
		t.Errorf("pool %v", p)
	}
	p, _ = DHCPRange("192.168.5.0/25")
	if p.Start.String() != "192.168.5.100" || p.End.String() != "192.168.5.126" {
		t.Errorf("dhcp /25 %v", p)
	}
	if g, _ := Gateway("172.17.0.0/24"); g != "172.17.0.1/24" {
		t.Errorf("gw %s", g)
	}
	if g, _ := Gateway("fd00:16::/64"); g != "fd00:16::1/64" {
		t.Errorf("gw6 %s", g)
	}
	if v := V6For("fd00:16::/64", "172.16.0.10"); v != "fd00:16::10" {
		t.Errorf("v6 %s", v)
	}
}

func TestBoot(t *testing.T) {
	b := RenderBoot(testState().Net)
	for _, want := range []string{"net.ipv4.ip_forward=1", "ip addr replace 172.16.0.1/24 dev vmbr1", "ip -6 addr replace fd00:16::1/64 dev vmbr1", "nft -f " + RulesFile, "accept_ra=2"} {
		if !strings.Contains(b, want) {
			t.Errorf("boot 缺少 %s\n%s", want, b)
		}
	}
}

const detectOut = `##hostname
pve-hk01
##uid
0
##pveversion
pve-manager/8.2.4/faa83925c9641325 (running kernel: 6.8.12-1-pve)
##nft
nftables v1.0.6 (Lester Gooch #5)
##addr
[{"ifindex":1,"ifname":"lo","flags":["LOOPBACK","UP","LOWER_UP"],"operstate":"UNKNOWN","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8,"scope":"host"}]},{"ifindex":2,"ifname":"enp3s0f1","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"operstate":"UP","addr_info":[{"family":"inet","local":"203.0.113.10","prefixlen":24,"scope":"global"},{"family":"inet6","local":"2001:db8:10::10","prefixlen":64,"scope":"global"},{"family":"inet6","local":"fe80::1","prefixlen":64,"scope":"link"}]},{"ifindex":3,"ifname":"enp3s0f0","flags":["BROADCAST","MULTICAST"],"operstate":"DOWN","addr_info":[]},{"ifindex":4,"ifname":"vmbr1","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"operstate":"UP","addr_info":[{"family":"inet","local":"10.0.0.1","prefixlen":24,"scope":"global"}]},{"ifindex":9,"ifname":"tap101i0","flags":["UP"],"operstate":"UNKNOWN","addr_info":[]}]
##link
[{"ifname":"enp3s0f1"},{"ifname":"enp3s0f0"},{"ifname":"vmbr1","linkinfo":{"info_kind":"bridge"}},{"ifname":"tap101i0","master":"vmbr1","linkinfo":{"info_kind":"tun"}}]
##route4
[{"dst":"default","gateway":"203.0.113.1","dev":"enp3s0f1"}]
##route6
[]
##phys
enp3s0f1 1000
enp3s0f0 -1
##dhcp
dnsmasq inactive
isc-dhcp-server inactive
kea-dhcp4-server inactive
##ours
##confs
#qemu 101
name: web01
cores: 4
net0: virtio=BC:24:11:3A:01:9F,bridge=vmbr1,firewall=1
#lxc 102
hostname: vpn01
net0: name=eth0,bridge=vmbr1,hwaddr=BC:24:11:7E:22:10,ip=dhcp,type=veth
##pub4

##pub6

`

func TestParseDetect(t *testing.T) {
	d, err := parseDetect(sections([]byte(detectOut)))
	if err != nil {
		t.Fatal(err)
	}
	if d.Hostname != "pve-hk01" || d.PVEVersion != "8.2.4" || !d.Root || d.NFTVersion == "" {
		t.Errorf("基本信息 %+v", d)
	}
	if d.DefaultIface != "enp3s0f1" || d.PublicIP4 != "203.0.113.10" || d.PublicIP6 != "2001:db8:10::10" {
		t.Errorf("公网 %s %s %s", d.DefaultIface, d.PublicIP4, d.PublicIP6)
	}
	if len(d.Bridges) != 1 || d.Bridges[0].Name != "vmbr1" || d.Bridges[0].CIDRs[0] != "10.0.0.1/24" || d.Bridges[0].VMs != 2 {
		t.Errorf("网桥 %+v", d.Bridges)
	}
	if d.VMCount != 2 || d.DHCP != "" {
		t.Errorf("vm=%d dhcp=%q", d.VMCount, d.DHCP)
	}
	names := []string{}
	for _, n := range d.NICs {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "enp3s0f1,enp3s0f0,vmbr1" || d.NICs[1].IP4 == nil {
		t.Errorf("网口 %v", names)
	}
}

func collectOut(cpuTotal, diskIO, netRx int) string {
	return strings.NewReplacer("CPU", itoa(cpuTotal), "DIO", itoa(diskIO), "RX", itoa(netRx)).Replace(`##hostname
pve-hk01
##stat
cpu  CPU 0 0 0 0 0 0 0 0 0
32
##mem
MemTotal:       131072000 kB
MemAvailable:    31072000 kB
##load
3.12 2.80 2.65 2/1000 12345
##uptime
3628800.12 1000.0
##diskstats
 259       0 nvme0n1 DIO 0 1000 0 DIO 0 2000 0 0 DIO 0
   8       1 sda1 5 0 5 0 5 0 5 0 0 5 0
##lsblk
{"blockdevices":[{"name":"nvme0n1","model":"Samsung PM9A3","rota":false,"tran":"nvme"},{"name":"sda","model":"WD 8TB","rota":"1","tran":"sata"}]}
##netdev
enp3s0f1: RX 100 0 0 0 0 0 0 RX 100 0 0 0 0 0 0
##storage
[{"storage":"local-lvm","total":1000,"used":400,"active":1,"shared":0},{"storage":"nfs","total":9000,"used":1,"active":1,"shared":1}]
##resources
[{"id":"qemu/101","type":"qemu","vmid":101,"name":"web01","node":"pve-hk01","status":"running","cpu":0.37,"maxcpu":4,"mem":5000,"maxmem":8000,"disk":0,"maxdisk":80000,"netin":RX,"netout":RX,"diskread":RX,"diskwrite":0,"uptime":100},{"id":"qemu/900","type":"qemu","vmid":900,"name":"tpl","node":"pve-hk01","status":"stopped","template":1},{"id":"qemu/500","type":"qemu","vmid":500,"name":"other","node":"pve-b","status":"running"}]
##confs
#qemu 101
name: web01
net0: virtio=BC:24:11:3A:01:9F,bridge=vmbr1
net1: virtio=BC:24:11:3A:01:A0,bridge=vmbr0
#qemu 900
name: tpl
##cgio
#101
259:0 rbytes=1 wbytes=1 rios=DIO wios=DIO dbytes=0 dios=0
some avg10=12.50 avg60=1.00 avg300=0.00 total=1
##nft
{"nftables":[{"metainfo":{}},{"table":{"family":"inet","name":"pve_forward"}},{"rule":{"chain":"forward","comment":"r1-up","expr":[{"counter":{"packets":1,"bytes":1000}}]}},{"rule":{"chain":"forward","comment":"r1-down","expr":[{"counter":{"packets":1,"bytes":5000}}]}},{"rule":{"chain":"forward","comment":"r1-up","expr":[{"counter":{"packets":1,"bytes":24}}]}}]}
##listen
0.0.0.0:22
[::]:8006
127.0.0.1:85
##conntrack
20000 3
##health
1 ok
`)
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestParseCollect(t *testing.T) {
	st := testState()
	st.Rules = append(st.Rules, proto.Rule{ID: 3, Port: 22, Proto: "tcp", IPVer: "v4", Target4: "172.16.0.11", TargetPort: 22})
	t0 := time.Unix(1000, 0)
	_, s1 := parseCollect(sections([]byte(collectOut(1000, 100, 1000))), nil, t0, st)
	rep, _ := parseCollect(sections([]byte(collectOut(2000, 200, 11000))), s1, t0.Add(10*time.Second), st)

	if rep.Node.Cores != 32 || rep.Node.MemTotal != 131072000*1024 || rep.Node.MemUsed != 100000000*1024 {
		t.Errorf("内存/核数 %+v", rep.Node)
	}
	if rep.Node.DiskTotal != 1000 || rep.Node.DiskUsed != 400 {
		t.Errorf("存储 %+v", rep.Node)
	}
	if rep.Node.NetIn != 1000 { // (11000-1000)/10
		t.Errorf("网络 %v", rep.Node.NetIn)
	}
	if len(rep.Disks) != 1 || rep.Disks[0].Kind != "NVMe" || rep.Disks[0].RIOPS != 10 || rep.Disks[0].Util != 1 {
		t.Errorf("磁盘 %+v", rep.Disks)
	}
	if len(rep.VMs) != 1 || rep.VMs[0].VMID != 101 || rep.VMs[0].MAC != "bc:24:11:3a:01:9f" || rep.VMs[0].CPU != 37 {
		t.Fatalf("虚拟机 %+v", rep.VMs)
	}
	if rep.VMs[0].IOPS != 20 || rep.VMs[0].IOPressure != 12.5 || rep.VMs[0].NetIn != 1000 {
		t.Errorf("虚拟机 IO %+v", rep.VMs[0])
	}
	if c := rep.Counters[1]; c.Up != 1024 || c.Down != 5000 || !rep.Loaded {
		t.Errorf("计数器 %+v", rep.Counters)
	}
	if rep.Conns[1] != 3 {
		t.Errorf("连接数 %v", rep.Conns)
	}
	if rep.Health[1] != "" || !strings.Contains(rep.Health[3], "端口冲突") {
		t.Errorf("健康 %v", rep.Health)
	}
}

// pvesh 的虚拟机计数器约 10 秒更新一次：两次采集之间未更新时沿用上次的速率，而不是变为 0。
func TestVMRateKeepsLastWhenCountersStale(t *testing.T) {
	st := testState()
	t0 := time.Unix(1000, 0)
	_, s1 := parseCollect(sections([]byte(collectOut(1000, 100, 1000))), nil, t0, st)
	rep, s2 := parseCollect(sections([]byte(collectOut(2000, 200, 11000))), s1, t0.Add(10*time.Second), st)
	if rep.VMs[0].NetIn != 1000 {
		t.Fatalf("首次速率 %v", rep.VMs[0].NetIn)
	}
	// 3 秒后计数器未变：沿用 1000，而不是 0
	rep, s3 := parseCollect(sections([]byte(collectOut(3000, 300, 11000))), s2, t0.Add(13*time.Second), st)
	if rep.VMs[0].NetIn != 1000 {
		t.Fatalf("计数器未更新时 %v", rep.VMs[0].NetIn)
	}
	// 计数器更新：按距上次真正变化的时间计算（10 秒前的基准）
	rep, _ = parseCollect(sections([]byte(collectOut(4000, 400, 31000))), s3, t0.Add(20*time.Second), st)
	if rep.VMs[0].NetIn != 2000 {
		t.Fatalf("更新后 %v", rep.VMs[0].NetIn)
	}
	// 长时间未变化（虚拟机确实没有流量）：速率归零
	rep, _ = parseCollect(sections([]byte(collectOut(3000, 300, 11000))), s2, t0.Add(50*time.Second), st)
	if rep.VMs[0].NetIn != 0 {
		t.Fatalf("长时间未变化 %v", rep.VMs[0].NetIn)
	}
}
