// devnode 是开发用的 PVE 节点模拟器：以 Agent 方式接入控制端，对下发的脚本返回仿真的命令输出。
// 仅用于在没有真实 PVE 的环境中调试界面，不随正式版本发布。
//
//	go run ./tools/devnode --server http://127.0.0.1:5080 --token XXXX-XXXX-XXXX --name pve-sim
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"pveforward/internal/manager"
	"pveforward/internal/proto"
)

type vm struct {
	id                int
	typ, name, status string
	mac               string
	cores             int
	mem, disk         uint64
	load              float64 // 负载基准 0-1
}

type sim struct {
	mu          sync.Mutex
	name        string
	start       time.Time
	lastCollect time.Time
	vms         []vm
	rules       map[int64]bool
	counters    map[int64][2]uint64
	ruleRate    map[int64]float64
	cpu         [3]uint64 // total, iowait, idle
	disk        [3][5]uint64
	diskRW      [3]uint64
	net         [2]uint64
	vmc         map[int][5]uint64
	dnsmasq     bool
	busyDisk    bool
	// Web 转发：已下发的站点（位置 → 站点 ID）与 OpenResty 统计
	webSites map[string][]int64
	webStats map[int64][3]uint64
}

var b64Re = regexp.MustCompile(`printf '%s' '([A-Za-z0-9+/=]*)' \| base64 -d > '([^']+)'`)
var ruleIDRe = regexp.MustCompile(`comment "r(\d+)-up"`)

var (
	sshWrapRe  = regexp.MustCompile(`printf '%s' '([A-Za-z0-9+/=]+)' \| base64 -d \| ssh [^\n]* [a-z_][a-z0-9_.-]*@([0-9.]+) `)
	siteConfRe = regexp.MustCompile(`base64 -d > "\$D\.tmp/sites/(\d+)\.conf"`)
	siteChkRe  = regexp.MustCompile(`echo 's(\d+) ok'`)
)

// web 模拟 Web 转发脚本：环境检测、安装 OpenResty、下发配置、移除。入口虚拟机的脚本经 ssh 包装。
func (s *sim) web(script string) ([]byte, bool, error) {
	loc := "host"
	if m := sshWrapRe.FindStringSubmatch(script); m != nil {
		inner, _ := base64.StdEncoding.DecodeString(m[1])
		script, loc = string(inner), "vm:"+m[2]
	}
	const version = "##version\nnginx version: openresty/1.27.1.2\n"
	switch {
	case strings.Contains(script, "##managed"):
		time.Sleep(1200 * time.Millisecond)
		return []byte("##uid\n0\n##os\nDebian GNU/Linux 12 (bookworm)|debian|\n##arch\nx86_64\n##apt\nyes\n##systemd\nyes\n##openresty\n##managed\n" +
			"##listen\n0.0.0.0:22 users:((\"sshd\",pid=1,fd=3))\n##repo\n200"), true, nil
	case strings.Contains(script, "openresty.list"):
		time.Sleep(3 * time.Second)
		log.Printf("安装 OpenResty（%s）", loc)
		return []byte(version), true, nil
	case strings.Contains(script, "-t -q -c"):
		var ids []int64
		for _, m := range siteConfRe.FindAllStringSubmatch(script, -1) {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			ids = append(ids, id)
		}
		s.webSites[loc] = ids
		log.Printf("下发 Web 配置（%s）：%d 个站点", loc, len(ids))
		time.Sleep(500 * time.Millisecond)
		return []byte(version), true, nil
	case strings.Contains(script, "openresty-installed") && strings.Contains(script, "pvef-orig"):
		delete(s.webSites, loc)
		log.Printf("移除 Web 配置（%s）", loc)
		return nil, true, nil
	}
	return nil, false, nil
}

// webCollect 为采集补充站点健康检查与 OpenResty 统计（请求数与流量随时间增长）。
func (s *sim) webCollect(script string) string {
	var b strings.Builder
	for _, m := range siteChkRe.FindAllStringSubmatch(script, -1) {
		b.WriteString("s" + m[1] + " ok\n")
	}
	if strings.Contains(script, "pvef-stats") {
		b.WriteString("##web\n")
		for _, ids := range s.webSites {
			for _, id := range ids {
				c := s.webStats[id]
				n := uint64(5 + rand.Intn(40))
				c = [3]uint64{c[0] + n, c[1] + n*uint64(400+rand.Intn(300)), c[2] + n*uint64(8000+rand.Intn(40000))}
				s.webStats[id] = c
				fmt.Fprintf(&b, "r%d %d\nu%d %d\nd%d %d\n", id, c[0], id, c[1], id, c[2])
			}
		}
	}
	return b.String()
}

func (s *sim) Run(_ context.Context, script string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if out, ok, err := s.web(script); ok {
		return out, err
	}
	switch {
	case strings.Contains(script, "##pveversion"):
		return []byte(s.detect()), nil
	case strings.Contains(script, "##before"):
		before := s.nftJSON()
		for _, m := range b64Re.FindAllStringSubmatch(script, -1) {
			if strings.Contains(m[2], "rules.nft") {
				b, _ := base64.StdEncoding.DecodeString(m[1])
				s.rules = map[int64]bool{}
				for _, r := range ruleIDRe.FindAllStringSubmatch(string(b), -1) {
					id, _ := strconv.ParseInt(r[1], 10, 64)
					s.rules[id] = true
					if _, ok := s.ruleRate[id]; !ok {
						s.ruleRate[id] = math.Pow(10, 3+rand.Float64()*3.5) // 1 KB/s – 3 MB/s
					}
				}
			}
		}
		s.counters = map[int64][2]uint64{}
		log.Printf("应用规则：%d 条", len(s.rules))
		return []byte("##before\n" + before + "\n"), nil
	case strings.Contains(script, "##diskstats"):
		return []byte(s.collect() + s.webCollect(script)), nil
	case strings.Contains(script, "rrddata"):
		return []byte(s.rrd(script)), nil
	case strings.Contains(script, "nft delete table"):
		s.rules = map[int64]bool{}
		return nil, nil
	default:
		if strings.Contains(script, "dnsmasq") {
			s.dnsmasq = true
		}
		time.Sleep(400 * time.Millisecond)
		return nil, nil
	}
}

func (s *sim) detect() string {
	var confs strings.Builder
	for _, v := range s.vms {
		fmt.Fprintf(&confs, "#%s %d\n", v.typ, v.id)
		if v.typ == "qemu" {
			fmt.Fprintf(&confs, "name: %s\ncores: %d\nnet0: virtio=%s,bridge=vmbr1\n", v.name, v.cores, strings.ToUpper(v.mac))
		} else {
			fmt.Fprintf(&confs, "hostname: %s\ncores: %d\nnet0: name=eth0,bridge=vmbr1,hwaddr=%s,ip=dhcp\n", v.name, v.cores, strings.ToUpper(v.mac))
		}
	}
	ours := ""
	if s.dnsmasq {
		ours = "yes"
	}
	return fmt.Sprintf(`##hostname
%[1]s
##uid
0
##pveversion
pve-manager/8.3.1/fb48e850ef9dde27 (running kernel: 6.8.12-4-pve)
##nft
nftables v1.0.6 (Lester Gooch #5)
##addr
[{"ifname":"lo","flags":["LOOPBACK","UP"],"operstate":"UNKNOWN","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8,"scope":"host"}]},{"ifname":"enp3s0f0","flags":["BROADCAST"],"operstate":"DOWN","addr_info":[]},{"ifname":"enp3s0f1","flags":["UP","LOWER_UP"],"operstate":"UP","addr_info":[]},{"ifname":"vmbr0","flags":["UP","LOWER_UP"],"operstate":"UP","addr_info":[{"family":"inet","local":"203.0.113.10","prefixlen":24,"scope":"global"},{"family":"inet6","local":"2001:db8:10::10","prefixlen":64,"scope":"global"}]},{"ifname":"vmbr1","flags":["UP","LOWER_UP"],"operstate":"UP","addr_info":[{"family":"inet","local":"10.0.0.1","prefixlen":24,"scope":"global"}]}]
##link
[{"ifname":"enp3s0f0"},{"ifname":"enp3s0f1","master":"vmbr0"},{"ifname":"vmbr0","linkinfo":{"info_kind":"bridge"}},{"ifname":"vmbr1","linkinfo":{"info_kind":"bridge"}}]
##route4
[{"dst":"default","gateway":"203.0.113.1","dev":"vmbr0"}]
##route6
[{"dst":"default","dev":"vmbr0"}]
##phys
enp3s0f0 -1
enp3s0f1 10000
##dhcp
dnsmasq inactive
isc-dhcp-server inactive
kea-dhcp4-server inactive
##ours
%[2]s
##confs
%[3]s##pub4
203.0.113.10
##pub6
`, s.name, ours, confs.String())
}

func (s *sim) nftJSON() string {
	items := []string{`{"table":{"family":"inet","name":"pve_forward"}}`}
	for id, c := range s.counters {
		items = append(items, fmt.Sprintf(`{"rule":{"comment":"r%d-up","expr":[{"counter":{"packets":1,"bytes":%d}}]}}`, id, c[0]),
			fmt.Sprintf(`{"rule":{"comment":"r%d-down","expr":[{"counter":{"packets":1,"bytes":%d}}]}}`, id, c[1]))
	}
	return `{"nftables":[` + strings.Join(items, ",") + `]}`
}

func wave(period float64, phase float64) float64 {
	return 0.5 + 0.5*math.Sin(float64(time.Now().Unix())/period+phase)
}

func (s *sim) collect() string {
	now := time.Now()
	dt := 10.0
	if !s.lastCollect.IsZero() {
		dt = now.Sub(s.lastCollect).Seconds()
	}
	s.lastCollect = now
	// 计数器递增
	for id := range s.rules {
		c := s.counters[id]
		r := s.ruleRate[id] * (0.4 + wave(300, float64(id)))
		c[0] += uint64(r * dt * 0.3)
		c[1] += uint64(r * dt)
		s.counters[id] = c
	}
	cpuBusy := 0.25 + 0.2*wave(120, 0)
	s.cpu[0] += uint64(3200 * dt)
	iow := 0.02
	if s.busyDisk {
		iow = 0.12 + 0.08*wave(60, 1)
	}
	s.cpu[2] += uint64(3200 * dt * (1 - cpuBusy - iow))
	idle := s.cpu[2]
	s.cpu[1] += uint64(3200 * dt * iow)
	disks := []struct {
		name string
		util float64
		iops float64
		lat  float64
	}{{"nvme0n1", 0.3 + 0.1*wave(90, 2), 4000, 0.8}, {"nvme1n1", 0.28, 3600, 0.7}, {"sda", 0.4, 180, 9}}
	if s.busyDisk {
		disks[2].util, disks[2].lat = 0.93+0.05*wave(30, 0), 45
	}
	var ds strings.Builder
	for i, d := range disks {
		x := &s.disk[i]
		ios := uint64(d.iops * dt)
		x[0] += ios / 2
		x[1] += ios / 2
		x[2] += ios * 16
		x[3] += ios * 32
		x[4] += uint64(d.util * dt * 1000)
		s.diskRW[i] += uint64(float64(ios) * d.lat)
		rw := s.diskRW[i]
		fmt.Fprintf(&ds, " 259 %d %s %d 0 %d %d %d 0 %d %d 0 %d %d 0 0 0 0\n", i, d.name, x[0], x[2], rw/2, x[1], x[3], rw/2, x[4], x[4])
	}
	s.net[0] += uint64(2e6 * dt * (0.5 + wave(200, 3)))
	s.net[1] += uint64(5e6 * dt * (0.5 + wave(200, 4)))

	var res, cg []string
	up := time.Since(s.start).Seconds() + 3*86400
	for _, v := range s.vms {
		c := s.vmc[v.id]
		run := v.status == "running"
		cpu := 0.0
		if run {
			cpu = math.Min(0.99, v.load*(0.6+0.8*wave(80, float64(v.id))))
			c[0] += uint64(v.load * 5e5 * dt)
			c[1] += uint64(v.load * 2e6 * dt)
			c[2] += uint64(v.load * 1e6 * dt)
			c[3] += uint64(v.load * 3e6 * dt)
			c[4] += uint64(v.load * 900 * dt)
		}
		s.vmc[v.id] = c
		mem := uint64(0)
		uptime := 0.0
		if run {
			mem = uint64(float64(v.mem) * (0.3 + 0.6*v.load))
			uptime = up - float64(v.id)
		}
		res = append(res, fmt.Sprintf(`{"type":"%s","vmid":%d,"name":"%s","node":"%s","status":"%s","cpu":%.3f,"maxcpu":%d,"mem":%d,"maxmem":%d,"disk":%d,"maxdisk":%d,"netin":%d,"netout":%d,"diskread":%d,"diskwrite":%d,"uptime":%d}`,
			v.typ, v.id, v.name, s.name, v.status, cpu, v.cores, mem, v.mem, v.disk/3, v.disk, c[0], c[1], c[2], c[3], int64(uptime)))
		if run {
			p := v.load * 40 * wave(60, float64(v.id))
			cg = append(cg, fmt.Sprintf("#%d\n259:0 rbytes=1 wbytes=1 rios=%d wios=%d dbytes=0 dios=0\nsome avg10=%.2f avg60=1 avg300=0 total=1", v.id, c[4]/2, c[4]/2, p))
		}
	}
	var confs strings.Builder
	for _, v := range s.vms {
		fmt.Fprintf(&confs, "#%s %d\nname: %s\nnet0: virtio=%s,bridge=vmbr1,hwaddr=%s\n", v.typ, v.id, v.name, strings.ToUpper(v.mac), strings.ToUpper(v.mac))
	}
	var health strings.Builder
	for id := range s.rules {
		st := "ok"
		if id%7 == 0 {
			st = "fail"
		}
		fmt.Fprintf(&health, "%d %s\n", id, st)
	}
	return fmt.Sprintf(`##hostname
%s
##stat
cpu  %d 0 0 %d %d 0 0 0 0 0
32
##mem
MemTotal:       131900000 kB
MemAvailable:   %d kB
##load
%.2f 2.80 2.65 2/1000 12345
##uptime
%.0f 1000.0
##diskstats
%s##lsblk
{"blockdevices":[{"name":"nvme0n1","model":"Samsung PM9A3 1.92TB","rota":false,"tran":"nvme"},{"name":"nvme1n1","model":"Samsung PM9A3 1.92TB","rota":false,"tran":"nvme"},{"name":"sda","model":"WDC WUH721816ALE6L4","rota":true,"tran":"sata"}]}
##netdev
  vmbr0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0
##storage
[{"storage":"local","total":100000000000,"used":31000000000,"active":1,"shared":0},{"storage":"local-lvm","total":1800000000000,"used":740000000000,"active":1,"shared":0}]
##resources
[%s]
##confs
%s##cgio
%s
##nft
%s
##listen
0.0.0.0:22
[::]:8006
0.0.0.0:20005
##conntrack
20000 3
20001 41
##health
%s`, s.name, s.cpu[0]-idle-s.cpu[1], idle, s.cpu[1], uint64(131900000*(0.42+0.1*wave(400, 1))), 3.12*cpuBusy*4, up, ds.String(),
		s.net[0], s.net[1], strings.Join(res, ","), confs.String(), strings.Join(cg, "\n"), s.nftJSON(), health.String())
}

func (s *sim) rrd(script string) string {
	n := 70
	step := int64(60)
	if strings.Contains(script, "--timeframe day") {
		step = 1200
	} else if strings.Contains(script, "--timeframe week") {
		step = 10800
	}
	now := time.Now().Unix()
	var pts []map[string]float64
	for i := 0; i < n; i++ {
		t := float64(i)
		pts = append(pts, map[string]float64{"time": float64(now - int64(n-i)*step), "cpu": 0.2 + 0.15*math.Sin(t/6) + 0.05*rand.Float64(), "maxcpu": 4,
			"mem": 4e9 + 1e9*math.Sin(t/10), "maxmem": 8e9, "netin": 2e5 + 1e5*math.Sin(t/5), "netout": 6e5 + 3e5*math.Cos(t/7),
			"diskread": 1e5 + 5e4*rand.Float64(), "diskwrite": 3e5 + 1e5*math.Sin(t/4)})
	}
	b, _ := json.Marshal(pts)
	return string(b)
}

func main() {
	server := flag.String("server", "http://127.0.0.1:5080", "控制端地址")
	token := flag.String("token", "", "接入令牌")
	name := flag.String("name", "pve-sim", "主机名")
	cfgPath := flag.String("config", "", "保存接入信息的文件（默认 devnode-<name>.json）")
	busy := flag.Bool("busy-disk", false, "模拟 sda 磁盘饱和")
	dnsAddr := flag.String("dns", "", "同时模拟 Cloudflare DNS API 的监听地址，如 127.0.0.1:5091")
	flag.Parse()
	if *dnsAddr != "" {
		serveFakeDNS(*dnsAddr)
	}
	if *cfgPath == "" {
		*cfgPath = "devnode-" + *name + ".json"
	}
	var jr proto.JoinResponse
	if b, err := os.ReadFile(*cfgPath); err == nil {
		json.Unmarshal(b, &jr)
	} else {
		body, _ := json.Marshal(proto.JoinRequest{Token: *token, Hostname: *name})
		resp, err := http.Post(*server+"/agent/join", "application/json", bytes.NewReader(body))
		if err != nil {
			log.Fatal(err)
		}
		if resp.StatusCode != 200 {
			log.Fatalf("接入失败 %d", resp.StatusCode)
		}
		json.NewDecoder(resp.Body).Decode(&jr)
		b, _ := json.Marshal(jr)
		os.WriteFile(*cfgPath, b, 0o600)
	}
	s := &sim{name: *name, start: time.Now(), rules: map[int64]bool{}, counters: map[int64][2]uint64{}, ruleRate: map[int64]float64{}, vmc: map[int][5]uint64{}, busyDisk: *busy,
		webSites: map[string][]int64{}, webStats: map[int64][3]uint64{}}
	rand.Seed(int64(len(*name)))
	names := []struct {
		t, n  string
		cores int
		load  float64
		st    string
	}{{"qemu", "web01", 4, 0.4, "running"}, {"lxc", "vpn01", 1, 0.1, "running"}, {"qemu", "win-rdp", 8, 0.7, "running"},
		{"qemu", "mc-server", 4, 0.5, "stopped"}, {"lxc", "blog", 1, 0.05, "running"}, {"qemu", "k8s-node1", 4, 0.55, "running"},
		{"qemu", "k8s-node2", 4, 0.5, "running"}, {"lxc", "nginx", 2, 0.08, "running"}}
	for i, x := range names {
		s.vms = append(s.vms, vm{id: 101 + i, typ: x.t, name: x.n, status: x.st, mac: fmt.Sprintf("bc:24:11:%02x:%02x:%02x", len(*name), i, 0x10+i),
			cores: x.cores, mem: uint64(x.cores) * 2 << 30, disk: uint64(x.cores) * 16 << 30, load: x.load})
	}
	ctx := context.Background()
	u := strings.Replace(*server, "http", "ws", 1) + "/agent/ws"
	for {
		ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {fmt.Sprintf("Bearer %d.%s", jr.NodeID, jr.Secret)}}})
		if err != nil {
			log.Printf("连接失败: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		ws.SetReadLimit(16 << 20)
		wsjson.Write(ctx, ws, proto.Hello{Version: "1.0.0", Hostname: *name})
		log.Printf("已连接（节点 %d）", jr.NodeID)
		err = manager.ServeAgentRPC(ctx, ws, s, "1.0.0")
		log.Printf("断开: %v", err)
		time.Sleep(2 * time.Second)
	}
}
