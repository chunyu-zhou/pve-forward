package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"pveforward/internal/manager"
	"pveforward/internal/proto"
	"pveforward/internal/secret"
)

// agentJoin 用一次性令牌换取节点长期密钥。
func (s *Server) agentJoin(w http.ResponseWriter, r *http.Request) {
	var req proto.JoinRequest
	if !readJSON(w, r, &req) {
		return
	}
	ip := clientIP(r)
	if s.limiter.Blocked("join:"+ip, 10, 15*time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "尝试次数过多")
		return
	}
	var nodeID, exp int64
	err := s.db.QueryRow(`SELECT node_id, expires_at FROM join_tokens WHERE token_hash=?`, secret.Hash(strings.ToUpper(strings.TrimSpace(req.Token)))).Scan(&nodeID, &exp)
	if err != nil || exp < time.Now().Unix() {
		s.limiter.Fail("join:" + ip)
		jsonErr(w, http.StatusUnauthorized, "接入令牌无效或已过期，请在控制端重新生成")
		return
	}
	n, err := s.db.Node(nodeID)
	if err != nil || n.Mode != "agent" {
		jsonErr(w, http.StatusUnauthorized, "节点不存在或不是 Agent 方式")
		return
	}
	sec := secret.RandomToken(32)
	s.db.Exec(`DELETE FROM join_tokens WHERE node_id=?`, nodeID)
	s.db.Exec(`UPDATE nodes SET agent_secret_hash=?, hostname=? WHERE id=?`, secret.Hash(sec), req.Hostname, nodeID)
	s.db.AddEvent("info", 0, nodeID, fmt.Sprintf("节点 %s 的 Agent 已接入（%s，%s）", n.Name, req.Hostname, ip))
	writeJSON(w, proto.JoinResponse{NodeID: nodeID, Secret: sec})
}

// agentWS 处理 Agent 的长连接。认证：Authorization: Bearer <节点ID>.<密钥>
func (s *Server) agentWS(w http.ResponseWriter, r *http.Request) {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	idStr, sec, ok := strings.Cut(auth, ".")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	n, err := s.db.Node(id)
	if !ok || err != nil || n.Mode != "agent" || n.AgentSecretHash == "" || secret.Hash(sec) != n.AgentSecretHash {
		jsonErr(w, http.StatusUnauthorized, "Agent 认证失败")
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var hello proto.Hello
	hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
	err = wsjson.Read(hctx, ws, &hello)
	hcancel()
	if err != nil {
		ws.Close(websocket.StatusPolicyViolation, "需要先发送 hello")
		return
	}
	s.db.Exec(`UPDATE nodes SET agent_version=?, hostname=?, last_seen=? WHERE id=?`, hello.Version, hello.Hostname, time.Now().Unix(), id)
	log.Printf("Agent 已连接：节点 %s（%s，v%s）", n.Name, hello.Hostname, hello.Version)

	conn := manager.NewAgentConn(ws)
	go conn.Serve(ctx)
	go func() { // 心跳
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-conn.Done():
				return
			case <-t.C:
				pctx, pc := context.WithTimeout(ctx, 10*time.Second)
				err := ws.Ping(pctx)
				pc()
				if err != nil {
					ws.Close(websocket.StatusGoingAway, "心跳超时")
					return
				}
			}
		}
	}()
	s.mgr.AttachAgent(ctx, id, conn)
	log.Printf("Agent 已断开：节点 %s", n.Name)
}

// agentDownload 提供 Agent 二进制（与控制端一同发布，位于 <数据目录>/agent 或程序同目录）。
func (s *Server) agentDownload(w http.ResponseWriter, r *http.Request) {
	arch := r.PathValue("arch")
	if arch != "amd64" && arch != "arm64" {
		http.Error(w, "不支持的架构", http.StatusNotFound)
		return
	}
	name := "pve-forward-agent-linux-" + arch
	exe, _ := os.Executable()
	for _, dir := range []string{filepath.Join(s.dataDir, "agent"), filepath.Dir(exe), "/usr/local/lib/pve-forward"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, p)
			return
		}
	}
	http.Error(w, "控制端未找到 "+name, http.StatusNotFound)
}

func (s *Server) agentInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Write([]byte(agentInstallSh))
}

const agentInstallSh = `#!/bin/bash
# PVE Forward Agent 安装脚本
set -euo pipefail
SERVER="" TOKEN="" PIN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="$2"; shift 2;;
    --token) TOKEN="$2"; shift 2;;
    --pin) PIN="$2"; shift 2;;
    *) echo "未知参数 $1" >&2; exit 1;;
  esac
done
[ -n "$SERVER" ] && [ -n "$TOKEN" ] || { echo "用法: install.sh --server URL --token TOKEN [--pin sha256//...]" >&2; exit 1; }
[ "$(id -u)" = 0 ] || { echo "请以 root 运行" >&2; exit 1; }
command -v pveversion >/dev/null || echo "警告：未检测到 Proxmox VE" >&2
case "$(uname -m)" in x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; *) echo "不支持的架构 $(uname -m)" >&2; exit 1;; esac
CURL=(curl -fsSL)
[ -n "$PIN" ] && CURL=(curl -fsSLk --pinnedpubkey "$PIN")
echo "下载 Agent ($ARCH) ..."
"${CURL[@]}" -o /usr/local/bin/pve-forward-agent.new "$SERVER/agent/download/$ARCH"
chmod 755 /usr/local/bin/pve-forward-agent.new
mv -f /usr/local/bin/pve-forward-agent.new /usr/local/bin/pve-forward-agent
echo "接入控制端 ..."
/usr/local/bin/pve-forward-agent join --server "$SERVER" --token "$TOKEN" ${PIN:+--pin "$PIN"}
cat > /etc/systemd/system/pve-forward-agent.service <<'EOF'
[Unit]
Description=PVE Forward Agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/pve-forward-agent run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable -q pve-forward-agent.service
systemctl restart pve-forward-agent.service
echo "完成：Agent 已启动，请回到控制端页面继续。"
`
