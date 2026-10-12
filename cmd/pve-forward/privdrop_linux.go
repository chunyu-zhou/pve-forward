//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Docker 镜像中运行控制端的默认用户（与 Dockerfile 中创建的用户一致）。
const dockerUID, dockerGID = 10001, 10001

// runAs 返回运行用户：默认 10001:10001，可用 PUID / PGID 指定
// （如群晖 NAS 上填写 DSM 用户的 UID/GID，使数据目录对该用户可读写）。PUID=0 表示以 root 运行。
func runAs(getenv func(string) string) (uid, gid int, err error) {
	uid, gid = dockerUID, dockerGID
	parse := func(name string, def int) (int, error) {
		v := getenv(name)
		if v == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1<<31-2 {
			return 0, fmt.Errorf("%s 必须是非负整数，当前为 %q", name, v)
		}
		return n, nil
	}
	if uid, err = parse("PUID", uid); err != nil {
		return
	}
	if gid, err = parse("PGID", gid); err != nil {
		return
	}
	return
}

// dropPrivileges 仅在 Docker 镜像中以 root 启动时生效：把数据目录（命名卷或宿主机目录）
// 的所有者改为运行用户（默认 10001），然后降为该用户运行。降权后进程不再持有任何特权。
// docker exec 执行的维护命令（如 reset-password）同样先降权，避免产生 root 所有的数据库文件。
func dropPrivileges(dataDir string) error {
	if os.Getenv("PVE_FORWARD_RUNTIME") != "docker" || os.Getuid() != 0 {
		return nil
	}
	uid, gid, err := runAs(os.Getenv)
	if err != nil {
		return err
	}
	if uid == 0 {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("创建数据目录 %s 失败：%w", dataDir, err)
	}
	n, err := chownTree(dataDir, uid, gid, os.Lchown)
	if err != nil {
		return fmt.Errorf("无法把数据目录 %s 的所有者改为 %d:%d：%w。如果 docker compose 中设置了 cap_drop: ALL，请删除这两行后重新启动", dataDir, uid, gid, err)
	}
	if n > 0 {
		log.Printf("已将数据目录 %s 中 %d 个文件的所有者改为 %d:%d", dataDir, n, uid, gid)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("降权失败：%w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("降权失败：%w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("降权失败：%w", err)
	}
	return nil
}

// chownTree 把 root 下所有者不是 uid:gid 的文件改过来（不跟随符号链接），返回修改的数量。
func chownTree(root string, uid, gid int, lchown func(string, int, int) error) (int, error) {
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid && int(st.Gid) == gid {
			return nil
		}
		if err := lchown(p, uid, gid); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}
