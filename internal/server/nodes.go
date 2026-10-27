package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"pveforward/internal/manager"
	"pveforward/internal/nodeops"
	"pveforward/internal/proto"
	"pveforward/internal/secret"
	"pveforward/internal/sshx"
	"pveforward/internal/store"
)

type nodeOut struct {
	*store.Node
	Status   string             `json:"status"`
	Error    string             `json:"error,omitempty"`
	Stale    bool               `json:"stale,omitempty"` // 在线但本轮采集失败，指标为上次结果
	Since    time.Time          `json:"since"`
	Metrics  *proto.NodeMetrics `json:"metrics,omitempty"`
	VMs      int                `json:"vms"`
	Rules    int                `json:"rules"`
	CredName string             `json:"credName,omitempty"`
}

func (s *Server) nodeOut(n *store.Node) *nodeOut {
	st := s.mgr.Status(n.ID)
	o := &nodeOut{Node: n, Status: st.State, Error: st.Error, Stale: st.Stale, Since: st.Since}
	if st.Report != nil {
		m := st.Report.Node
		o.Metrics = &m
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM vms WHERE node_id=? AND present=1`, n.ID).Scan(&o.VMs)
	s.db.QueryRow(`SELECT COUNT(*) FROM rules WHERE node_id=?`, n.ID).Scan(&o.Rules)
	if n.CredID != nil {
		if c, err := s.db.Credential(*n.CredID); err == nil {
			o.CredName = c.Name
		}
	}
	return o
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.db.Nodes()
	if err != nil {
		fail(w, err)
		return
	}
	res := []*nodeOut{}
	for _, n := range nodes {
		res = append(res, s.nodeOut(n))
	}
	writeJSON(w, map[string]any{"nodes": res, "agentUrl": s.agentURL(), "version": Version})
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	st := s.mgr.Status(n.ID)
	writeJSON(w, map[string]any{"node": s.nodeOut(n), "detect": st.Detect, "agentUnavailable": s.agentUnavailable()})
}

type nodeIn struct {
	Name          string `json:"name"`
	Remark        string `json:"remark"`
	Mode          string `json:"mode"`
	SSHHost       string `json:"sshHost"`
	SSHPort       int    `json:"sshPort"`
	SSHAuth       string `json:"sshAuth"`
	SSHUser       string `json:"sshUser"`
	SSHPassword   string `json:"sshPassword"`
	SSHKey        string `json:"sshKey"`
	SSHPassphrase string `json:"sshPassphrase"`
	CredID        int64  `json:"credId"`
	SaveCred      bool   `json:"saveCred"`
	CredName      string `json:"credName"`
	// 端口策略：可分配范围与保留端口（创建时不填则使用系统设置中的默认值）
	PortRange     *string `json:"portRange"`
	ReservedPorts *string `json:"reservedPorts"`
	// 以下仅更新已初始化节点时使用
	WANIP4 *string  `json:"wanIp4"`
	WANIP6 *string  `json:"wanIp6"`
	DNS    []string `json:"dns"`
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9.:-]{1,253}$`)

// applyConn 校验并写入连接参数；返回是否需要重启节点循环。
// agentUnavailable 返回不能使用 Agent 方式的原因；为空表示可以使用。
// Agent 通过 WSS 主动连接控制端，访问地址必须是 HTTPS。
func (s *Server) agentUnavailable() string {
	st := s.settings()
	switch {
	case st.URL == "":
		return "尚未设置控制端访问地址，请在「系统设置」中填写 https:// 开头的访问地址"
	case !strings.HasPrefix(st.URL, "https://"):
		return "控制端访问地址 " + st.URL + " 不是 HTTPS。Agent 通过 WSS 连接控制端，需要 HTTPS：请先配置反向代理（Nginx / Caddy 等）提供 HTTPS，再在「系统设置」中把访问地址改为 https:// 开头的地址；或改用 SSH 方式接入"
	}
	return ""
}

func (s *Server) applyConn(n *store.Node, in *nodeIn) (bool, error) {
	if in.Mode != "agent" && in.Mode != "ssh" {
		return false, bad("连接方式无效")
	}
	// 新建或改为 Agent 方式时检查（已接入的 Agent 节点不受影响）
	if in.Mode == "agent" && (n.ID == 0 || n.Mode != "agent") {
		if why := s.agentUnavailable(); why != "" {
			return false, bad("%s", why)
		}
	}
	changed := n.Mode != in.Mode
	n.Mode = in.Mode
	if in.Mode == "agent" {
		return changed, nil
	}
	in.SSHHost = strings.TrimSpace(in.SSHHost)
	if !hostRe.MatchString(in.SSHHost) {
		return false, bad("SSH 地址无效")
	}
	if in.SSHPort == 0 {
		in.SSHPort = 22
	}
	if in.SSHPort < 1 || in.SSHPort > 65535 {
		return false, bad("SSH 端口无效")
	}
	if n.SSHHost != in.SSHHost || n.SSHPort != in.SSHPort {
		n.HostKey = "" // 地址变化后需要重新确认指纹
		changed = true
	}
	n.SSHHost, n.SSHPort = in.SSHHost, in.SSHPort
	switch in.SSHAuth {
	case "cred":
		if _, err := s.db.Credential(in.CredID); err != nil {
			return false, bad("请选择凭证")
		}
		id := in.CredID
		n.CredID, n.SSHSecretEnc, n.SSHPassEnc = &id, nil, nil
	case "password", "key":
		if in.SSHUser == "" {
			in.SSHUser = "root"
		}
		secretText := in.SSHPassword
		if in.SSHAuth == "key" {
			secretText = in.SSHKey
			if secretText != "" {
				if _, _, err := sshx.KeyInfo(secretText, in.SSHPassphrase); err != nil {
					return false, bad("%v", err)
				}
			}
		}
		if secretText == "" && (n.SSHAuth != in.SSHAuth || len(n.SSHSecretEnc) == 0) {
			if in.SSHAuth == "key" {
				return false, bad("请粘贴私钥")
			}
			return false, bad("请输入密码")
		}
		if in.SaveCred && secretText != "" {
			c := &store.Credential{Name: strings.TrimSpace(in.CredName), Type: in.SSHAuth, Username: in.SSHUser, SecretEnc: s.box.Seal(secretText), PassphraseEnc: s.box.Seal(in.SSHPassphrase)}
			if c.Name == "" {
				c.Name = n.Name + " 凭证"
			}
			if c.Type == "key" {
				c.KeyAlgo, c.KeyFP, _ = sshx.KeyInfo(secretText, in.SSHPassphrase)
			}
			if err := s.db.SaveCredential(c); err != nil {
				return false, err
			}
			id := c.ID
			n.SSHAuth, n.CredID, n.SSHUser, n.SSHSecretEnc, n.SSHPassEnc = "cred", &id, in.SSHUser, nil, nil
			return true, nil
		}
		n.CredID = nil
		n.SSHUser = in.SSHUser
		if secretText != "" {
			n.SSHSecretEnc, n.SSHPassEnc = s.box.Seal(secretText), s.box.Seal(in.SSHPassphrase)
		}
	default:
		return false, bad("认证方式无效")
	}
	n.SSHAuth = in.SSHAuth
	return true, nil
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	var in nodeIn
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 40 {
		jsonErr(w, http.StatusBadRequest, "请输入节点名称（不超过 40 个字符）")
		return
	}
	n := &store.Node{Name: in.Name, Remark: in.Remark, SSHPort: 22, SSHUser: "root"}
	st := s.settings()
	rng, reserved := st.DefaultPortRange, st.DefaultReservedPorts
	if in.PortRange != nil {
		rng = *in.PortRange
	}
	if in.ReservedPorts != nil {
		reserved = *in.ReservedPorts
	}
	np, err := parseNodePorts(rng, reserved)
	if err != nil {
		fail(w, err)
		return
	}
	n.Ports = np
	if _, err := s.applyConn(n, &in); err != nil {
		fail(w, err)
		return
	}
	if err := s.db.SaveNode(n); err != nil {
		fail(w, bad("%v", err))
		return
	}
	s.mgr.AddNode(n.ID)
	res := map[string]any{"node": s.nodeOut(n)}
	if n.Mode == "agent" {
		res["install"] = s.joinCommand(n.ID)
	}
	writeJSON(w, res)
}

func (s *Server) updateNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var in nodeIn
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) != "" {
		n.Name = strings.TrimSpace(in.Name)
	}
	n.Remark = in.Remark
	if in.PortRange != nil || in.ReservedPorts != nil {
		rng, reserved := n.Ports.Range, n.Ports.Reserved
		if in.PortRange != nil {
			rng = *in.PortRange
		}
		if in.ReservedPorts != nil {
			reserved = *in.ReservedPorts
		}
		np, err := parseNodePorts(rng, reserved)
		if err == nil {
			err = s.checkNodePortsChange(n.ID, np)
		}
		if err != nil {
			fail(w, err)
			return
		}
		n.Ports = np
	}
	restart, err := s.applyConn(n, &in)
	if err != nil {
		fail(w, err)
		return
	}
	push := false
	oldIP := n.Net.WANIP4
	if n.Initialized {
		if in.WANIP4 != nil {
			n.Net.WANIP4 = strings.TrimSpace(*in.WANIP4)
		}
		if in.WANIP6 != nil {
			n.Net.WANIP6 = strings.TrimSpace(*in.WANIP6)
		}
		if in.DNS != nil {
			n.Net.DNS = in.DNS
			push = true
		}
	}
	if err := s.db.SaveNode(n); err != nil {
		fail(w, bad("%v", err))
		return
	}
	if restart {
		s.mgr.AddNode(n.ID)
	}
	if push {
		s.mgr.Push(n.ID)
	}
	// 公网 IP 变化：Web 转发站点的解析记录指向新地址
	if n.Net.WANIP4 != oldIP && n.Net.WANIP4 != "" {
		go s.resyncNodeDNS(n.ID, n.Net.WANIP4)
	}
	writeJSON(w, map[string]any{"node": s.nodeOut(n)})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	cleaned := false
	if sites, err := s.db.Sites(0, n.ID); err == nil {
		s.deleteSitesDNS(sites)
	}
	if r.URL.Query().Get("cleanup") == "1" {
		if _, st, _ := s.db.NodeWeb(n.ID); st.Location != "" {
			s.removeWeb(r.Context(), n.ID, st.Deployed)
		}
		err := s.mgr.WithDriver(n.ID, func(d manager.Driver) error {
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			return d.Cleanup(ctx)
		})
		cleaned = err == nil
	}
	s.mgr.RemoveNode(n.ID)
	if _, err := s.db.Exec(`DELETE FROM nodes WHERE id=?`, n.ID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true, "cleaned": cleaned})
}

// ---------------- SSH 测试与指纹 ----------------

func (s *Server) sshTest(w http.ResponseWriter, r *http.Request) {
	var in nodeIn
	if !readJSON(w, r, &in) {
		return
	}
	in.Mode = "ssh"
	n := &store.Node{Name: "test", SSHPort: 22, SSHUser: "root"}
	in.SaveCred = false
	if _, err := s.applyConn(n, &in); err != nil {
		fail(w, err)
		return
	}
	cfg, err := s.mgr.SSHConfig(n)
	if err != nil {
		fail(w, bad("%v", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	writeJSON(w, sshx.Test(ctx, cfg))
}

func (s *Server) nodeSSHTest(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	if n.Mode != "ssh" {
		jsonErr(w, http.StatusBadRequest, "该节点不是 SSH 方式")
		return
	}
	cfg, err := s.mgr.SSHConfig(n)
	if err != nil {
		fail(w, bad("%v", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res := sshx.Test(ctx, cfg)
	if res.OK {
		// 只保证循环在运行；已在线时重建 runtime 会把节点状态重置为离线
		s.mgr.EnsureNode(n.ID)
	}
	writeJSON(w, res)
}

func (s *Server) trustHostKey(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		HostKey string `json:"hostKey"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !sshx.ValidHostKey(req.HostKey) {
		jsonErr(w, http.StatusBadRequest, "主机公钥格式无效")
		return
	}
	n.HostKey = strings.TrimSpace(req.HostKey)
	if err := s.db.SaveNode(n); err != nil {
		fail(w, err)
		return
	}
	s.db.AddEvent("info", 0, n.ID, "已信任节点 "+n.Name+" 的主机指纹")
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------------- Agent 接入令牌 ----------------

func (s *Server) agentURL() string {
	st := s.settings()
	return fmt.Sprintf("%s:%d", strings.TrimRight(st.URL, "/"), st.Port)
}

type installInfo struct {
	Token     string `json:"token"`
	Command   string `json:"command"`
	ExpiresAt int64  `json:"expiresAt"`
}

func (s *Server) joinCommand(nodeID int64) *installInfo {
	tok := secret.ReadableToken()
	exp := time.Now().Add(30 * time.Minute).Unix()
	s.db.Exec(`DELETE FROM join_tokens WHERE node_id=?`, nodeID)
	s.db.Exec(`INSERT INTO join_tokens(token_hash,node_id,expires_at) VALUES(?,?,?)`, secret.Hash(tok), nodeID, exp)
	base := s.agentURL()
	curl := "curl -fsSL"
	pinArg := ""
	if pin := s.CertFingerprint(); pin != "" {
		curl = "curl -fsSLk --pinnedpubkey '" + pin + "'"
		pinArg = " \\\n  --pin '" + pin + "'"
	}
	cmd := fmt.Sprintf("%s %s/agent/install.sh | bash -s -- \\\n  --server %s \\\n  --token %s%s", curl, base, base, tok, pinArg)
	return &installInfo{Token: tok, Command: cmd, ExpiresAt: exp}
}

func (s *Server) newJoinToken(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	if n.Mode != "agent" {
		jsonErr(w, http.StatusBadRequest, "该节点不是 Agent 方式")
		return
	}
	writeJSON(w, s.joinCommand(n.ID))
}

// ---------------- 检测与网络初始化 ----------------

func (s *Server) detectNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var d *proto.Detect
	err = s.mgr.WithDriver(n.ID, func(drv manager.Driver) error {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		d, err = drv.Detect(ctx)
		return err
	})
	if err != nil {
		fail(w, bad("检测失败：%v", err))
		return
	}
	s.mgr.SetDetect(n.ID, d)
	s.db.Exec(`UPDATE nodes SET hostname=?, pve_version=? WHERE id=?`, d.Hostname, d.PVEVersion, n.ID)
	writeJSON(w, d)
}

type initIn struct {
	WANIface    string   `json:"wanIface"`
	WANIP4      string   `json:"wanIp4"`
	WANIP6      string   `json:"wanIp6"`
	Bridge      string   `json:"bridge"`
	NewBridge   bool     `json:"newBridge"`
	CIDR4       string   `json:"cidr4"`
	AddrMode    string   `json:"addrMode"` // append | replace（网段与网桥现有地址不同时）
	DHCP        bool     `json:"dhcp"`     // 由本系统管理 dnsmasq
	InstallDHCP bool     `json:"installDhcp"`
	DNS         []string `json:"dns"`
	IPv6        bool     `json:"ipv6"`
	CIDR6       string   `json:"cidr6"`
}

var bridgeNameRe = regexp.MustCompile(`^vmbr\d{1,4}$`)

// buildNet 校验初始化参数并生成 NetConfig。
func (s *Server) buildNet(n *store.Node, in initIn, d *proto.Detect) (proto.NetConfig, error) {
	var nc proto.NetConfig
	if !nodeops.ValidIface(in.WANIface) {
		return nc, bad("请选择公网网口")
	}
	if _, err := netip.ParseAddr(in.WANIP4); err != nil && in.WANIP4 != "" {
		return nc, bad("公网 IPv4 无效")
	}
	if in.WANIP6 != "" {
		if _, err := netip.ParseAddr(in.WANIP6); err != nil {
			return nc, bad("公网 IPv6 无效")
		}
	}
	p, err := netip.ParsePrefix(strings.TrimSpace(in.CIDR4))
	if err != nil || !p.Addr().Is4() || p.Bits() < 16 || p.Bits() > 28 {
		return nc, bad("内网网段无效（需为 /16 – /28 的 IPv4 网段）")
	}
	p = p.Masked()
	if !p.Addr().IsPrivate() {
		return nc, bad("内网网段应使用私有地址（10.0.0.0/8、172.16.0.0/12、192.168.0.0/16）")
	}
	nc = proto.NetConfig{WANIface: in.WANIface, WANIP4: in.WANIP4, WANIP6: in.WANIP6, CIDR4: p.String(), DNS: in.DNS}
	// 与公网网口地址冲突检查
	if d != nil {
		for _, nic := range d.NICs {
			if nic.Name != in.WANIface {
				continue
			}
			for _, c := range nic.IP4 {
				if q, err := netip.ParsePrefix(c); err == nil && q.Masked().Overlaps(p) {
					return nc, bad("内网网段与公网网口 %s 的地址 %s 冲突", nic.Name, c)
				}
			}
		}
	}
	gw, _ := nodeops.Gateway(nc.CIDR4)
	if in.NewBridge {
		if !bridgeNameRe.MatchString(in.Bridge) {
			return nc, bad("新网桥名称需为 vmbrN 格式")
		}
		if d != nil {
			for _, b := range d.Bridges {
				if b.Name == in.Bridge {
					return nc, bad("网桥 %s 已存在", in.Bridge)
				}
			}
		}
		nc.Bridge, nc.AddrMode, nc.GW4 = in.Bridge, "new", gw
	} else {
		if !nodeops.ValidIface(in.Bridge) {
			return nc, bad("请选择内网网桥")
		}
		if in.Bridge == in.WANIface {
			return nc, bad("内网网桥不能与公网网口相同")
		}
		nc.Bridge = in.Bridge
		var existing []string
		if d != nil {
			found := false
			for _, b := range d.Bridges {
				if b.Name == in.Bridge {
					found, existing = true, b.CIDRs
				}
			}
			if !found {
				return nc, bad("网桥 %s 不存在", in.Bridge)
			}
		}
		for _, c := range existing {
			if q, err := netip.ParsePrefix(c); err == nil && p.Contains(q.Addr()) && q.Bits() == p.Bits() {
				nc.AddrMode, nc.GW4 = "existing", c
			}
		}
		if nc.AddrMode == "" {
			if len(existing) == 0 {
				nc.AddrMode, nc.GW4 = "append", gw
			} else if in.AddrMode == "replace" {
				nc.AddrMode, nc.GW4 = "replace", gw
			} else {
				nc.AddrMode, nc.GW4 = "append", gw
			}
		}
	}
	if in.IPv6 {
		if in.WANIP6 == "" {
			return nc, bad("启用 IPv6 需要公网 IPv6 地址")
		}
		c6 := strings.TrimSpace(in.CIDR6)
		if c6 == "" {
			c6 = randomULA()
		}
		q, err := netip.ParsePrefix(c6)
		if err != nil || !q.Addr().Is6() || q.Bits() != 64 {
			return nc, bad("内网 IPv6 网段需为 /64")
		}
		nc.CIDR6 = q.Masked().String()
		nc.GW6, _ = nodeops.Gateway(nc.CIDR6)
	}
	switch {
	case in.DHCP:
		nc.DHCP = "managed"
	case d != nil && d.DHCP != "":
		nc.DHCP = "external"
	default:
		nc.DHCP = "none"
	}
	if len(nc.DNS) == 0 {
		nc.DNS = []string{"223.5.5.5", "1.1.1.1"}
	}
	for _, dns := range nc.DNS {
		if _, err := netip.ParseAddr(strings.TrimSpace(dns)); err != nil {
			return nc, bad("DNS 地址 %q 无效", dns)
		}
	}
	return nc, nil
}

func randomULA() string {
	b := make([]byte, 5)
	rand.Read(b)
	return fmt.Sprintf("fd%02x:%02x%02x:%02x%02x:1::/64", b[0], b[1], b[2], b[3], b[4])
}

// initNode 执行网络初始化，以 NDJSON 流式返回每一步的进度。
func (s *Server) initNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.db.Node(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var in initIn
	if !readJSON(w, r, &in) {
		return
	}
	d := s.mgr.Status(n.ID).Detect
	if d == nil {
		jsonErr(w, http.StatusBadRequest, "请先检测节点环境")
		return
	}
	nc, err := s.buildNet(n, in, d)
	if err != nil {
		fail(w, err)
		return
	}
	if n.Initialized && n.Net.CIDR4 != nc.CIDR4 {
		var cnt int
		s.db.QueryRow(`SELECT COUNT(*) FROM vms WHERE node_id=? AND ipv4<>''`, n.ID).Scan(&cnt)
		if cnt > 0 {
			jsonErr(w, http.StatusBadRequest, fmt.Sprintf("已有 %d 台虚拟机分配了 %s 内的地址，不能更改网段", cnt, n.Net.CIDR4))
			return
		}
	}
	if q := r.URL.Query().Get("preview"); q == "1" {
		writeJSON(w, nc)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		w.Write(append(b, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err = s.mgr.WithDriver(n.ID, func(drv manager.Driver) error {
		return drv.InitNetwork(ctx, proto.NetInit{Net: nc, InstallDHCP: in.DHCP}, func(st proto.Step) {
			send(map[string]any{"step": st})
		})
	})
	if err != nil {
		s.db.AddEvent("error", 0, n.ID, fmt.Sprintf("节点 %s 网络初始化失败：%v", n.Name, err))
		send(map[string]any{"error": err.Error()})
		return
	}
	n.Net = nc
	n.Initialized = true
	if err := s.db.SaveNode(n); err != nil {
		send(map[string]any{"error": err.Error()})
		return
	}
	s.db.BumpState(n.ID)
	err = s.mgr.RunOnce(ctx, n.ID)
	st := proto.Step{Name: "加载转发规则并同步虚拟机", OK: err == nil}
	if err != nil {
		st.Error = err.Error()
	}
	send(map[string]any{"step": st})
	s.db.AddEvent("info", 0, n.ID, "节点 "+n.Name+" 网络初始化完成")
	send(map[string]any{"done": true})
}

func (s *Server) syncNode(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if _, err := s.db.Node(id); err != nil {
		fail(w, err)
		return
	}
	if r.URL.Query().Get("push") == "1" {
		s.db.BumpState(id)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.mgr.RunOnce(ctx, id); err != nil {
		fail(w, bad("%v", err))
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func atoiDefault(s string, d int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return d
}
