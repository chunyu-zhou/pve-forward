package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"pveforward/internal/nodeops"
	"pveforward/internal/proto"
	"pveforward/internal/sshx"
)

// Driver 管理一个节点。SSH 与 Agent 两种连接方式各有一个实现，上层逻辑相同。
type Driver interface {
	Detect(ctx context.Context) (*proto.Detect, error)
	InitNetwork(ctx context.Context, in proto.NetInit, progress func(proto.Step)) error
	Apply(ctx context.Context, st *proto.DesiredState) (*proto.ApplyResult, error)
	Collect(ctx context.Context, st *proto.DesiredState) (*proto.Report, error)
	RRD(ctx context.Context, req proto.RRDReq) ([]proto.RRDPoint, error)
	// Web 在宿主机或入口虚拟机上操作 OpenResty（检测、安装、下发配置、移除）
	Web(ctx context.Context, req *proto.WebReq) (*proto.WebResult, error)
	Cleanup(ctx context.Context) error
	Close()
}

// ---------------- SSH ----------------

type sshDriver struct {
	ex  *sshx.Executor
	col nodeops.Collector
}

func NewSSHDriver(c sshx.Config) Driver { return &sshDriver{ex: sshx.NewExecutor(c)} }

func (d *sshDriver) Detect(ctx context.Context) (*proto.Detect, error) {
	return nodeops.Detect(ctx, d.ex)
}
func (d *sshDriver) InitNetwork(ctx context.Context, in proto.NetInit, p func(proto.Step)) error {
	return nodeops.InitNetwork(ctx, d.ex, in, p)
}
func (d *sshDriver) Apply(ctx context.Context, st *proto.DesiredState) (*proto.ApplyResult, error) {
	return nodeops.Apply(ctx, d.ex, st)
}
func (d *sshDriver) Collect(ctx context.Context, st *proto.DesiredState) (*proto.Report, error) {
	return d.col.Collect(ctx, d.ex, st)
}
func (d *sshDriver) RRD(ctx context.Context, req proto.RRDReq) ([]proto.RRDPoint, error) {
	return nodeops.RRD(ctx, d.ex, req)
}
func (d *sshDriver) Web(ctx context.Context, req *proto.WebReq) (*proto.WebResult, error) {
	return nodeops.Web(ctx, d.ex, req)
}
func (d *sshDriver) Cleanup(ctx context.Context) error { return nodeops.Cleanup(ctx, d.ex) }
func (d *sshDriver) Close()                            { d.ex.Close() }

// ---------------- Agent（WSS RPC） ----------------

// AgentConn 是与一个 Agent 的 WebSocket 连接，提供请求 / 响应式调用。
type AgentConn struct {
	ws       *websocket.Conn
	nextID   atomic.Int64
	mu       sync.Mutex
	pending  map[int64]chan proto.RPCMessage
	progress map[int64]func(proto.Step)
	done     chan struct{}
	err      error
}

func NewAgentConn(ws *websocket.Conn) *AgentConn {
	return &AgentConn{ws: ws, pending: map[int64]chan proto.RPCMessage{}, progress: map[int64]func(proto.Step){}, done: make(chan struct{})}
}

// Serve 读取消息直到连接断开。
func (c *AgentConn) Serve(ctx context.Context) error {
	defer close(c.done)
	for {
		var m proto.RPCMessage
		if err := wsjson.Read(ctx, c.ws, &m); err != nil {
			c.mu.Lock()
			c.err = err
			for _, ch := range c.pending {
				close(ch)
			}
			c.pending = map[int64]chan proto.RPCMessage{}
			c.mu.Unlock()
			return err
		}
		c.mu.Lock()
		if m.Progress != nil {
			if f := c.progress[m.ID]; f != nil {
				f(*m.Progress)
			}
		} else if ch, ok := c.pending[m.ID]; ok {
			ch <- m
			delete(c.pending, m.ID)
		}
		c.mu.Unlock()
	}
}

func (c *AgentConn) Done() <-chan struct{} { return c.done }

func (c *AgentConn) Call(ctx context.Context, method string, params, result any, progress func(proto.Step)) error {
	id := c.nextID.Add(1)
	var pb []byte
	if params != nil {
		var err error
		if pb, err = json.Marshal(params); err != nil {
			return err
		}
	}
	ch := make(chan proto.RPCMessage, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return errors.New("Agent 已断开")
	}
	c.pending[id] = ch
	if progress != nil {
		c.progress[id] = progress
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.progress, id)
		c.mu.Unlock()
	}()
	if err := wsjson.Write(ctx, c.ws, proto.RPCMessage{ID: id, Method: method, Params: pb}); err != nil {
		return fmt.Errorf("发送到 Agent 失败: %w", err)
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errors.New("Agent 已断开")
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *AgentConn) Close() { c.ws.Close(websocket.StatusNormalClosure, "") }

type agentDriver struct{ c *AgentConn }

func (d *agentDriver) Detect(ctx context.Context) (*proto.Detect, error) {
	var r proto.Detect
	return &r, d.c.Call(ctx, "detect", nil, &r, nil)
}
func (d *agentDriver) InitNetwork(ctx context.Context, in proto.NetInit, p func(proto.Step)) error {
	return d.c.Call(ctx, "init_network", in, nil, p)
}
func (d *agentDriver) Apply(ctx context.Context, st *proto.DesiredState) (*proto.ApplyResult, error) {
	var r proto.ApplyResult
	return &r, d.c.Call(ctx, "apply", st, &r, nil)
}
func (d *agentDriver) Collect(ctx context.Context, st *proto.DesiredState) (*proto.Report, error) {
	var r proto.Report
	return &r, d.c.Call(ctx, "collect", st, &r, nil)
}
func (d *agentDriver) RRD(ctx context.Context, req proto.RRDReq) ([]proto.RRDPoint, error) {
	var r []proto.RRDPoint
	return r, d.c.Call(ctx, "rrd", req, &r, nil)
}
func (d *agentDriver) Web(ctx context.Context, req *proto.WebReq) (*proto.WebResult, error) {
	var r proto.WebResult
	return &r, d.c.Call(ctx, "web", req, &r, nil)
}
func (d *agentDriver) Cleanup(ctx context.Context) error {
	return d.c.Call(ctx, "cleanup", nil, nil, nil)
}
func (d *agentDriver) Close() { d.c.Close() }

// ServeAgentRPC 在 Agent 端处理控制端的调用（Agent 进程使用）。
func ServeAgentRPC(ctx context.Context, ws *websocket.Conn, ex nodeops.Executor, version string) error {
	var col nodeops.Collector
	var wmu sync.Mutex
	write := func(m proto.RPCMessage) {
		wmu.Lock()
		defer wmu.Unlock()
		wsjson.Write(ctx, ws, m)
	}
	for {
		var m proto.RPCMessage
		if err := wsjson.Read(ctx, ws, &m); err != nil {
			return err
		}
		go func(m proto.RPCMessage) {
			// 网络初始化、采集等可能较慢，单独设置超时
			cctx, cancel := context.WithTimeout(ctx, timeoutFor(m.Method))
			defer cancel()
			res, err := handleAgentCall(cctx, m, ex, &col, version, func(s proto.Step) {
				st := s
				write(proto.RPCMessage{ID: m.ID, Progress: &st})
			})
			out := proto.RPCMessage{ID: m.ID}
			if err != nil {
				out.Error = err.Error()
			} else if res != nil {
				out.Result, _ = json.Marshal(res)
			}
			write(out)
		}(m)
	}
}

func handleAgentCall(ctx context.Context, m proto.RPCMessage, ex nodeops.Executor, col *nodeops.Collector, version string, progress func(proto.Step)) (any, error) {
	switch m.Method {
	case "detect":
		d, err := nodeops.Detect(ctx, ex)
		if d != nil {
			d.AgentVersion = version
		}
		return d, err
	case "init_network":
		var in proto.NetInit
		if err := json.Unmarshal(m.Params, &in); err != nil {
			return nil, err
		}
		return nil, nodeops.InitNetwork(ctx, ex, in, progress)
	case "apply":
		var st proto.DesiredState
		if err := json.Unmarshal(m.Params, &st); err != nil {
			return nil, err
		}
		return nodeops.Apply(ctx, ex, &st)
	case "collect":
		var st *proto.DesiredState
		if len(m.Params) > 0 && string(m.Params) != "null" {
			st = &proto.DesiredState{}
			if err := json.Unmarshal(m.Params, st); err != nil {
				return nil, err
			}
		}
		return col.Collect(ctx, ex, st)
	case "cleanup":
		return nil, nodeops.Cleanup(ctx, ex)
	case "web":
		var req proto.WebReq
		if err := json.Unmarshal(m.Params, &req); err != nil {
			return nil, err
		}
		return nodeops.Web(ctx, ex, &req)
	case "rrd":
		var req proto.RRDReq
		if err := json.Unmarshal(m.Params, &req); err != nil {
			return nil, err
		}
		return nodeops.RRD(ctx, ex, req)
	}
	return nil, fmt.Errorf("未知方法 %q", m.Method)
}
