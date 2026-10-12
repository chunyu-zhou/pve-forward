package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"pveforward/internal/manager"
	"pveforward/internal/proto"
	"pveforward/internal/secret"
	"pveforward/internal/store"
)

// fakeNode 模拟 PVE 宿主机：根据脚本内容返回录制的命令输出，并记录下发的规则。
type fakeNode struct {
	mu          sync.Mutex
	rules       string // 最近一次下发的 nft 规则
	hosts       string
	inits       []string
	counters    map[int64][2]uint64
	failCollect bool
	rrdCalls    int
}

var b64Re = regexp.MustCompile(`printf '%s' '([A-Za-z0-9+/=]*)' \| base64 -d > '([^']+)'`)

func (f *fakeNode) Run(_ context.Context, script string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(script, "##pveversion"):
		return []byte(fakeDetect), nil
	case strings.Contains(script, "##before"):
		for _, m := range b64Re.FindAllStringSubmatch(script, -1) {
			b, _ := base64.StdEncoding.DecodeString(m[1])
			switch {
			case strings.Contains(m[2], "rules.nft"):
				f.rules = string(b)
			case strings.Contains(m[2], "dhcp-hosts"):
				f.hosts = string(b)
			}
		}
		f.counters = map[int64][2]uint64{} // 整表替换后计数清零
		return []byte("##before\n{}\n"), nil
	case strings.Contains(script, "rrddata"):
		f.rrdCalls++
		return []byte(`[{"time":1700000000,"cpu":0.25,"mem":1024}]`), nil
	case strings.Contains(script, "##diskstats"):
		if f.failCollect {
			return nil, fmt.Errorf("test collection failure")
		}
		var rules []string
		for id, c := range f.counters {
			rules = append(rules, fmt.Sprintf(`{"rule":{"comment":"r%d-up","expr":[{"counter":{"bytes":%d}}]}},{"rule":{"comment":"r%d-down","expr":[{"counter":{"bytes":%d}}]}}`, id, c[0], id, c[1]))
		}
		nft := `{"nftables":[{"table":{}}` + func() string {
			if len(rules) == 0 {
				return ""
			}
			return "," + strings.Join(rules, ",")
		}() + `]}`
		return []byte(strings.Replace(fakeCollect, "NFT", nft, 1)), nil
	default:
		f.inits = append(f.inits, script)
		return nil, nil
	}
}

func (f *fakeNode) setCounter(id int64, up, down uint64) {
	f.mu.Lock()
	f.counters[id] = [2]uint64{up, down}
	f.mu.Unlock()
}

func (f *fakeNode) nft() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rules
}

const fakeDetect = `##hostname
pve-e2e
##uid
0
##pveversion
pve-manager/8.2.4/abc (running kernel: 6.8)
##nft
nftables v1.0.6
##addr
[{"ifname":"enp3s0f1","flags":["UP","LOWER_UP"],"operstate":"UP","addr_info":[{"family":"inet","local":"203.0.113.10","prefixlen":24,"scope":"global"},{"family":"inet6","local":"2001:db8::10","prefixlen":64,"scope":"global"}]},{"ifname":"vmbr1","flags":["UP"],"operstate":"UP","addr_info":[{"family":"inet","local":"10.0.0.1","prefixlen":24,"scope":"global"}]}]
##link
[{"ifname":"enp3s0f1"},{"ifname":"vmbr1","linkinfo":{"info_kind":"bridge"}}]
##route4
[{"dev":"enp3s0f1"}]
##route6
[]
##phys
enp3s0f1 1000
##dhcp
dnsmasq inactive
##ours
##confs
#qemu 101
name: web01
net0: virtio=BC:24:11:00:00:01,bridge=vmbr1
#lxc 102
hostname: vpn01
net0: name=eth0,bridge=vmbr1,hwaddr=BC:24:11:00:00:02
##pub4
##pub6
`

const fakeCollect = `##hostname
pve-e2e
##stat
cpu  100 0 100 800 0 0 0 0 0 0
8
##mem
MemTotal: 16000000 kB
MemAvailable: 8000000 kB
##load
0.5 0.4 0.3 1/100 1
##uptime
1000 1000
##diskstats
##lsblk
##netdev
##storage
[]
##resources
[{"type":"qemu","vmid":101,"name":"web01","node":"pve-e2e","status":"running","cpu":0.1,"maxcpu":2,"mem":1,"maxmem":2},{"type":"lxc","vmid":102,"name":"vpn01","node":"pve-e2e","status":"running","maxcpu":1}]
##confs
#qemu 101
name: web01
net0: virtio=BC:24:11:00:00:01,bridge=vmbr1
#lxc 102
hostname: vpn01
net0: name=eth0,bridge=vmbr1,hwaddr=BC:24:11:00:00:02,ip=10.0.0.8/24,gw=10.0.0.1
##cgio
##nft
NFT
##listen
0.0.0.0:22
0.0.0.0:20009
##conntrack
##leases
1760000000 bc:24:11:00:00:01 10.0.0.57 web01 *
##health
`

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t, base, &http.Client{Jar: jar}}
}

// do 发送请求；want 为期望的状态码，out 可为 nil。返回响应体。
func (c *client) do(method, path string, body any, want int, out any) string {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "pveforward")
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s: 状态 %d（期望 %d）: %s", method, path, resp.StatusCode, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s 解析失败: %v: %s", method, path, err, b)
		}
	}
	return string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secret.Open(filepath.Join(dir, "master.key"))
	mgr := manager.New(db, box)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); time.Sleep(100 * time.Millisecond); db.Close() })
	mgr.Start(ctx)
	mgr.Interval = func() time.Duration { return time.Hour } // 只由 Kick / RunOnce 驱动
	srv := New(db, box, mgr, dir, fstest.MapFS{"index.html": {Data: []byte("<html>")}}, "")
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	admin := newClient(t, ts.URL)
	var boot map[string]any
	admin.do("GET", "/api/bootstrap", nil, 200, &boot)
	if boot["setup"] != false {
		t.Fatal("应未初始化")
	}
	admin.do("GET", "/api/users", nil, http.StatusPreconditionRequired, nil)

	// ---- 控制端初始化 ----
	tok := srv.NewSetupToken()
	if b, _ := os.ReadFile(filepath.Join(dir, "setup-token")); strings.TrimSpace(string(b)) != tok {
		t.Fatal("令牌文件不一致")
	}
	setup := map[string]any{"token": "WRONG-TOKEN-XXXX", "username": "admin", "password": "password123", "site": "测试站", "url": "https://panel.example.com", "port": 5000, "tls": "self"}
	admin.do("POST", "/api/setup", setup, 400, nil)
	setup["token"] = strings.ToLower(tok)
	admin.do("POST", "/api/setup", setup, 200, nil)
	admin.do("POST", "/api/setup", setup, http.StatusConflict, nil)
	admin.do("GET", "/api/bootstrap", nil, 200, &boot)
	if boot["setup"] != true || boot["user"] == nil {
		t.Fatalf("初始化后状态 %v", boot)
	}
	if _, err := os.Stat(filepath.Join(dir, "setup-token")); !os.IsNotExist(err) {
		t.Fatal("初始化后令牌文件应删除")
	}

	// CSRF：写请求缺少自定义头应被拒绝
	req, _ := http.NewRequest("POST", ts.URL+"/api/rules", strings.NewReader("{}"))
	if resp, _ := admin.hc.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 X-Requested-With 应返回 403，得到 %d", resp.StatusCode)
	}

	// ---- 添加 Agent 节点并接入 ----
	var created struct {
		Node    store.Node  `json:"node"`
		Install installInfo `json:"install"`
	}
	// 端口策略：可分配范围必填（为空用系统默认），保留端口支持单个与范围
	admin.do("POST", "/api/nodes", map[string]any{"name": "pve-bad", "mode": "agent", "portRange": "30000-20000"}, 400, nil)
	admin.do("POST", "/api/nodes", map[string]any{"name": "pve-bad", "mode": "agent", "portRange": " "}, 400, nil)
	admin.do("POST", "/api/nodes", map[string]any{"name": "pve-bad", "mode": "agent", "portRange": "100-101", "reservedPorts": "100-101"}, 400, nil)
	admin.do("POST", "/api/nodes", map[string]any{"name": "pve-e2e", "mode": "agent",
		"portRange": "20000-29999", "reservedPorts": "22，20095-20099 8006"}, 200, &created)
	nid := created.Node.ID
	if np := created.Node.Ports; np.Range != "20000-29999" || np.Reserved != "22,8006,20095-20099" {
		t.Fatalf("端口策略 %+v", np)
	}
	if !strings.Contains(created.Install.Command, "--token "+created.Install.Token) {
		t.Fatalf("安装命令 %q", created.Install.Command)
	}
	anon := newClient(t, ts.URL)
	anon.do("POST", "/agent/join", proto.JoinRequest{Token: "BAD"}, 401, nil)
	var jr proto.JoinResponse
	anon.do("POST", "/agent/join", proto.JoinRequest{Token: created.Install.Token, Hostname: "pve-e2e"}, 200, &jr)
	anon.do("POST", "/agent/join", proto.JoinRequest{Token: created.Install.Token}, 401, nil) // 令牌只能使用一次

	fake := &fakeNode{counters: map[int64][2]uint64{}}
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/agent/ws"
	if _, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {fmt.Sprintf("Bearer %d.wrong", nid)}}}); err == nil {
		t.Fatal("错误密钥应拒绝")
	}
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {fmt.Sprintf("Bearer %d.%s", nid, jr.Secret)}}})
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(16 << 20)
	wsjson.Write(ctx, ws, proto.Hello{Version: "1.0.0", Hostname: "pve-e2e"})
	go manager.ServeAgentRPC(ctx, ws, fake, "1.0.0")
	waitFor(t, "节点进入待初始化", func() bool { return mgr.Status(nid).State == "pending" })

	// ---- 检测与网络初始化 ----
	var det proto.Detect
	admin.do("POST", fmt.Sprintf("/api/nodes/%d/detect", nid), nil, 200, &det)
	if det.DefaultIface != "enp3s0f1" || len(det.Bridges) != 1 || det.PublicIP6 != "2001:db8::10" {
		t.Fatalf("检测结果 %+v", det)
	}
	initReq := map[string]any{"wanIface": "enp3s0f1", "wanIp4": "203.0.113.10", "wanIp6": "2001:db8::10", "bridge": "vmbr1",
		"cidr4": "203.0.113.0/24", "dhcp": true, "installDhcp": true, "ipv6": true}
	admin.do("POST", fmt.Sprintf("/api/nodes/%d/init", nid), initReq, 400, nil) // 公网段不允许作为内网
	initReq["cidr4"] = "10.0.0.0/24"
	stream := admin.do("POST", fmt.Sprintf("/api/nodes/%d/init", nid), initReq, 200, nil)
	if !strings.Contains(stream, `"done":true`) || strings.Contains(stream, `"error"`) {
		t.Fatalf("初始化流 %s", stream)
	}
	n, _ := db.Node(nid)
	if !n.Initialized || n.Net.AddrMode != "existing" || n.Net.GW4 != "10.0.0.1/24" || n.Net.DHCP != "managed" || !strings.HasSuffix(n.Net.CIDR6, "::/64") {
		t.Fatalf("节点网络 %+v", n.Net)
	}
	if mgr.Status(nid).State != "online" {
		t.Fatalf("状态 %+v", mgr.Status(nid))
	}

	// ---- 虚拟机同步 ----
	var vms []vmOut
	admin.do("GET", "/api/vms", nil, 200, &vms)
	if len(vms) != 2 || vms[0].MAC != "bc:24:11:00:00:01" {
		t.Fatalf("虚拟机 %+v", vms)
	}
	// 已有虚拟机沿用原来的内网地址（DHCP 租约 / LXC 静态地址），而不是重新分配
	if vms[0].IPv4 != "10.0.0.57" || vms[1].IPv4 != "10.0.0.8" || !strings.HasSuffix(vms[0].IPv6, "::57") {
		t.Fatalf("虚拟机原有地址 %+v %+v", vms[0], vms[1])
	}
	vmWeb, vmVPN := vms[0].ID, vms[1].ID

	// ---- 用户与端口分配 ----
	alice := map[string]any{"username": "alice", "password": "alicepass1", "enabled": true, "quotaGb": 0, "maxRules": 2,
		"allocations": []map[string]any{{"nodeId": nid, "kind": "range", "spec": "20000-20009"}}, "vmIds": []int64{vmWeb}}
	admin.do("POST", "/api/users", alice, 200, nil)
	admin.do("POST", "/api/users", alice, 400, nil) // 用户名重复
	bob := map[string]any{"username": "bob", "password": "bobpass12", "enabled": true,
		"allocations": []map[string]any{{"nodeId": nid, "kind": "multi", "spec": "20005,21000"}}}
	if body := admin.do("POST", "/api/users", bob, 400, nil); !strings.Contains(body, "已分配给用户 alice") {
		t.Fatalf("端口冲突提示 %s", body)
	}
	bob["allocations"] = []map[string]any{{"nodeId": nid, "kind": "single", "spec": "22"}}
	if body := admin.do("POST", "/api/users", bob, 400, nil); !strings.Contains(body, "不在 pve-e2e 的可分配范围 20000-29999 内") {
		t.Fatalf("可分配范围提示 %s", body)
	}
	bob["allocations"] = []map[string]any{{"nodeId": nid, "kind": "range", "spec": "20090-20096"}}
	if body := admin.do("POST", "/api/users", bob, 400, nil); !strings.Contains(body, "端口 20095 是 pve-e2e 的保留端口") {
		t.Fatalf("保留端口提示 %s", body)
	}
	bob["allocations"] = []map[string]any{{"nodeId": nid, "kind": "single", "spec": "21000"}}
	bob["vmIds"] = []int64{vmWeb}
	if body := admin.do("POST", "/api/users", bob, 400, nil); !strings.Contains(body, "已分配给用户 alice") {
		t.Fatalf("虚拟机归属冲突 %s", body)
	}
	bob["vmIds"] = []int64{}
	admin.do("POST", "/api/users", bob, 200, nil)
	// 修改节点端口策略不能与已有分配冲突（alice 20000-20009，bob 21000）
	if body := admin.do("PUT", fmt.Sprintf("/api/nodes/%d", nid), map[string]any{"mode": "agent", "reservedPorts": "20005-20006"}, 400, nil); !strings.Contains(body, "alice 的 20005") {
		t.Fatalf("保留端口冲突提示 %s", body)
	}
	if body := admin.do("PUT", fmt.Sprintf("/api/nodes/%d", nid), map[string]any{"mode": "agent", "portRange": "20000-20500"}, 400, nil); !strings.Contains(body, "bob 的 21000") {
		t.Fatalf("可分配范围冲突提示 %s", body)
	}
	var upd struct{ Node store.Node }
	admin.do("PUT", fmt.Sprintf("/api/nodes/%d", nid), map[string]any{"mode": "agent", "portRange": "20000-25000", "reservedPorts": "22,8006,20095-20099,22000"}, 200, &upd)
	if upd.Node.Ports.Range != "20000-25000" || upd.Node.Ports.Reserved != "22,8006,20095-20099,22000" {
		t.Fatalf("修改端口策略 %+v", upd.Node.Ports)
	}
	admin.do("GET", "/api/vms", nil, 200, &vms)
	if vms[0].IPv4 != "10.0.0.57" || vms[0].Owner != "alice" || !strings.HasSuffix(vms[0].IPv6, "::57") {
		t.Fatalf("分配后应沿用原有地址 %+v", vms[0])
	}
	// 公共虚拟机
	admin.do("PUT", fmt.Sprintf("/api/vms/%d", vmVPN), map[string]any{"action": "share"}, 200, nil)

	// ---- 普通用户 ----
	ua := newClient(t, ts.URL)
	ua.do("POST", "/api/login", map[string]string{"username": "alice", "password": "wrong"}, 401, nil)
	ua.do("POST", "/api/login", map[string]string{"username": "alice", "password": "alicepass1"}, 200, nil)
	ua.do("GET", "/api/users", nil, 403, nil)
	var opts struct {
		Nodes     []map[string]any `json:"nodes"`
		Ports     []int            `json:"ports"`
		BusyCount int              `json:"busyCount"`
		VMs       []map[string]any `json:"vms"`
	}
	ua.do("GET", fmt.Sprintf("/api/rule-options?node=%d&user=999", nid), nil, 200, &opts)
	if len(opts.Nodes) != 1 || len(opts.Ports) != 9 || opts.BusyCount != 1 || len(opts.VMs) != 2 {
		t.Fatalf("规则选项 %+v", opts) // 20009 被宿主机占用
	}
	rule := map[string]any{"userId": 999, "nodeId": nid, "port": 21000, "proto": "tcp", "ipVer": "v4", "vmId": vmWeb, "targetPort": 22}
	ua.do("POST", "/api/rules", rule, 400, nil) // 21000 属于 bob（userId 被忽略）
	rule["port"] = 20009
	if body := ua.do("POST", "/api/rules", rule, 400, nil); !strings.Contains(body, "宿主机进程占用") {
		t.Fatalf("宿主机占用提示 %s", body)
	}
	rule["port"] = 20000
	rule["ipVer"] = "both"
	rule["proto"] = "both"
	ua.do("POST", "/api/rules", rule, 200, nil)
	ua.do("POST", "/api/rules", rule, 400, nil) // 端口已被使用
	rule2 := map[string]any{"nodeId": nid, "port": 20001, "proto": "udp", "ipVer": "v4", "vmId": vmVPN, "targetPort": 51820}
	ua.do("POST", "/api/rules", rule2, 200, nil)
	rule2["port"] = 20002
	if body := ua.do("POST", "/api/rules", rule2, 400, nil); !strings.Contains(body, "规则上限") {
		t.Fatalf("规则上限提示 %s", body)
	}

	// 规则已下发到节点
	waitFor(t, "规则下发", func() bool { return strings.Contains(fake.nft(), "dport 20001") })
	nft := fake.nft()
	for _, want := range []string{`tcp dport 20000 counter dnat ip to 10.0.0.57:22`, `udp dport 20000 counter dnat ip to 10.0.0.57:22`,
		`dnat ip6 to [`, `udp dport 20001 counter dnat ip to 10.0.0.`, `ip saddr 10.0.0.0/24 masquerade`} {
		if !strings.Contains(nft, want) {
			t.Fatalf("规则缺少 %q\n%s", want, nft)
		}
	}
	// 由本系统管理 DHCP：所有已识别地址的虚拟机都写入保留，续租后仍为原来的地址
	if !strings.Contains(fake.hosts, "bc:24:11:00:00:01,10.0.0.57,") || !strings.Contains(fake.hosts, "bc:24:11:00:00:02,10.0.0.8,") {
		t.Fatalf("DHCP 绑定 %q", fake.hosts)
	}

	var rules []ruleOut
	ua.do("GET", "/api/rules", nil, 200, &rules)
	if len(rules) != 2 || rules[0].Entry4 != "203.0.113.10" || rules[0].Target4 != "10.0.0.57" {
		t.Fatalf("用户规则 %+v", rules)
	}
	ub := newClient(t, ts.URL)
	ub.do("POST", "/api/login", map[string]string{"username": "bob", "password": "bobpass12"}, 200, nil)
	var bobRules []ruleOut
	ub.do("GET", "/api/rules", nil, 200, &bobRules)
	if len(bobRules) != 0 {
		t.Fatal("bob 不应看到 alice 的规则")
	}

	// ---- 公共规则：管理员创建，可使用保留端口和宿主机在监听的端口（22），可指向任意虚拟机 ----
	var meInfo struct{ User store.User }
	admin.do("GET", "/api/me", nil, 200, &meInfo)
	adminID := meInfo.User.ID
	pubRule := map[string]any{"userId": adminID, "nodeId": nid, "port": 22, "proto": "tcp", "ipVer": "v4", "vmId": vmWeb, "targetPort": 22, "remark": "公共 SSH"}
	admin.do("POST", "/api/rules", pubRule, 200, nil)
	pubRule["port"] = 20003 // 已分配给 alice 的端口不能用于公共规则
	if body := admin.do("POST", "/api/rules", pubRule, 400, nil); !strings.Contains(body, "已分配给用户 alice") {
		t.Fatalf("公共规则占用用户端口 %s", body)
	}
	// 普通用户不能创建公共规则：userId 会被替换为自己，22 不是他的端口
	pubRule["port"] = 22
	ub.do("POST", "/api/rules", pubRule, 400, nil)
	// 公共规则占用的端口不能再分配给用户
	carol := map[string]any{"username": "carol", "password": "carolpass1", "enabled": true,
		"allocations": []map[string]any{{"nodeId": nid, "kind": "single", "spec": "22"}}}
	admin.do("POST", "/api/users", carol, 400, nil)
	var all []ruleOut
	admin.do("GET", "/api/rules", nil, 200, &all)
	var pubR *ruleOut
	for i := range all {
		if all[i].Port == 22 {
			pubR = &all[i]
		}
	}
	if pubR == nil || !pubR.Public || pubR.Owner != "公共" {
		t.Fatalf("公共规则 %+v", pubR)
	}
	ub.do("GET", "/api/rules", nil, 200, &bobRules)
	ua.do("GET", "/api/rules", nil, 200, &rules)
	if len(bobRules) != 0 || len(rules) != 2 {
		t.Fatal("普通用户不应看到公共规则")
	}
	waitFor(t, "公共规则下发", func() bool { return strings.Contains(fake.nft(), "tcp dport 22 counter dnat ip to 10.0.0.57:22") })
	admin.do("DELETE", fmt.Sprintf("/api/rules/%d", pubR.ID), nil, 200, nil)
	// 虚拟机转发流量趋势：管理员与归属用户可看，其他用户不可看
	var vt struct {
		Series []map[string]int64 `json:"series"`
		Step   int64              `json:"step"`
	}
	admin.do("GET", fmt.Sprintf("/api/monitor/traffic?vm=%d&range=7d&tz=480", vmWeb), nil, 200, &vt)
	if len(vt.Series) != 7 || vt.Step != 86400 {
		t.Fatalf("流量趋势 %+v", vt)
	}
	ua.do("GET", fmt.Sprintf("/api/monitor/traffic?vm=%d", vmWeb), nil, 200, &vt)
	if len(vt.Series) != 24 || vt.Step != 3600 {
		t.Fatalf("流量趋势 %+v", vt)
	}
	ub.do("GET", fmt.Sprintf("/api/monitor/traffic?vm=%d", vmWeb), nil, 404, nil)
	waitFor(t, "公共规则删除下发", func() bool { return !strings.Contains(fake.nft(), "dport 22 ") })
	waitFor(t, "节点状态已同步", func() bool { n, _ := db.Node(nid); return n.AppliedVersion == n.StateVersion })
	ub.do("DELETE", fmt.Sprintf("/api/rules/%d", rules[0].ID), nil, 404, nil)

	// ---- 流量统计与配额 ----
	fake.setCounter(rules[0].ID, 600<<20, 500<<20)
	if err := mgr.RunOnce(ctx, nid); err != nil {
		t.Fatal(err)
	}
	u, _ := db.UserByName("alice")
	if u.UsedBytes != 1100<<20 {
		t.Fatalf("已用流量 %d", u.UsedBytes)
	}
	fake.setCounter(rules[0].ID, 700<<20, 500<<20) // 增量 100 MiB
	mgr.RunOnce(ctx, nid)
	if u, _ = db.UserByName("alice"); u.UsedBytes != 1200<<20 {
		t.Fatalf("增量累计 %d", u.UsedBytes)
	}
	// 设置 1 GiB 配额 → 超限，规则从状态中移除
	var full userOut
	admin.do("GET", fmt.Sprintf("/api/users/%d", u.ID), nil, 200, &full)
	allocs := []map[string]any{}
	for _, a := range full.Allocations {
		allocs = append(allocs, map[string]any{"id": a.ID, "nodeId": a.NodeID, "kind": a.Kind, "spec": a.Spec})
	}
	admin.do("PUT", fmt.Sprintf("/api/users/%d", u.ID), map[string]any{"enabled": true, "quotaGb": 1, "maxRules": 2, "allocations": allocs, "vmIds": full.VMIDs}, 200, nil)
	waitFor(t, "超限后规则移除", func() bool { return !strings.Contains(fake.nft(), "dport 20000") })
	ua.do("GET", "/api/rules", nil, 200, &rules)
	if rules[0].Error != "用户流量已超限" {
		t.Fatalf("超限原因 %q", rules[0].Error)
	}
	var dash map[string]any
	admin.do("GET", "/api/dashboard", nil, 200, &dash)
	if dash["totalUp"].(float64) != float64(700<<20) {
		t.Fatalf("仪表盘总流量 %v", dash["totalUp"])
	}

	// 收回端口时删除对应规则
	admin.do("POST", fmt.Sprintf("/api/users/%d/action", u.ID), map[string]any{"action": "reset-traffic"}, 200, nil)
	admin.do("PUT", fmt.Sprintf("/api/users/%d", u.ID), map[string]any{"enabled": true, "maxRules": 2, "allocations": []any{}, "vmIds": full.VMIDs}, 200, nil)
	ua.do("GET", "/api/rules", nil, 200, &rules)
	if len(rules) != 0 {
		t.Fatalf("端口收回后规则应删除，剩余 %d", len(rules))
	}

	// ---- 公开监控页 ----
	anon.do("GET", "/api/public/abcd1234", nil, 404, nil)
	var set map[string]any
	admin.do("GET", "/api/settings", nil, 200, &set)
	pub := set["public"].(map[string]any)
	pub["enabled"], pub["slug"], pub["access"], pub["nodes"] = true, "abcd1234", "password", []int64{nid}
	admin.do("PUT", "/api/settings", map[string]any{"settings": set["settings"], "public": pub}, 400, nil) // 缺少密码
	admin.do("PUT", "/api/settings", map[string]any{"settings": set["settings"], "public": pub, "publicPwd": "view2026"}, 200, nil)
	anon.do("GET", "/api/public/abcd1234", nil, 401, nil)
	req, _ = http.NewRequest("GET", ts.URL+"/api/public/abcd1234", nil)
	req.Header.Set("X-Access-Password", "view2026")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("公开页 %v %v", err, resp.StatusCode)
	}
	var pubData map[string]any
	json.NewDecoder(resp.Body).Decode(&pubData)
	pvms := pubData["vms"].([]any)
	if len(pvms) != 2 || pvms[0].(map[string]any)["ipv4"] != nil || pvms[0].(map[string]any)["owner"] != nil {
		t.Fatalf("公开页不应包含 IP / 归属: %v", pvms[0])
	}

	resp.Body.Close()
	testMonitorRegressions(t, srv, admin, ua, ub, anon, fake, nid, vmWeb, vmVPN)

	// ---- 删除节点（含清理） ----
	admin.do("DELETE", fmt.Sprintf("/api/nodes/%d?cleanup=1", nid), nil, 200, nil)
	if fake.inits[len(fake.inits)-1] == "" || !strings.Contains(fake.inits[len(fake.inits)-1], "nft delete table inet pve_forward") {
		t.Fatal("删除节点应执行清理")
	}
	admin.do("GET", "/api/vms", nil, 200, &vms)
	if len(vms) != 0 {
		t.Fatal("节点删除后虚拟机记录应删除")
	}
}

// Exercise the serialized HTTP responses, not just the Go projection structs.
func testMonitorRegressions(t *testing.T, srv *Server, admin, alice, bob, anon *client, fake *fakeNode, nid, ownedID, sharedID int64) {
	t.Helper()
	original := srv.publicCfg()
	t.Cleanup(func() { srv.db.SetSettingJSON("public", original) })
	setPublic := func(c PublicCfg) {
		t.Helper()
		if err := srv.db.SetSettingJSON("public", c); err != nil {
			t.Fatal(err)
		}
	}
	publicGet := func(t *testing.T, path, password string, want int) []byte {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Access-Password", password)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("GET %s: status %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.Bytes()
	}
	decodeVMs := func(t *testing.T, body []byte) []map[string]any {
		t.Helper()
		var d struct {
			VMs []map[string]any `json:"vms"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			t.Fatal(err)
		}
		return d.VMs
	}
	rrdCalls := func() int { fake.mu.Lock(); defer fake.mu.Unlock(); return fake.rrdCalls }

	t.Run("monitor-json-privacy", func(t *testing.T) {
		for _, showIP := range []bool{false, true} {
			for _, showOwner := range []bool{false, true} {
				t.Run(fmt.Sprintf("ip=%t/owner=%t", showIP, showOwner), func(t *testing.T) {
					c := original
					c.ShowIP, c.ShowOwner = showIP, showOwner
					setPublic(c)
					defer setPublic(original)
					body := publicGet(t, "/api/public/abcd1234", "view2026", 200)
					vms := decodeVMs(t, body)
					if len(vms) != 2 {
						t.Fatalf("VMs: %s", body)
					}
					for _, v := range vms {
						for _, key := range []string{"mac", "userId", "passwordHash"} {
							if _, ok := v[key]; ok {
								t.Errorf("private field %s serialized: %v", key, v)
							}
						}
						for _, key := range []string{"ipv4", "ipv6"} {
							if _, ok := v[key]; ok != showIP {
								t.Errorf("%s visibility = %t, want %t", key, ok, showIP)
							}
						}
						if int64(v["id"].(float64)) == ownedID {
							owner, ok := v["owner"]
							if ok != showOwner || (ok && owner != "alice") {
								t.Errorf("owner visibility: %v", v)
							}
						}
					}
					// Redaction must not modify the manager's report used by other consumers.
					if srv.mgr.Status(nid).Report.VMs[0].MAC == "" {
						t.Error("source report MAC was cleared")
					}
				})
			}
		}
	})

	t.Run("monitor-user-scope", func(t *testing.T) {
		anon.do("GET", "/api/monitor", nil, 401, nil)
		av := decodeVMs(t, []byte(alice.do("GET", "/api/monitor", nil, 200, nil)))
		if len(av) != 1 || int64(av[0]["id"].(float64)) != ownedID || av[0]["ipv4"] == nil {
			t.Fatalf("alice scope: %v", av)
		}
		if bv := decodeVMs(t, []byte(bob.do("GET", "/api/monitor", nil, 200, nil))); len(bv) != 0 {
			t.Fatalf("bob scope: %v", bv)
		}
	})

	t.Run("rrd-permissions", func(t *testing.T) {
		ownedPath := fmt.Sprintf("/api/monitor/rrd?vm=%d&tf=hour", ownedID)
		sharedPath := fmt.Sprintf("/api/monitor/rrd?vm=%d&tf=day", sharedID)
		nodePath := fmt.Sprintf("/api/monitor/rrd?node=%d", nid)
		before := rrdCalls()
		anon.do("GET", ownedPath, nil, 401, nil)
		bob.do("GET", ownedPath, nil, 404, nil)
		alice.do("GET", sharedPath, nil, 404, nil)
		alice.do("GET", nodePath, nil, 403, nil)
		if rrdCalls() != before {
			t.Error("unauthorized request reached node")
		}
		for _, tc := range []struct {
			c    *client
			path string
		}{{alice, ownedPath}, {admin, sharedPath}, {admin, nodePath}} {
			var points []proto.RRDPoint
			tc.c.do("GET", tc.path, nil, 200, &points)
			if len(points) != 1 || points[0]["cpu"] != 0.25 {
				t.Errorf("RRD points: %v", points)
			}
		}
		pubPath := fmt.Sprintf("/api/public/abcd1234/rrd?vm=%d", ownedID)
		before = rrdCalls()
		publicGet(t, pubPath, "", 401)
		publicGet(t, pubPath, "wrong", 401)
		publicGet(t, strings.Replace(pubPath, "abcd1234", "wrong-slug", 1), "view2026", 404)
		for _, tc := range []struct {
			name   string
			change func(*PublicCfg)
		}{
			{"disabled", func(c *PublicCfg) { c.Enabled = false }},
			{"detail-hidden", func(c *PublicCfg) { c.ShowDetail = false }},
			{"vms-hidden", func(c *PublicCfg) { c.ShowVMs = false }},
			{"node-excluded", func(c *PublicCfg) { c.Nodes = []int64{} }},
			{"only-shared", func(c *PublicCfg) { c.OnlyShared = true }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := original
				tc.change(&c)
				setPublic(c)
				defer setPublic(original)
				publicGet(t, pubPath, "view2026", 404)
			})
		}
		publicGet(t, "/api/public/abcd1234/rrd?vm=999999", "view2026", 404)
		publicGet(t, fmt.Sprintf("/api/public/abcd1234/rrd?node=%d", nid), "view2026", 404)
		if rrdCalls() != before {
			t.Error("denied public request reached node")
		}
		publicGet(t, pubPath, "view2026", 200)
		c := original
		c.OnlyShared = true
		setPublic(c)
		publicGet(t, fmt.Sprintf("/api/public/abcd1234/rrd?vm=%d", sharedID), "view2026", 200)
		setPublic(original)
	})

	t.Run("rrd-excludes-missing-vms-and-uninitialized-nodes", func(t *testing.T) {
		for _, tc := range []struct {
			name, update, restore string
			id                    int64
		}{
			{"missing-vm", "UPDATE vms SET present=0 WHERE id=?", "UPDATE vms SET present=1 WHERE id=?", ownedID},
			{"uninitialized-node", "UPDATE nodes SET initialized=0 WHERE id=?", "UPDATE nodes SET initialized=1 WHERE id=?", nid},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := srv.db.Exec(tc.update, tc.id); err != nil {
					t.Fatal(err)
				}
				defer srv.db.Exec(tc.restore, tc.id)
				before := rrdCalls()
				publicGet(t, fmt.Sprintf("/api/public/abcd1234/rrd?vm=%d", ownedID), "view2026", 404)
				alice.do("GET", fmt.Sprintf("/api/monitor/rrd?vm=%d", ownedID), nil, 404, nil)
				admin.do("GET", fmt.Sprintf("/api/monitor/rrd?vm=%d", ownedID), nil, 404, nil)
				if rrdCalls() != before {
					t.Error("invisible VM RRD reached node")
				}
			})
		}
	})

	t.Run("offline-vm-status", func(t *testing.T) {
		fake.mu.Lock()
		fake.failCollect = true
		fake.mu.Unlock()
		defer func() {
			fake.mu.Lock()
			fake.failCollect = false
			fake.mu.Unlock()
			if err := srv.mgr.RunOnce(context.Background(), nid); err != nil {
				t.Error(err)
			}
		}()
		if err := srv.mgr.RunOnce(context.Background(), nid); err == nil {
			t.Fatal("expected collection failure")
		}
		st := srv.mgr.Status(nid)
		if st.State != "offline" || st.Report == nil {
			t.Fatalf("need offline node with cached report: %+v", st)
		}
		c := original
		c.ShowNodes = false
		setPublic(c)
		defer setPublic(original)
		bodies := []string{
			admin.do("GET", "/api/monitor", nil, 200, nil),
			alice.do("GET", "/api/monitor", nil, 200, nil),
			string(publicGet(t, "/api/public/abcd1234", "view2026", 200)),
		}
		for _, body := range bodies {
			vms := decodeVMs(t, []byte(body))
			if len(vms) == 0 {
				t.Fatalf("missing VMs: %s", body)
			}
			for _, v := range vms {
				if v["status"] != "offline" {
					t.Errorf("cached VM shown as live: %v", v)
				}
				for _, key := range []string{"cpu", "memUsed", "diskUsed", "netIn", "netOut", "diskR", "diskW", "iops", "ioPressure", "uptime"} {
					if v[key] != float64(0) {
						t.Errorf("stale %s retained: %v", key, v[key])
					}
				}
				if int64(v["id"].(float64)) == ownedID && v["cores"] != float64(2) {
					t.Error("offline VM lost known configuration")
				}
			}
		}
	})
}
