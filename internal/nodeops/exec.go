// Package nodeops 实现在 PVE 宿主机上执行的全部操作。
// 通过 Executor 抽象执行方式：Agent 在本机执行，SSH 模式由控制端远程执行。
package nodeops

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"unicode/utf8"
)

// Executor 在节点上以 root 权限运行 bash 脚本。
type Executor interface {
	// Run 将 script 作为 bash 脚本执行，返回标准输出；退出码非 0 时返回错误（包含标准错误）。
	Run(ctx context.Context, script string) ([]byte, error)
}

// LocalExecutor 在本机执行（Agent 使用）。
type LocalExecutor struct{}

type outputLogKey struct{}

func WithOutputLog(ctx context.Context, log func(string)) context.Context {
	return context.WithValue(ctx, outputLogKey{}, log)
}

func OutputLog(ctx context.Context) func(string) {
	log, _ := ctx.Value(outputLogKey{}).(func(string))
	return log
}

// CommandOutput bounds retained output while forwarding each chunk as it arrives.
// The same instance accepts stdout and stderr, serializing callbacks.
type CommandOutput struct {
	mu      sync.Mutex
	data    []byte
	log     func(string)
	pending []byte
}

func NewCommandOutput(log func(string)) *CommandOutput { return &CommandOutput{log: log} }

func (w *CommandOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	const limit = 256 << 10
	if w.log != nil {
		for rest := p; len(rest) > 0; {
			n := min(len(rest), 4096-len(w.pending))
			w.pending = append(w.pending, rest[:n]...)
			rest = rest[n:]
			end := 0
			for end < len(w.pending) && utf8.FullRune(w.pending[end:]) {
				_, size := utf8.DecodeRune(w.pending[end:])
				end += size
			}
			if end > 0 {
				w.log(strings.ToValidUTF8(string(w.pending[:end]), "�"))
				w.pending = append(w.pending[:0], w.pending[end:]...)
			}
		}
	}
	if len(p) >= limit {
		w.data = append(w.data[:0], p[len(p)-limit:]...)
	} else {
		if len(w.data)+len(p) > limit {
			w.data = w.data[len(w.data)+len(p)-limit:]
		}
		w.data = append(w.data, p...)
	}
	return len(p), nil
}

func (w *CommandOutput) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 && w.log != nil {
		w.log(strings.ToValidUTF8(string(w.pending), "�"))
		w.pending = nil
	}
	return append([]byte(nil), w.data...)
}

func (LocalExecutor) Run(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	if log := OutputLog(ctx); log != nil {
		out := NewCommandOutput(log)
		cmd.Stdout, cmd.Stderr = out, out
		err := cmd.Run()
		if err != nil {
			return out.Bytes(), &ExecError{Err: err, Stderr: string(out.Bytes())}
		}
		return out.Bytes(), nil
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), &ExecError{Err: err, Stderr: errb.String()}
	}
	return out.Bytes(), nil
}

type ExecError struct {
	Err    error
	Stderr string
}

func (e *ExecError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 600 {
		msg = msg[len(msg)-600:]
	}
	if msg == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%v: %s", e.Err, msg)
}

// writeFileCmd 生成写文件的 shell 片段（base64 传输，避免转义问题）。
func writeFileCmd(path string, data []byte, mode string) string {
	b64 := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("mkdir -p %s && printf '%%s' '%s' | base64 -d > %s.tmp && chmod %s %s.tmp && mv -f %s.tmp %s\n",
		shq(dirOf(path)), b64, shq(path), mode, shq(path), shq(path), shq(path))
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

// shq 对字符串做 shell 单引号转义。
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sections 解析以 "##name" 行分隔的输出。
func sections(out []byte) map[string]string {
	res := map[string]string{}
	var cur string
	var buf strings.Builder
	flush := func() {
		if cur != "" {
			res[cur] = buf.String()
		}
		buf.Reset()
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "##") && !strings.Contains(line, " ") {
			flush()
			cur = strings.TrimPrefix(line, "##")
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	return res
}
