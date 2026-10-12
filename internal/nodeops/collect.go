package nodeops

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"pveforward/internal/proto"
)

// Collector 采集节点指标。CPU、磁盘、网络速率需要两次采样之差，因此保存上一次的样本。
type Collector struct {
	mu   sync.Mutex
	prev *sample
}

type sample struct {
	t       time.Time
	cpu     [2]uint64 // total, iowait
	cpuBusy uint64
	disks   map[string][5]uint64 // reads, writes, sectorsR, sectorsW, msIO; 另记 msRW
	diskRW  map[string]uint64
	net     [2]uint64
	vmNet   map[int]vmCounter
	vmIOs   map[int]uint64
}

// vmCounter 为虚拟机网络与磁盘计数器（netin, netout, diskread, diskwrite）。
// 这些计数器来自 pvesh，PVE 大约每 10 秒才更新一次：计数器未变化时沿用上次的速率与基准，
// 否则采集间隔较短时速率会时而为 0。
type vmCounter struct {
	v    [4]uint64
	t    time.Time
	rate [4]float64
}

// vmStale 为计数器未更新时沿用上次速率的最长时间。
const vmStale = 30 * time.Second

func collectScript(st *proto.DesiredState) string {
	wan := "lo"
	if st != nil && ValidIface(st.Net.WANIface) {
		wan = st.Net.WANIface
	}
	vmip := ""
	if st != nil && ValidIface(st.Net.Bridge) {
		vmip = vmIPScript(st.Net.Bridge)
	}
	var health strings.Builder
	if st != nil {
		for _, r := range st.Rules {
			if r.Proto == "udp" {
				continue
			}
			t := r.Target4
			if r.IPVer == "v6" || t == "" {
				t = r.Target6
			}
			if t == "" || !safeAddrRe.MatchString(t) {
				continue
			}
			fmt.Fprintf(&health, "( timeout 2 bash -c '</dev/tcp/%s/%d' 2>/dev/null && echo '%d ok' || echo '%d fail' ) &\n", t, r.TargetPort, r.ID, r.ID)
		}
	}
	return `export LC_ALL=C
N=$(hostname -s)
echo '##hostname'; echo "$N"
echo '##stat'; head -1 /proc/stat; grep -c '^processor' /proc/cpuinfo
echo '##mem'; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo
echo '##load'; cat /proc/loadavg
echo '##uptime'; cat /proc/uptime
echo '##diskstats'; cat /proc/diskstats
echo '##lsblk'; lsblk -J -d -o NAME,MODEL,ROTA,TRAN 2>/dev/null
echo '##netdev'; grep -E '^\s*` + wan + `:' /proc/net/dev
echo '##storage'; pvesh get /nodes/$N/storage --output-format json 2>/dev/null
echo '##resources'; pvesh get /cluster/resources --type vm --output-format json 2>/dev/null
echo '##confs'
` + vmConfScript + `
echo '##cgio'
for d in /sys/fs/cgroup/qemu.slice/*.scope /sys/fs/cgroup/lxc/*; do [ -f "$d/io.stat" ] || continue; echo "#$(basename "$d" .scope)"; cat "$d/io.stat"; grep '^some' "$d/io.pressure" 2>/dev/null; done
echo '##nft'; nft -j list table inet ` + TableName + ` 2>/dev/null
echo '##listen'; ss -Hlntu 2>/dev/null | awk '{print $5}'
echo '##conntrack'; conntrack -L 2>/dev/null | awk '{d="";for(i=1;i<=NF;i++){if($i~/^dport=/){d=substr($i,7);break}} if(d!="")c[d]++} END{for(k in c)print k,c[k]}'
` + vmip + `echo '##health'
` + health.String() + `wait
`
}

var safeAddrRe = regexp.MustCompile(`^[0-9a-fA-F:.]+$`)

// Collect 执行一次采集。st 为当前期望状态（用于健康检查和定位公网网口），可为 nil。
func (c *Collector) Collect(ctx context.Context, ex Executor, st *proto.DesiredState) (*proto.Report, error) {
	out, err := ex.Run(ctx, collectScript(st))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rep, cur := parseCollect(sections(out), c.prev, time.Now(), st)
	c.prev = cur
	return rep, nil
}

func parseCollect(s map[string]string, prev *sample, now time.Time, st *proto.DesiredState) (*proto.Report, *sample) {
	rep := &proto.Report{Time: now, Counters: map[int64]proto.Counter{}, Conns: map[int64]int{}, Health: map[int64]string{}}
	cur := &sample{t: now, disks: map[string][5]uint64{}, diskRW: map[string]uint64{}, vmNet: map[int]vmCounter{}, vmIOs: map[int]uint64{}}
	dt := 0.0
	if prev != nil {
		dt = now.Sub(prev.t).Seconds()
	}
	rate := func(a, b uint64) float64 {
		if dt <= 0 || a < b {
			return 0
		}
		return float64(a-b) / dt
	}

	// CPU
	lines := strings.Split(strings.TrimSpace(s["stat"]), "\n")
	if len(lines) >= 1 {
		f := strings.Fields(lines[0])
		var vals []uint64
		for _, x := range f[1:] {
			v, _ := strconv.ParseUint(x, 10, 64)
			vals = append(vals, v)
		}
		if len(vals) >= 5 {
			var total uint64
			for i, v := range vals {
				if i < 8 { // user nice system idle iowait irq softirq steal
					total += v
				}
			}
			idle := vals[3] + vals[4]
			cur.cpu = [2]uint64{total, vals[4]}
			cur.cpuBusy = total - idle
			if prev != nil && total > prev.cpu[0] {
				d := float64(total - prev.cpu[0])
				rep.Node.CPU = round1(float64(cur.cpuBusy-min64(cur.cpuBusy, prev.cpuBusy)) / d * 100)
				rep.Node.IOWait = round1(float64(vals[4]-min64(vals[4], prev.cpu[1])) / d * 100)
			}
		}
	}
	if len(lines) >= 2 {
		rep.Node.Cores, _ = strconv.Atoi(strings.TrimSpace(lines[1]))
	}
	// 内存
	for _, l := range strings.Split(s["mem"], "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			rep.Node.MemTotal = v * 1024
		case "MemAvailable:":
			rep.Node.MemUsed = v * 1024
		}
	}
	if rep.Node.MemTotal >= rep.Node.MemUsed {
		rep.Node.MemUsed = rep.Node.MemTotal - rep.Node.MemUsed
	}
	if f := strings.Fields(s["load"]); len(f) >= 3 {
		for i := 0; i < 3; i++ {
			rep.Node.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	if f := strings.Fields(s["uptime"]); len(f) >= 1 {
		u, _ := strconv.ParseFloat(f[0], 64)
		rep.Node.Uptime = int64(u)
	}

	// 物理磁盘
	models := parseLsblk(s["lsblk"])
	for _, l := range strings.Split(s["diskstats"], "\n") {
		f := strings.Fields(l)
		if len(f) < 14 || !physDiskRe.MatchString(f[2]) {
			continue
		}
		u := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		name := f[2]
		v := [5]uint64{u(3), u(7), u(5), u(9), u(12)}
		cur.disks[name] = v
		cur.diskRW[name] = u(6) + u(10)
		m := models[name]
		dm := proto.DiskMetrics{Dev: name, Model: m.model, Kind: m.kind}
		if p, ok := prevDisk(prev, name); ok && dt > 0 {
			dm.RIOPS = round1(rate(v[0], p[0]))
			dm.WIOPS = round1(rate(v[1], p[1]))
			dm.RBps = rate(v[2], p[2]) * 512
			dm.WBps = rate(v[3], p[3]) * 512
			dm.Util = round1(minf(rate(v[4], p[4])/10, 100)) // ms/s → %
			ios := (v[0] - min64(v[0], p[0])) + (v[1] - min64(v[1], p[1]))
			if ios > 0 {
				rw := cur.diskRW[name] - min64(cur.diskRW[name], prev.diskRW[name])
				dm.Await = round1(float64(rw) / float64(ios))
			}
		}
		rep.Disks = append(rep.Disks, dm)
	}

	// 公网网口流量
	if f := strings.Fields(strings.ReplaceAll(s["netdev"], ":", " ")); len(f) >= 10 {
		rx, _ := strconv.ParseUint(f[1], 10, 64)
		tx, _ := strconv.ParseUint(f[9], 10, 64)
		cur.net = [2]uint64{rx, tx}
		if prev != nil {
			rep.Node.NetIn = rate(rx, prev.net[0])
			rep.Node.NetOut = rate(tx, prev.net[1])
		}
	}

	// 存储容量（本地存储合计）
	var stores []struct {
		Total  uint64 `json:"total"`
		Used   uint64 `json:"used"`
		Active int    `json:"active"`
		Shared int    `json:"shared"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(s["storage"])), &stores)
	for _, st := range stores {
		if st.Active == 1 && st.Shared == 0 {
			rep.Node.DiskTotal += st.Total
			rep.Node.DiskUsed += st.Used
		}
	}

	// 虚拟机
	bridge := ""
	if st != nil {
		bridge = st.Net.Bridge
	}
	confs := map[int]vmConf{}
	for _, c := range parseConfs(s["confs"]) {
		confs[c.VMID] = c
	}
	cg := parseCgroupIO(s["cgio"])
	host := strings.TrimSpace(s["hostname"])
	var addrs vmAddrs
	if st != nil {
		addrs = parseVMAddrs(s, st.Net.CIDR4, st.Net.CIDR6)
	}
	var res []struct {
		Type      string  `json:"type"`
		VMID      int     `json:"vmid"`
		Name      string  `json:"name"`
		Node      string  `json:"node"`
		Status    string  `json:"status"`
		Template  int     `json:"template"`
		CPU       float64 `json:"cpu"`
		MaxCPU    float64 `json:"maxcpu"`
		Mem       uint64  `json:"mem"`
		MaxMem    uint64  `json:"maxmem"`
		Disk      uint64  `json:"disk"`
		MaxDisk   uint64  `json:"maxdisk"`
		NetIn     uint64  `json:"netin"`
		NetOut    uint64  `json:"netout"`
		DiskRead  uint64  `json:"diskread"`
		DiskWrite uint64  `json:"diskwrite"`
		Uptime    int64   `json:"uptime"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(s["resources"])), &res)
	inRes := map[int]bool{}
	for _, r := range res {
		c, local := confs[r.VMID]
		if !local || r.Template == 1 || (host != "" && r.Node != host && r.Node != "") {
			continue
		}
		inRes[r.VMID] = true
		vm := proto.VMInfo{VMID: r.VMID, Type: r.Type, Name: r.Name, Status: r.Status, Cores: int(r.MaxCPU),
			CPU: round1(r.CPU * 100), MemUsed: r.Mem, MemTotal: r.MaxMem, DiskUsed: r.Disk, DiskTotal: r.MaxDisk, Uptime: r.Uptime}
		vm.MAC = pickMAC(c, bridge)
		// 虚拟机当前实际使用的内网地址：静态配置优先，其次 DHCP 租约与邻居表
		vm.IP4, vm.IP6 = staticAddrs(c, bridge)
		if vm.IP4 == "" && vm.MAC != "" {
			vm.IP4 = addrs.v4[vm.MAC]
		}
		if vm.IP6 == "" && vm.MAC != "" {
			vm.IP6 = addrs.v6[vm.MAC]
		}
		vc := vmCounter{v: [4]uint64{r.NetIn, r.NetOut, r.DiskRead, r.DiskWrite}, t: now}
		if p, ok := prevVM(prev, r.VMID); ok && r.Status == "running" {
			if vc.v == p.v && now.Sub(p.t) < vmStale {
				vc = p // 计数器尚未更新：保留基准，沿用上次的速率
			} else if d := now.Sub(p.t).Seconds(); d > 0 {
				for i := range vc.v {
					if vc.v[i] >= p.v[i] {
						vc.rate[i] = float64(vc.v[i]-p.v[i]) / d
					}
				}
			}
		}
		cur.vmNet[r.VMID] = vc
		if r.Status == "running" {
			vm.NetIn, vm.NetOut, vm.DiskR, vm.DiskW = vc.rate[0], vc.rate[1], vc.rate[2], vc.rate[3]
		}
		if io, ok := cg[r.VMID]; ok {
			cur.vmIOs[r.VMID] = io.ios
			if prev != nil {
				if p, ok := prev.vmIOs[r.VMID]; ok {
					vm.IOPS = round1(rate(io.ios, p))
				}
			}
			vm.IOPressure = io.pressure
		}
		if r.Status != "running" {
			vm.CPU, vm.MemUsed = 0, 0
		}
		rep.VMs = append(rep.VMs, vm)
	}
	// pvesh 不可用时，至少从配置文件得到虚拟机列表
	for id, c := range confs {
		if !inRes[id] && len(res) == 0 {
			rep.VMs = append(rep.VMs, proto.VMInfo{VMID: id, Type: c.Type, Name: c.Name, Status: "unknown", Cores: c.Cores, MAC: pickMAC(c, bridge)})
		}
	}

	// 规则计数器
	rep.Counters, rep.Loaded = parseNftCounters(s["nft"])
	// 宿主机监听端口
	seen := map[int]bool{}
	for _, l := range strings.Split(s["listen"], "\n") {
		l = strings.TrimSpace(l)
		if i := strings.LastIndex(l, ":"); i >= 0 {
			if p, err := strconv.Atoi(l[i+1:]); err == nil && !seen[p] {
				seen[p] = true
				rep.Listen = append(rep.Listen, p)
			}
		}
	}
	// 连接数（按原始目标端口）
	conns := map[int]int{}
	for _, l := range strings.Split(s["conntrack"], "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			p, _ := strconv.Atoi(f[0])
			n, _ := strconv.Atoi(f[1])
			conns[p] = n
		}
	}
	healthy := map[int64]string{}
	for _, l := range strings.Split(s["health"], "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			id, _ := strconv.ParseInt(f[0], 10, 64)
			healthy[id] = f[1]
		}
	}
	if st != nil {
		for _, r := range st.Rules {
			rep.Conns[r.ID] = conns[r.Port]
			msg := ""
			if seen[r.Port] {
				msg = fmt.Sprintf("端口冲突：%d 已被宿主机进程监听", r.Port)
			} else if healthy[r.ID] == "fail" {
				t := r.Target4
				if r.IPVer == "v6" || t == "" {
					t = "[" + r.Target6 + "]"
				}
				msg = fmt.Sprintf("目标不可达：%s:%d 连接失败", t, r.TargetPort)
			} else if !rep.Loaded {
				msg = "规则表未加载"
			}
			rep.Health[r.ID] = msg
		}
	}
	return rep, cur
}

var physDiskRe = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme\d+n\d+)$`)

type diskModel struct{ model, kind string }

func parseLsblk(text string) map[string]diskModel {
	var v struct {
		Devices []struct {
			Name  string          `json:"name"`
			Model *string         `json:"model"`
			Rota  json.RawMessage `json:"rota"`
			Tran  *string         `json:"tran"`
		} `json:"blockdevices"`
	}
	res := map[string]diskModel{}
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &v) != nil {
		return res
	}
	for _, d := range v.Devices {
		m := diskModel{}
		if d.Model != nil {
			m.model = strings.TrimSpace(*d.Model)
		}
		rota := strings.Trim(string(d.Rota), `"`)
		switch {
		case strings.HasPrefix(d.Name, "nvme") || (d.Tran != nil && *d.Tran == "nvme"):
			m.kind = "NVMe"
		case rota == "true" || rota == "1":
			m.kind = "HDD"
		default:
			m.kind = "SSD"
		}
		res[d.Name] = m
	}
	return res
}

type cgIO struct {
	ios      uint64
	pressure float64
}

// parseCgroupIO 解析 "#101" 分隔的 io.stat 与 io.pressure。
func parseCgroupIO(text string) map[int]cgIO {
	res := map[int]cgIO{}
	id := -1
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") {
			id, _ = strconv.Atoi(strings.TrimPrefix(l, "#"))
			if id <= 0 {
				id = -1
			}
			continue
		}
		if id < 0 || l == "" {
			continue
		}
		c := res[id]
		if strings.HasPrefix(l, "some ") {
			for _, kv := range strings.Fields(l)[1:] {
				if v, ok := strings.CutPrefix(kv, "avg10="); ok {
					c.pressure, _ = strconv.ParseFloat(v, 64)
				}
			}
		} else {
			for _, kv := range strings.Fields(l)[1:] {
				k, v, _ := strings.Cut(kv, "=")
				if k == "rios" || k == "wios" {
					n, _ := strconv.ParseUint(v, 10, 64)
					c.ios += n
				}
			}
		}
		res[id] = c
	}
	return res
}

var counterCommentRe = regexp.MustCompile(`^r(\d+)-(up|down)$`)

// parseNftCounters 从 nft -j 输出中读取每条规则的上下行字节数。
func parseNftCounters(text string) (map[int64]proto.Counter, bool) {
	res := map[int64]proto.Counter{}
	var v struct {
		Nftables []struct {
			Rule *struct {
				Comment string            `json:"comment"`
				Expr    []json.RawMessage `json:"expr"`
			} `json:"rule"`
			Table *struct{} `json:"table"`
		} `json:"nftables"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &v) != nil {
		return res, false
	}
	loaded := false
	for _, it := range v.Nftables {
		if it.Table != nil {
			loaded = true
		}
		if it.Rule == nil {
			continue
		}
		m := counterCommentRe.FindStringSubmatch(it.Rule.Comment)
		if m == nil {
			continue
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		for _, e := range it.Rule.Expr {
			var c struct {
				Counter *struct {
					Bytes uint64 `json:"bytes"`
				} `json:"counter"`
			}
			if json.Unmarshal(e, &c) == nil && c.Counter != nil {
				cc := res[id]
				if m[2] == "up" {
					cc.Up += c.Counter.Bytes
				} else {
					cc.Down += c.Counter.Bytes
				}
				res[id] = cc
			}
		}
	}
	return res, loaded
}

func pickMAC(c vmConf, bridge string) string {
	for _, n := range c.Nets {
		if bridge != "" && n.Bridge == bridge {
			return n.MAC
		}
	}
	return ""
}

func prevDisk(p *sample, name string) ([5]uint64, bool) {
	if p == nil {
		return [5]uint64{}, false
	}
	v, ok := p.disks[name]
	return v, ok
}

func prevVM(p *sample, id int) (vmCounter, bool) {
	if p == nil {
		return vmCounter{}, false
	}
	v, ok := p.vmNet[id]
	return v, ok
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
