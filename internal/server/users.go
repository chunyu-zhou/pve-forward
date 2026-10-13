package server

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"

	"pveforward/internal/ports"
	"pveforward/internal/store"
)

type allocIn struct {
	ID     int64  `json:"id"`
	NodeID int64  `json:"nodeId"`
	Kind   string `json:"kind"`
	Spec   string `json:"spec"`
}

type userIn struct {
	Username    string    `json:"username"`
	Password    string    `json:"password"`
	Enabled     bool      `json:"enabled"`
	QuotaGB     float64   `json:"quotaGb"`
	MaxRules    int       `json:"maxRules"`
	Remark      string    `json:"remark"`
	Allocations []allocIn `json:"allocations"`
	VMIDs       []int64   `json:"vmIds"`
}

type userOut struct {
	*store.User
	Allocations []*store.Allocation `json:"allocations"`
	UsedPorts   map[int64][]int     `json:"usedPorts"` // 节点 → 已被规则使用的端口
	VMIDs       []int64             `json:"vmIds"`
	Rules       int                 `json:"rules"`
	ErrRules    int                 `json:"errRules"`
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.db.Users()
	if err != nil {
		fail(w, err)
		return
	}
	allocs, _ := s.db.Allocations(0)
	rules, _ := s.db.Rules(0, 0)
	vms, _ := s.db.VMs(0)
	res := []*userOut{}
	for _, u := range users {
		if u.Role == "admin" {
			continue
		}
		o := &userOut{User: u, Allocations: []*store.Allocation{}, UsedPorts: map[int64][]int{}, VMIDs: []int64{}}
		for _, a := range allocs {
			if a.UserID == u.ID {
				o.Allocations = append(o.Allocations, a)
			}
		}
		for _, rl := range rules {
			if rl.UserID == u.ID {
				o.Rules++
				o.UsedPorts[rl.NodeID] = append(o.UsedPorts[rl.NodeID], rl.Port)
				if rl.Enabled && (rl.Error != "" || s.mgr.RuleError(rl.NodeID, rl.ID) != "") {
					o.ErrRules++
				}
			}
		}
		for _, v := range vms {
			if v.UserID != nil && *v.UserID == u.ID {
				o.VMIDs = append(o.VMIDs, v.ID)
			}
		}
		res = append(res, o)
	}
	writeJSON(w, res)
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.db.User(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	allocs, _ := s.db.Allocations(u.ID)
	vms, _ := s.db.VMs(0)
	o := &userOut{User: u, Allocations: allocs, VMIDs: []int64{}}
	if o.Allocations == nil {
		o.Allocations = []*store.Allocation{}
	}
	for _, v := range vms {
		if v.UserID != nil && *v.UserID == u.ID {
			o.VMIDs = append(o.VMIDs, v.ID)
		}
	}
	writeJSON(w, o)
}

// checkAlloc 即时校验一条端口分配（表单中“添加”按钮）。
func (s *Server) checkAlloc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID  int64     `json:"userId"`
		Alloc   allocIn   `json:"alloc"`
		Pending []allocIn `json:"pending"` // 表单中已添加但未保存的分配
	}
	if !readJSON(w, r, &req) {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	spec, list, err := s.validateAlloc(tx, req.UserID, req.Alloc, req.Pending)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"spec": spec, "count": len(list)})
}

func (s *Server) validateAlloc(tx *sql.Tx, uid int64, a allocIn, pending []allocIn) (string, []int, error) {
	n, err := s.db.Node(a.NodeID)
	if err != nil {
		return "", nil, bad("节点不存在")
	}
	if !n.Initialized {
		return "", nil, bad("节点 %s 尚未完成网络初始化", n.Name)
	}
	spec, list, err := ports.Parse(a.Kind, a.Spec)
	if err != nil {
		return "", nil, bad("%v", err)
	}
	rng, reserved := ports.MustSet(n.Ports.Range), ports.MustSet(n.Ports.Reserved)
	panelPort := s.settings().Port
	webPorts := s.webPorts(n.ID)
	mine := map[int]bool{}
	for _, p := range pending {
		if p.NodeID != a.NodeID || p.ID != 0 && p.ID == a.ID {
			continue
		}
		if _, ps, err := ports.Parse(p.Kind, p.Spec); err == nil {
			for _, x := range ps {
				mine[x] = true
			}
		}
	}
	for _, p := range list {
		if !rng.Contains(p) {
			return "", nil, bad("端口 %d 不在 %s 的可分配范围 %s 内", p, n.Name, n.Ports.Range)
		}
		if reserved.Contains(p) || p == panelPort {
			return "", nil, bad("端口 %d 是 %s 的保留端口", p, n.Name)
		}
		if webPorts[p] {
			return "", nil, bad("端口 %d 是 %s 的 Web 转发端口", p, n.Name)
		}
		if mine[p] {
			return "", nil, bad("端口 %d 已在当前用户 %s 的分配中", p, n.Name)
		}
		var owner string
		var ouid int64
		err := tx.QueryRow(`SELECT u.username, u.id FROM allocated_ports a JOIN users u ON u.id=a.user_id WHERE a.node_id=? AND a.port=?`, a.NodeID, p).Scan(&owner, &ouid)
		if err == nil && (ouid != uid || uid == 0) {
			return "", nil, bad("%s 的端口 %d 已分配给用户 %s", n.Name, p, owner)
		}
	}
	return spec, list, nil
}

func (s *Server) saveUser(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var in userIn
	if !readJSON(w, r, &in) {
		return
	}
	pushNodes, err := s.writeUser(id, &in)
	if err != nil {
		fail(w, err)
		return
	}
	s.mgr.Push(pushNodes...)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) writeUser(id int64, in *userIn) ([]int64, error) {
	if in.MaxRules < 0 || in.QuotaGB < 0 {
		return nil, bad("配额不能为负数")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	quota := int64(math.Round(in.QuotaGB * (1 << 30)))
	push := map[int64]bool{}

	var u *store.User
	if id == 0 {
		if !usernameRe.MatchString(in.Username) {
			return nil, bad("用户名需为 3-20 位字母、数字或下划线")
		}
		if len(in.Password) < 8 {
			return nil, bad("密码至少 8 位")
		}
		h, _ := hashPassword(in.Password)
		u = &store.User{Username: in.Username, PasswordHash: h, Role: "user", Enabled: in.Enabled, QuotaBytes: quota, MaxRules: in.MaxRules, Remark: in.Remark}
		if err := s.db.CreateUser(tx, u); err != nil {
			return nil, bad("%v", err)
		}
	} else {
		if u, err = s.db.User(id); err != nil {
			return nil, err
		}
		if u.Role == "admin" {
			return nil, bad("不能在这里编辑管理员")
		}
		if u.Enabled != in.Enabled || u.QuotaBytes != quota {
			// 启用状态或配额变化影响该用户全部节点
			rows, _ := tx.Query(`SELECT DISTINCT node_id FROM rules WHERE user_id=?`, id)
			for rows != nil && rows.Next() {
				var n int64
				rows.Scan(&n)
				push[n] = true
			}
			if rows != nil {
				rows.Close()
			}
		}
		if _, err := tx.Exec(`UPDATE users SET enabled=?, quota_bytes=?, max_rules=?, remark=? WHERE id=?`, in.Enabled, quota, in.MaxRules, in.Remark, id); err != nil {
			return nil, err
		}
		if in.Password != "" {
			if len(in.Password) < 8 || len(in.Password) > 72 {
				return nil, bad("密码须为 8–72 字节")
			}
			h, err := hashPassword(in.Password)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`UPDATE users SET password_hash=?,totp_pending_enc=NULL,totp_pending_expires=0 WHERE id=?`, h, id); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, id); err != nil {
				return nil, err
			}
		}
		if !in.Enabled {
			tx.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
		}
	}

	// ---- 端口分配 ----
	keep := map[int64]bool{}
	for _, a := range in.Allocations {
		if a.ID > 0 {
			keep[a.ID] = true
		}
	}
	old, err := s.db.Allocations(u.ID)
	if err != nil {
		return nil, err
	}
	for _, a := range old {
		if !keep[a.ID] {
			if _, err := tx.Exec(`DELETE FROM port_allocations WHERE id=? AND user_id=?`, a.ID, u.ID); err != nil {
				return nil, err
			}
			push[a.NodeID] = true
		}
	}
	for _, a := range in.Allocations {
		if a.ID > 0 {
			continue
		}
		spec, list, err := s.validateAlloc(tx, u.ID, a, nil)
		if err != nil {
			return nil, err
		}
		res, err := tx.Exec(`INSERT INTO port_allocations(user_id,node_id,kind,spec) VALUES(?,?,?,?)`, u.ID, a.NodeID, a.Kind, spec)
		if err != nil {
			return nil, err
		}
		aid, _ := res.LastInsertId()
		for _, p := range list {
			if _, err := tx.Exec(`INSERT INTO allocated_ports(node_id,port,user_id,allocation_id) VALUES(?,?,?,?)`, a.NodeID, p, u.ID, aid); err != nil {
				return nil, bad("端口 %d 重复分配", p)
			}
		}
	}
	// 端口被收回时，删除使用这些端口的规则
	res, err := tx.Exec(`DELETE FROM rules WHERE user_id=? AND NOT EXISTS (SELECT 1 FROM allocated_ports a WHERE a.node_id=rules.node_id AND a.port=rules.listen_port AND a.user_id=rules.user_id)`, u.ID)
	if err != nil {
		return nil, err
	}
	removed, _ := res.RowsAffected()

	// ---- 虚拟机 ----
	want := map[int64]bool{}
	for _, v := range in.VMIDs {
		want[v] = true
	}
	vms, err := s.db.VMs(0)
	if err != nil {
		return nil, err
	}
	for _, v := range vms {
		owned := v.UserID != nil && *v.UserID == u.ID
		switch {
		case want[v.ID] && !owned:
			if v.Shared {
				return nil, bad("虚拟机 %d 是公共虚拟机，不能分配给用户", v.VMID)
			}
			if v.UserID != nil {
				var name string
				tx.QueryRow(`SELECT username FROM users WHERE id=?`, *v.UserID).Scan(&name)
				return nil, bad("虚拟机 %d 已分配给用户 %s", v.VMID, name)
			}
			if err := s.assignVM(tx, v, &u.ID, false); err != nil {
				return nil, err
			}
			push[v.NodeID] = true
		case !want[v.ID] && owned:
			tx.Exec(`UPDATE vms SET user_id=NULL WHERE id=?`, v.ID)
			push[v.NodeID] = true
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if removed > 0 {
		s.db.AddEvent("warn", u.ID, 0, fmt.Sprintf("%s 的 %d 条规则因端口被收回而删除", u.Username, removed))
	}
	var ids []int64
	for n := range push {
		ids = append(ids, n)
	}
	return ids, nil
}

// assignVM 设置虚拟机归属，无 IP 时从节点分配池取地址。
func (s *Server) assignVM(tx *sql.Tx, v *store.VM, uid *int64, shared bool) error {
	n, err := s.db.Node(v.NodeID)
	if err != nil {
		return err
	}
	if !n.Initialized {
		return bad("节点 %s 尚未完成网络初始化", n.Name)
	}
	if v.MAC == "" {
		return bad("虚拟机 %d 没有接入内网网桥 %s 的网卡", v.VMID, n.Net.Bridge)
	}
	// 内网地址属于虚拟机本身（节点上报的当前地址），分配时不改变；尚未识别到地址时，
	// 虚拟机开机并获得地址后自动记录
	_, err = tx.Exec(`UPDATE vms SET user_id=?, shared=? WHERE id=?`, uid, shared, v.ID)
	return err
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.db.User(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	if u.Role == "admin" {
		jsonErr(w, http.StatusBadRequest, "不能删除管理员")
		return
	}
	nodes := s.userNodes(u.ID)
	// 删除用户的站点前先删除自动添加的解析记录
	if sites, err := s.db.Sites(u.ID, 0); err == nil {
		s.deleteSitesDNS(sites)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id=?`, u.ID); err != nil {
		fail(w, err)
		return
	}
	s.mgr.Push(nodes...)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) userNodes(uid int64) []int64 {
	var ids []int64
	rows, err := s.db.Query(`SELECT DISTINCT node_id FROM rules WHERE user_id=? UNION SELECT DISTINCT node_id FROM vms WHERE user_id=?
		UNION SELECT DISTINCT node_id FROM sites WHERE user_id=?`, uid, uid, uid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func (s *Server) userAction(w http.ResponseWriter, r *http.Request) {
	u, err := s.db.User(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Action   string `json:"action"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	switch req.Action {
	case "enable", "disable":
		s.db.Exec(`UPDATE users SET enabled=? WHERE id=?`, req.Action == "enable", u.ID)
		if req.Action == "disable" {
			s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID)
		}
		s.mgr.PushUser(u.ID)
	case "reset-password":
		if len(req.Password) < 8 || len(req.Password) > 72 {
			jsonErr(w, http.StatusBadRequest, "密码须为 8–72 字节")
			return
		}
		h, err := hashPassword(req.Password)
		if err != nil {
			fail(w, err)
			return
		}
		tx, err := s.db.Begin()
		if err != nil {
			fail(w, err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`UPDATE users SET password_hash=?,totp_pending_enc=NULL,totp_pending_expires=0 WHERE id=?`, h, u.ID); err != nil {
			fail(w, err)
			return
		}
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID); err != nil {
			fail(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			fail(w, err)
			return
		}
	case "reset-2fa": // 用户丢失手机时由管理员解除
		if err := s.db.DisableTOTP(u.ID); err != nil {
			fail(w, err)
			return
		}
		s.db.AddEvent("warn", u.ID, 0, "管理员已解除 "+u.Username+" 的两步验证")
	case "reset-traffic":
		s.db.Exec(`UPDATE users SET used_bytes=0 WHERE id=?`, u.ID)
		s.mgr.PushUser(u.ID)
	default:
		jsonErr(w, http.StatusBadRequest, "未知操作")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
