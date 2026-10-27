package store

import (
	"errors"
	"testing"

	"pveforward/internal/proto"
)

func TestSyncVMsPreservesKnownName(t *testing.T) {
	for _, tc := range []struct {
		name, reportedName, reportedStatus, wantName, wantStatus string
	}{
		{"空名称", "", "running", "web01", "running"},
		{"空白名称与状态", " \t\n", " \t", "web01", "unknown"},
		{"空状态", "web01", "", "web01", "unknown"},
		{"真实名称更新", "web02", "stopped", "web02", "stopped"},
		{"名称与状态规范化", " web03 ", " RUNNING ", "web03", "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			n := &Node{Name: "pve-test", Mode: "agent"}
			if err := d.SaveNode(n); err != nil {
				t.Fatal(err)
			}
			v := proto.VMInfo{VMID: 101, Type: "qemu", Name: "web01", Status: "running", MAC: "bc:24:11:00:00:01"}
			if _, err := d.SyncVMs(n.ID, []proto.VMInfo{v}); err != nil {
				t.Fatal(err)
			}
			before, err := d.VMs(n.ID)
			if err != nil || len(before) != 1 {
				t.Fatalf("初始虚拟机: %+v, %v", before, err)
			}
			v.Name, v.Status = tc.reportedName, tc.reportedStatus
			changed, err := d.SyncVMs(n.ID, []proto.VMInfo{v})
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Fatal("仅名称或状态变化不应触发期望状态更新")
			}
			got, err := d.VM(before[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tc.wantName || got.Status != tc.wantStatus || !got.Present {
				t.Fatalf("同步结果: %+v，期望名称 %q、状态 %q 且存在", got, tc.wantName, tc.wantStatus)
			}
		})
	}
}

func TestSyncVMsNewVMWithoutNameOrStatus(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	n := &Node{Name: "pve-test", Mode: "agent"}
	if err := d.SaveNode(n); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SyncVMs(n.ID, []proto.VMInfo{{VMID: 101, Type: "qemu", Name: " \t"}}); err != nil {
		t.Fatal(err)
	}
	vms, err := d.VMs(n.ID)
	if err != nil || len(vms) != 1 {
		t.Fatalf("新虚拟机: %+v, %v", vms, err)
	}
	if v := vms[0]; v.Name != "" || v.Status != "unknown" || !v.Present {
		t.Fatalf("未命名虚拟机不应生成占位名称或空状态: %+v", v)
	}
}

func TestSyncVMsMissingVMs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial []bool
		empty   bool
	}{
		{name: "部分列表", partial: []bool{true}},
		{name: "空部分列表", partial: []bool{true}, empty: true},
		{name: "默认完整列表"},
		{name: "显式完整列表", partial: []bool{false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			n := &Node{Name: "pve-test", Mode: "agent"}
			other := &Node{Name: "pve-other", Mode: "agent"}
			for _, node := range []*Node{n, other} {
				if err := d.SaveNode(node); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES(1,'alice','test',1)`); err != nil {
				t.Fatal(err)
			}
			list := []proto.VMInfo{
				{VMID: 100, Type: "qemu", Name: "seen", Status: "running"},
				{VMID: 101, Type: "qemu", Name: "owned", Status: "running", IP4: "10.0.0.11", IP6: "fd00::11"},
				{VMID: 102, Type: "lxc", Name: "shared", Status: "stopped", IP4: "10.0.0.12", IP6: "fd00::12"},
				{VMID: 103, Type: "qemu", Name: "unassigned", Status: "running", IP4: "10.0.0.13"},
				{VMID: 104, Type: "qemu", Name: "referenced", Status: "running", IP4: "10.0.0.14"},
				{VMID: 105, Type: "qemu", Name: "already-deleted", Status: "running"},
			}
			if _, err := d.SyncVMs(n.ID, list); err != nil {
				t.Fatal(err)
			}
			if _, err := d.SyncVMs(other.ID, []proto.VMInfo{list[1]}); err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{
				`UPDATE vms SET user_id=1 WHERE vmid=101 AND node_id=?`,
				`UPDATE vms SET shared=1 WHERE vmid=102 AND node_id=?`,
				`UPDATE vms SET present=0,status='deleted',shared=1 WHERE vmid=105 AND node_id=?`,
				`INSERT INTO rules(user_id,node_id,listen_port,proto,ip_version,vm_id,target_port,created_at) SELECT 1,node_id,20000,'tcp','4',id,80,1 FROM vms WHERE vmid=104 AND node_id=?`,
			} {
				if _, err := d.Exec(statement, n.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := d.VMs(n.ID)
			if err != nil {
				t.Fatal(err)
			}
			report := []proto.VMInfo{{VMID: 100, Type: "qemu", Name: "seen", Status: "stopped"}}
			if tc.empty {
				report = nil
			}
			changed, err := d.SyncVMs(n.ID, report, tc.partial...)
			if err != nil {
				t.Fatal(err)
			}
			partial := len(tc.partial) > 0 && tc.partial[0]
			if changed == partial {
				t.Fatalf("期望状态变化标记 = %t，部分同步 = %t", changed, partial)
			}
			for _, old := range before {
				got, err := d.VM(old.ID)
				if old.VMID == 103 && !partial {
					if !errors.Is(err, ErrNotFound) {
						t.Fatalf("完整同步应删除无引用且未分配的虚拟机: %+v, %v", got, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				wantStatus, wantPresent := "unknown", true
				if old.VMID == 100 && !tc.empty {
					wantStatus = "stopped"
				} else if !partial || old.VMID == 105 {
					wantStatus, wantPresent = "deleted", false
				}
				if got.Status != wantStatus || got.Present != wantPresent {
					t.Errorf("VM %d: %+v，期望状态 %q、存在 %t", old.VMID, got, wantStatus, wantPresent)
				}
				if got.Name != old.Name || got.IPv4 != old.IPv4 || got.IPv6 != old.IPv6 || got.Shared != old.Shared || (got.UserID == nil) != (old.UserID == nil) || (old.UserID != nil && *got.UserID != *old.UserID) {
					t.Errorf("VM %d 的名称、地址或分配被意外修改: 原值 %+v，现值 %+v", old.VMID, old, got)
				}
			}
			others, err := d.VMs(other.ID)
			if err != nil || len(others) != 1 || !others[0].Present || others[0].Status != "running" {
				t.Fatalf("同步不应影响其他节点: %+v, %v", others, err)
			}
			var refs int
			if err := d.QueryRow(`SELECT COUNT(*) FROM rules WHERE node_id=?`, n.ID).Scan(&refs); err != nil || refs != 1 {
				t.Fatalf("同步不应删除引用规则: 数量 %d, %v", refs, err)
			}
		})
	}
}
