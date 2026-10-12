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
)

// Executor 在节点上以 root 权限运行 bash 脚本。
type Executor interface {
	// Run 将 script 作为 bash 脚本执行，返回标准输出；退出码非 0 时返回错误（包含标准错误）。
	Run(ctx context.Context, script string) ([]byte, error)
}

// LocalExecutor 在本机执行（Agent 使用）。
type LocalExecutor struct{}

func (LocalExecutor) Run(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
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
