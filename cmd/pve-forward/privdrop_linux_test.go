//go:build linux

package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestChownTree(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "tls"), 0o700)
	os.WriteFile(filepath.Join(root, "master.key"), []byte("k"), 0o600)
	os.WriteFile(filepath.Join(root, "tls", "cert.pem"), []byte("c"), 0o600)
	os.Symlink("/etc/passwd", filepath.Join(root, "link"))

	var changed []string
	lchown := func(p string, uid, gid int) error {
		if uid != 10001 || gid != 10001 {
			t.Fatalf("uid/gid %d:%d", uid, gid)
		}
		rel, _ := filepath.Rel(root, p)
		changed = append(changed, rel)
		return nil
	}
	// 当前用户拥有所有文件：目标为其他用户时全部修改（符号链接本身也改，不跟随）
	n, err := chownTree(root, 10001, 10001, lchown)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(changed)
	want := []string{".", "link", "master.key", "tls", "tls/cert.pem"}
	if n != len(want) || len(changed) != len(want) {
		t.Fatalf("changed %v", changed)
	}
	for i := range want {
		if changed[i] != want[i] {
			t.Fatalf("changed %v, want %v", changed, want)
		}
	}
	// 所有者已正确时不做修改
	changed = nil
	if n, err := chownTree(root, os.Getuid(), os.Getgid(), lchown); err != nil || n != 0 || len(changed) != 0 {
		t.Fatalf("n=%d err=%v changed=%v", n, err, changed)
	}
}

// 非 Docker 或非 root 启动时不做任何事。
func TestDropPrivilegesNoop(t *testing.T) {
	t.Setenv("PVE_FORWARD_RUNTIME", "")
	if err := dropPrivileges(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 {
		t.Setenv("PVE_FORWARD_RUNTIME", "docker")
		if err := dropPrivileges(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
}
