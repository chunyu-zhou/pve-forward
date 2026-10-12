// pve-forward 控制端
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pveforward/internal/manager"
	"pveforward/internal/secret"
	"pveforward/internal/server"
	"pveforward/internal/store"
	"pveforward/internal/update"
	"pveforward/web"
)

func main() {
	fs := flag.NewFlagSet("pve-forward", flag.ExitOnError)
	dataDir := fs.String("data", "/var/lib/pve-forward", "数据目录")
	listen := fs.String("listen", "", "监听地址（覆盖系统设置中的端口），如 :5000")
	tlsMode := fs.String("tls", "", "证书方式（覆盖系统设置）：self | acme | proxy（HTTP，由反向代理处理 HTTPS）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `PVE Forward 控制端 %s

用法:
  pve-forward [run] [-data 目录] [-listen 地址] [-tls self|acme|proxy]   启动服务
  pve-forward setup-token [-data 目录]            重新生成初始化令牌
  pve-forward reset-password 用户名 新密码 [-data 目录]
  pve-forward version
`, server.Version)
	}
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var pos []string
	for len(args) > 0 {
		fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	if cmd == "version" {
		fmt.Println(server.Version)
		return
	}
	if cmd != "run" && cmd != "setup-token" && cmd != "reset-password" {
		fs.Usage()
		os.Exit(2)
	}
	// Only the server command reads env. Maintenance commands must still work
	// when a container has stale or incomplete bootstrap configuration.
	if cmd == "run" {
		if err := applyRuntimeEnv(fs, os.Getenv); err != nil {
			log.Fatal(err)
		}
		// 数据目录中有更新的在线更新版本时切换过去（不返回）；新版本启动失败时回退
		update.Launch(*dataDir, server.Version)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	if abs, err := filepath.Abs(*dataDir); err == nil {
		*dataDir = abs
	}
	box, err := openMasterKey(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	db, err := store.Open(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	mgr := manager.New(db, box)
	srv := server.New(db, box, mgr, *dataDir, web.FS(), *listen)
	srv.TLSMode = *tlsMode

	switch cmd {
	case "setup-token":
		if db.Setting("setup_done", "") == "1" {
			fmt.Fprintln(os.Stderr, "控制端已初始化，无需令牌")
			os.Exit(1)
		}
		fmt.Println("初始化令牌：" + srv.NewSetupToken())
		return
	case "reset-password":
		if len(pos) != 2 || len(pos[1]) < 8 || len(pos[1]) > 72 {
			fmt.Fprintln(os.Stderr, "用法: pve-forward reset-password 用户名 新密码（8-72 字节）")
			os.Exit(1)
		}
		h, err := bcrypt.GenerateFromPassword([]byte(pos[1]), bcrypt.DefaultCost)
		if err != nil {
			log.Fatal("密码哈希生成失败")
		}
		res, err := db.Exec(`UPDATE users SET password_hash=? WHERE username=?`, string(h), pos[0])
		if err != nil {
			log.Fatal("重置密码失败")
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			fmt.Fprintln(os.Stderr, "用户不存在")
			os.Exit(1)
		}
		db.Exec(`DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE username=?)`, pos[0])
		fmt.Println("已重置密码")
		return
	case "run":
		if err := srv.BootstrapFromEnv(os.Getenv); err != nil {
			log.Fatal(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mgr.Start(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("PVE Forward 控制端 %s，数据目录 %s", server.Version, *dataDir)
	err = srv.Run(ctx)
	var upgrade *server.UpgradeRequest
	if errors.As(err, &upgrade) {
		// 在线更新：停止节点任务并等待当前操作结束，关闭数据库后以新程序替换当前进程
		stop()
		if !mgr.Wait(30 * time.Second) {
			log.Printf("在线更新：部分节点任务 30 秒内未结束，继续切换")
		}
		db.Close()
		log.Printf("在线更新：切换到 v%s", upgrade.Version)
		if err := update.Switch(upgrade.Path); err != nil {
			if merr := update.MarkFailed(*dataDir, upgrade.Version); merr != nil {
				log.Printf("在线更新：记录失败状态出错：%v", merr)
			}
			log.Fatalf("在线更新：无法启动 v%s：%v；已回退，重启后使用原版本", upgrade.Version, err)
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	// 正常停止：尚未确认的新版本不计为启动失败
	if err := update.Release(*dataDir, server.Version); err != nil {
		log.Printf("在线更新：记录更新状态失败：%v", err)
	}
}

// applyRuntimeEnv preserves explicit flags, including -listen= and -tls= to
// opt back into database settings. Empty env values leave flag defaults alone.
func applyRuntimeEnv(fs *flag.FlagSet, getenv func(string) string) error {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for _, binding := range [][2]string{{"listen", "PVE_FORWARD_LISTEN"}, {"tls", "PVE_FORWARD_TLS"}} {
		if !explicit[binding[0]] {
			if value := getenv(binding[1]); value != "" {
				if err := fs.Set(binding[0], value); err != nil {
					return fmt.Errorf("无法应用 %s", binding[1])
				}
			}
		}
	}
	switch fs.Lookup("tls").Value.String() {
	case "", "self", "acme", "proxy":
		return nil
	default:
		return errors.New("PVE_FORWARD_TLS / -tls 必须是 self、acme 或 proxy")
	}
}

// Open the key before creating a database: an existing database without its
// matching key is a restore error, not permission to silently generate a key.
func openMasterKey(dataDir string) (*secret.Box, error) {
	keyPath := filepath.Join(dataDir, "master.key")
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		for _, name := range []string{"data.db", "data.db-wal", "data.db-shm"} {
			if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
				return nil, errors.New("发现已有数据库但缺失 master.key；拒绝生成新主密钥，请恢复与数据库配套的 master.key（数据目录必须整体持久化/恢复）")
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}
	return secret.Open(keyPath)
}
