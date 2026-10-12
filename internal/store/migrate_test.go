package store

import (
	"testing"
)

// 旧版本的节点没有端口策略：升级后可分配范围为 1-65535，保留端口沿用旧的列表。
func TestMigrateNodePorts(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO nodes(name,mode,net_json,ports_json,created_at) VALUES('old','agent','{"reserved":[22,8006,8007,8008]}','',1)`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	// 重新打开时执行升级；再次打开不重复修改
	for i := 0; i < 2; i++ {
		d, err = Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		ns, err := d.Nodes()
		if err != nil || len(ns) != 1 {
			t.Fatalf("%v %v", ns, err)
		}
		if p := ns[0].Ports; p.Range != "1-65535" || p.Reserved != "22,8006-8008" {
			t.Fatalf("升级后的端口策略 %+v", p)
		}
		if !ns[0].Ports.Allowed(9000) || ns[0].Ports.Allowed(8007) {
			t.Fatal("Allowed")
		}
		d.Close()
	}
}

// 没有 ports_json 列的旧数据库：打开时自动添加。
func TestMigrateAddsColumn(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`ALTER TABLE nodes DROP COLUMN ports_json`); err != nil {
		t.Fatal(err)
	}
	d.Exec(`INSERT INTO nodes(name,mode,net_json,created_at) VALUES('old','ssh','{}',1)`)
	d.Close()
	d, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ns, _ := d.Nodes()
	if len(ns) != 1 || ns[0].Ports.Range != "1-65535" || ns[0].Ports.Reserved != "" {
		t.Fatalf("%+v", ns)
	}
}
