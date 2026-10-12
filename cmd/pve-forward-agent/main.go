// pve-forward-agent 运行在 PVE 节点上，主动连接控制端并执行下发的操作。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"pveforward"
	"pveforward/internal/manager"
	"pveforward/internal/nodeops"
	"pveforward/internal/proto"
)

// Version 与控制端一致，来自仓库根目录 version.json。
var Version = pveforward.Version

var configPath = "/etc/pve-forward-agent/agent.json"

type config struct {
	Server string `json:"server"` // https://panel.example.com:5601
	Pin    string `json:"pin"`    // sha256//base64，自签名证书时使用
	NodeID int64  `json:"nodeId"`
	Secret string `json:"secret"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "PVE Forward Agent %s\n用法:\n  pve-forward-agent join --server URL --token TOKEN [--pin sha256//...]\n  pve-forward-agent run\n  pve-forward-agent version\n", Version)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "join":
		fs := flag.NewFlagSet("join", flag.ExitOnError)
		server := fs.String("server", "", "控制端地址")
		token := fs.String("token", "", "接入令牌")
		pin := fs.String("pin", "", "控制端证书公钥固定值")
		fs.StringVar(&configPath, "config", configPath, "配置文件")
		fs.Parse(os.Args[2:])
		if err := join(strings.TrimRight(*server, "/"), *token, *pin); err != nil {
			log.Fatalf("接入失败: %v", err)
		}
		fmt.Println("已接入控制端")
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		fs.StringVar(&configPath, "config", configPath, "配置文件")
		fs.Parse(os.Args[2:])
		run()
	case "version":
		fmt.Println(Version)
	default:
		fmt.Fprintln(os.Stderr, "未知命令", os.Args[1])
		os.Exit(2)
	}
}

// httpClient 返回校验控制端证书的客户端：设置 pin 时按公钥固定值校验，否则使用系统 CA。
func tlsConfig(pin string) *tls.Config {
	if pin == "" {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // 改为下面的公钥固定校验
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("控制端未提供证书")
			}
			c, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
			if "sha256//"+base64.StdEncoding.EncodeToString(h[:]) != pin {
				return errors.New("控制端证书公钥与固定值不一致")
			}
			return nil
		},
	}
}

func httpClient(pin string) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig(pin), Proxy: http.ProxyFromEnvironment}}
}

func join(server, token, pin string) error {
	if server == "" || token == "" {
		return errors.New("缺少 --server 或 --token")
	}
	host, _ := os.Hostname()
	body, _ := json.Marshal(proto.JoinRequest{Token: token, Hostname: host})
	resp, err := httpClient(pin).Post(server+"/agent/join", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		json.Unmarshal(data, &e)
		return fmt.Errorf("%s (%d)", e.Error, resp.StatusCode)
	}
	var jr proto.JoinResponse
	if err := json.Unmarshal(data, &jr); err != nil {
		return err
	}
	cfg := config{Server: server, Pin: pin, NodeID: jr.NodeID, Secret: jr.Secret}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(configPath, b, 0o600)
}

func run() {
	b, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("读取配置失败（请先执行 join）: %v", err)
	}
	var cfg config
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatalf("配置文件无效: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := session(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Printf("与控制端断开: %v，%v 后重连", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func session(ctx context.Context, cfg config) error {
	u := strings.Replace(cfg.Server, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1) + "/agent/ws"
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+strconv.FormatInt(cfg.NodeID, 10)+"."+cfg.Secret)
	ws, resp, err := websocket.Dial(dctx, u, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig(cfg.Pin), Proxy: http.ProxyFromEnvironment}},
		HTTPHeader: hdr, CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return errors.New("认证失败：节点可能已在控制端删除，请重新执行安装命令")
		}
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)
	host, _ := os.Hostname()
	if err := wsjson.Write(ctx, ws, proto.Hello{Version: Version, Hostname: host}); err != nil {
		return err
	}
	log.Printf("已连接控制端 %s", cfg.Server)
	return manager.ServeAgentRPC(ctx, ws, nodeops.LocalExecutor{}, Version)
}
