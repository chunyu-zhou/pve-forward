package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"pveforward/internal/ports"
	"pveforward/internal/proto"
	"pveforward/internal/store"
)

type ruleOut struct {
	*store.Rule
	Owner      string `json:"owner"`
	NodeName   string `json:"nodeName"`
	NodeStatus string `json:"nodeStatus"`
	Entry4     string `json:"entry4"`
	Entry6     string `json:"entry6"`
	Public     bool   `json:"public"` // 管理员创建的公共规则
	VMName     string `json:"vmName"`
	VMVMID     int    `json:"vmVmid"`
	VMShared   bool   `json:"vmShared"`
	Target4    string `json:"target4"`
	Target6    string `json:"target6"`
	Conns      int    `json:"conns"`
	ConnsTCP   int    `json:"connsTcp"`
	ConnsUDP   int    `json:"connsUdp"`
	Up24       int64  `json:"up24"`
	Down24     int64  `json:"down24"`
}

func (s *Server) ruleRows(uid int64) ([]*ruleOut, error) {
	rules, err := s.db.Rules(uid, 0)
	if err != nil {
		return nil, err
	}
	users := map[int64]string{}
	admins := map[int64]bool{}
	if us, err := s.db.Users(); err == nil {
		for _, u := range us {
			users[u.ID] = u.Username
			admins[u.ID] = u.Role == "admin"
		}
	}
	nodes := map[int64]*store.Node{}
	if ns, err := s.db.Nodes(); err == nil {
		for _, n := range ns {
			nodes[n.ID] = n
		}
	}
	vms := map[int64]*store.VM{}
	if vs, err := s.db.VMs(0); err == nil {
		for _, v := range vs {
			vms[v.ID] = v
		}
	}
	t24 := map[int64][2]int64{}
	rows, err := s.db.Query(`SELECT rule_id, SUM(bytes_up), SUM(bytes_down) FROM traffic WHERE hour>=? GROUP BY rule_id`, time.Now().Add(-24*time.Hour).Unix())
	if err == nil {
		for rows.Next() {
			var id, u, d int64
			rows.Scan(&id, &u, &d)
			t24[id] = [2]int64{u, d}
		}
		rows.Close()
	}
	status := map[int64]string{}
	reports := map[int64]*proto.Report{}
	for id := range nodes {
		st := s.mgr.Status(id)
		status[id] = st.State
		reports[id] = st.Report
	}
	res := []*ruleOut{}
	for _, rl := range rules {
		o := &ruleOut{Rule: rl, Owner: users[rl.UserID], Public: admins[rl.UserID], Up24: t24[rl.ID][0], Down24: t24[rl.ID][1]}
		if o.Public {
			o.Owner = "公共"
		}
		if n := nodes[rl.NodeID]; n != nil {
			o.NodeName, o.NodeStatus = n.Name, status[n.ID]
			o.Entry4, o.Entry6 = n.Net.WANIP4, n.Net.WANIP6
			if rep := reports[n.ID]; rep != nil {
				o.ConnsTCP, o.ConnsUDP = ruleConns(rep, rl.ID)
				o.Conns = o.ConnsTCP + o.ConnsUDP
			}
		}
		if v := vms[rl.VMID]; v != nil {
			o.VMName, o.VMVMID, o.VMShared, o.Target4, o.Target6 = v.Name, v.VMID, v.Shared, v.IPv4, v.IPv6
		}
		if rl.Enabled && o.Error == "" {
			o.Error = s.mgr.RuleError(rl.NodeID, rl.ID)
		}
		if rl.Enabled && o.Error == "" && o.NodeStatus == "offline" {
			o.Error = "节点 " + o.NodeName + " 离线，规则状态未知"
		}
		res = append(res, o)
	}
	return res, nil
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uid := int64(0)
	if u.Role != "admin" {
		uid = u.ID
	}
	rows, err := s.ruleRows(uid)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, rows)
}

type ruleIn struct {
	UserID     int64  `json:"userId"`
	NodeID     int64  `json:"nodeId"`
	Port       int    `json:"port"`
	Proto      string `json:"proto"`
	IPVer      string `json:"ipVer"`
	VMID       int64  `json:"vmId"`
	TargetPort int    `json:"targetPort"`
	Remark     string `json:"remark"`
	Enabled    *bool  `json:"enabled"`
}

func (s *Server) saveRule(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	id := pathID(r)
	var in ruleIn
	if !readJSON(w, r, &in) {
		return
	}
	var old *store.Rule
	if id > 0 {
		var err error
		if old, err = s.db.Rule(id); err != nil || (me.Role != "admin" && old.UserID != me.ID) {
			jsonErr(w, http.StatusNotFound, "规则不存在")
			return
		}
	}
	// 只切换启用状态
	if old != nil && in.NodeID == 0 && in.Enabled != nil {
		s.db.Exec(`UPDATE rules SET enabled=?, error_msg='' WHERE id=?`, *in.Enabled, id)
		s.mgr.Push(old.NodeID)
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	// 所属用户：普通用户只能是自己
	if me.Role != "admin" {
		in.UserID = me.ID
	} else if old != nil && in.UserID == 0 {
		in.UserID = old.UserID
	}
	owner, err := s.db.User(in.UserID)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "请选择所属用户")
		return
	}
	if owner.Role == "admin" {
		owner = me // 公共规则统一记在当前管理员名下
	}
	if err := s.validateRule(owner, old, &in); err != nil {
		fail(w, err)
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	in.Remark = strings.TrimSpace(in.Remark)
	if len([]rune(in.Remark)) > 40 {
		jsonErr(w, http.StatusBadRequest, "备注不超过 40 个字符")
		return
	}
	if old == nil {
		_, err = s.db.Exec(`INSERT INTO rules(user_id,node_id,listen_port,proto,ip_version,vm_id,target_port,remark,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			owner.ID, in.NodeID, in.Port, in.Proto, in.IPVer, in.VMID, in.TargetPort, in.Remark, enabled, time.Now().Unix())
	} else {
		_, err = s.db.Exec(`UPDATE rules SET user_id=?,node_id=?,listen_port=?,proto=?,ip_version=?,vm_id=?,target_port=?,remark=?,enabled=?,error_msg='' WHERE id=?`,
			owner.ID, in.NodeID, in.Port, in.Proto, in.IPVer, in.VMID, in.TargetPort, in.Remark, enabled, id)
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			jsonErr(w, http.StatusBadRequest, "该端口已被其他规则使用")
			return
		}
		fail(w, err)
		return
	}
	push := []int64{in.NodeID}
	if old != nil {
		push = append(push, old.NodeID)
	}
	s.mgr.Push(push...)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) validateRule(owner *store.User, old *store.Rule, in *ruleIn) error {
	if old == nil && owner.MaxRules > 0 {
		var cnt int
		s.db.QueryRow(`SELECT COUNT(*) FROM rules WHERE user_id=?`, owner.ID).Scan(&cnt)
		if cnt >= owner.MaxRules {
			return bad("已达到规则上限（%d 条）", owner.MaxRules)
		}
	}
	n, err := s.db.Node(in.NodeID)
	if err != nil || !n.Initialized {
		return bad("请选择已初始化的节点")
	}
	if in.Proto != "tcp" && in.Proto != "udp" && in.Proto != "both" {
		return bad("协议无效")
	}
	if in.IPVer != "v4" && in.IPVer != "v6" && in.IPVer != "both" {
		return bad("IP 版本无效")
	}
	if in.TargetPort < 1 || in.TargetPort > 65535 {
		return bad("目标端口无效")
	}
	public := owner.Role == "admin"
	if in.Port < 1 || in.Port > 65535 {
		return bad("监听端口无效")
	}
	if s.webPorts(n.ID)[in.Port] {
		return bad("端口 %d 是节点 %s 的 Web 转发端口，请使用 Web 转发", in.Port, n.Name)
	}
	// 监听端口：普通用户必须使用分配给自己的端口；公共规则（管理员）可使用任意端口，包括保留端口，
	// 但不能占用已分配给用户的端口
	if public {
		var holder string
		if s.db.QueryRow(`SELECT u.username FROM allocated_ports a JOIN users u ON u.id=a.user_id WHERE a.node_id=? AND a.port=?`, n.ID, in.Port).Scan(&holder) == nil {
			return bad("端口 %d 已分配给用户 %s，请使用其他端口或先收回分配", in.Port, holder)
		}
	} else {
		mine, err := s.db.UserPorts(owner.ID, n.ID)
		if err != nil {
			return err
		}
		if !mine[in.Port] {
			return bad("端口 %d 未分配给用户 %s", in.Port, owner.Username)
		}
		if ports.MustSet(n.Ports.Reserved).Contains(in.Port) {
			return bad("端口 %d 是保留端口", in.Port)
		}
	}
	samePort := old != nil && old.NodeID == in.NodeID && old.Port == in.Port
	if !samePort {
		var other int64
		if s.db.QueryRow(`SELECT id FROM rules WHERE node_id=? AND listen_port=?`, n.ID, in.Port).Scan(&other) == nil {
			return bad("端口 %d 已被其他规则使用", in.Port)
		}
		// 公共规则允许使用宿主机也在监听的端口：公网网口进入的流量由 DNAT 转到虚拟机
		if st := s.mgr.Status(n.ID); st.Report != nil && !public {
			for _, p := range st.Report.Listen {
				if p == in.Port {
					return bad("端口 %d 已被宿主机进程占用", in.Port)
				}
			}
		}
	}
	// 目标虚拟机：同节点，属于该用户或为公共虚拟机
	v, err := s.db.VM(in.VMID)
	if err != nil || v.NodeID != n.ID || !v.Present {
		return bad("请选择该节点上的虚拟机")
	}
	if !public && !v.Shared && (v.UserID == nil || *v.UserID != owner.ID) {
		return bad("只能选择 %s 的虚拟机或公共虚拟机", owner.Username)
	}
	if in.IPVer != "v6" && v.IPv4 == "" {
		return bad("虚拟机 %d 没有 IPv4 地址", v.VMID)
	}
	if in.IPVer != "v4" {
		if n.Net.CIDR6 == "" || n.Net.WANIP6 == "" {
			return bad("节点 %s 未启用 IPv6", n.Name)
		}
		if v.IPv6 == "" {
			return bad("虚拟机 %d 没有 IPv6 地址", v.VMID)
		}
	}
	return nil
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	rl, err := s.db.Rule(pathID(r))
	if err != nil || (me.Role != "admin" && rl.UserID != me.ID) {
		jsonErr(w, http.StatusNotFound, "规则不存在")
		return
	}
	s.db.Exec(`DELETE FROM rules WHERE id=?`, rl.ID)
	s.mgr.Push(rl.NodeID)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) batchRules(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	var req struct {
		IDs    []int64 `json:"ids"`
		Action string  `json:"action"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	nodes := []int64{}
	n := 0
	for _, id := range req.IDs {
		rl, err := s.db.Rule(id)
		if err != nil || (me.Role != "admin" && rl.UserID != me.ID) {
			continue
		}
		switch req.Action {
		case "enable", "disable":
			s.db.Exec(`UPDATE rules SET enabled=?, error_msg='' WHERE id=?`, req.Action == "enable", id)
		case "delete":
			s.db.Exec(`DELETE FROM rules WHERE id=?`, id)
		default:
			jsonErr(w, http.StatusBadRequest, "未知操作")
			return
		}
		nodes = append(nodes, rl.NodeID)
		n++
	}
	s.mgr.Push(nodes...)
	writeJSON(w, map[string]int{"count": n})
}

// ruleOptions 返回规则表单的可选项：节点、可用端口、可选虚拟机。
func (s *Server) ruleOptions(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	uid := queryInt(r, "user")
	if me.Role != "admin" {
		uid = me.ID
	}
	nodeID := queryInt(r, "node")
	keep := int(queryInt(r, "keep")) // 编辑时保留当前端口
	res := map[string]any{}
	public := false
	if owner, err := s.db.User(uid); err == nil && owner.Role == "admin" {
		public = true
	}
	res["public"] = public

	type nodeOpt struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		WANIP4 string `json:"wanIp4"`
		WANIP6 string `json:"wanIp6"`
		V6     bool   `json:"v6"`
	}
	nodes := []nodeOpt{}
	q := `SELECT DISTINCT node_id FROM allocated_ports WHERE user_id=?`
	if public {
		q = `SELECT id FROM nodes WHERE initialized=1 OR id=-?`
	}
	rows, err := s.db.Query(q, uid)
	if err != nil {
		fail(w, err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		n, err := s.db.Node(id)
		if err != nil || !n.Initialized {
			continue
		}
		nodes = append(nodes, nodeOpt{n.ID, n.Name, s.mgr.Status(n.ID).State, n.Net.WANIP4, n.Net.WANIP6, n.Net.CIDR6 != "" && n.Net.WANIP6 != ""})
	}
	res["nodes"] = nodes

	if nodeID > 0 {
		n, err := s.db.Node(nodeID)
		if err != nil {
			fail(w, err)
			return
		}
		mine, _ := s.db.UserPorts(uid, nodeID)
		used := map[int]bool{}
		rules, _ := s.db.Rules(0, nodeID)
		for _, rl := range rules {
			used[rl.Port] = true
		}
		busy := map[int]bool{}
		if st := s.mgr.Status(nodeID); st.Report != nil {
			for _, p := range st.Report.Listen {
				busy[p] = true
			}
		}
		reserved := ports.MustSet(n.Ports.Reserved)
		webPorts := s.webPorts(nodeID)
		free := []int{}
		nUsed, nBusy := 0, 0
		for p := range mine {
			switch {
			case p == keep:
				free = append(free, p)
			case used[p]:
				nUsed++
			case busy[p]:
				nBusy++
			case reserved.Contains(p), webPorts[p]:
			default:
				free = append(free, p)
			}
		}
		sort.Ints(free)
		res["ports"] = free
		res["total"] = len(mine)
		res["usedCount"] = nUsed
		res["busyCount"] = nBusy
		res["reservedCount"] = len(mine) - len(free) - nUsed - nBusy
		type vmOpt struct {
			ID     int64  `json:"id"`
			VMID   int    `json:"vmid"`
			Name   string `json:"name"`
			Status string `json:"status"`
			IPv4   string `json:"ipv4"`
			IPv6   string `json:"ipv6"`
			Shared bool   `json:"shared"`
		}
		vms, _ := s.db.VMs(nodeID)
		opts := []vmOpt{}
		for _, v := range vms {
			if !v.Present || !(public || v.Shared || (v.UserID != nil && *v.UserID == uid)) {
				continue
			}
			opts = append(opts, vmOpt{v.ID, v.VMID, v.Name, v.Status, v.IPv4, v.IPv6, v.Shared})
		}
		res["vms"] = opts
	}
	writeJSON(w, res)
}
