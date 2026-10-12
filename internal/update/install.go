package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxAsset 限制单个下载文件大小，防止错误的镜像地址写满磁盘。
const maxAsset = 256 << 20

// Assets 返回在线更新需要的发布文件：本机架构的控制端，以及供节点下载的两种架构 Agent。
func Assets(arch string) []string {
	return []string{"pve-forward-linux-" + arch, "pve-forward-agent-linux-amd64", "pve-forward-agent-linux-arm64"}
}

// Progress 报告下载进度。
type Progress func(step string, percent int)

// Installer 下载并安装在线更新版本。
type Installer struct {
	DataDir string
	Source  Source
	Arch    string
	// Verify 检查下载的程序能在本机运行且版本号正确；为空时执行 "<程序> version"。
	Verify func(ctx context.Context, path, version string) error
}

// Install 下载指定版本，校验后放入 updates/<版本>/，返回控制端程序路径。
// 只有全部文件下载并校验通过后才会出现在最终目录，失败时不留下半成品。
func (in *Installer) Install(ctx context.Context, version string, progress Progress) (string, error) {
	if !Valid(version) {
		return "", fmt.Errorf("版本号 %q 无效", version)
	}
	arch := in.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	if progress == nil {
		progress = func(string, int) {}
	}
	progress("读取校验文件", 2)
	sums, err := in.Source.Checksums(ctx, version)
	if err != nil {
		return "", err
	}
	assets := Assets(arch)
	for _, a := range assets {
		if sums[a] == "" {
			return "", fmt.Errorf("版本 v%s 的发布文件中缺少 %s", version, a)
		}
	}
	dir := Dir(in.DataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".download-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for i, a := range assets {
		progress("下载 "+a, 5+i*85/len(assets))
		if err := in.download(ctx, in.Source.AssetURL(version, a), filepath.Join(tmp, a), sums[a]); err != nil {
			return "", fmt.Errorf("下载 %s 失败：%w", a, err)
		}
	}
	// 控制端程序固定命名为 pve-forward，Agent 放在同一目录供节点下载
	if err := os.Rename(filepath.Join(tmp, assets[0]), filepath.Join(tmp, "pve-forward")); err != nil {
		return "", err
	}
	progress("校验新程序", 92)
	verify := in.Verify
	if verify == nil {
		verify = runVersion
	}
	if err := verify(ctx, filepath.Join(tmp, "pve-forward"), version); err != nil {
		return "", err
	}
	final := filepath.Join(dir, version)
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	progress("下载完成", 95)
	return filepath.Join(final, "pve-forward"), nil
}

func (in *Installer) download(ctx context.Context, u, path, want string) error {
	resp, err := in.Source.get(ctx, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAsset+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxAsset {
		return errors.New("文件超过大小限制")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("SHA256 校验失败（期望 %s，实际 %s）", want, got)
	}
	return os.Chmod(path, 0o755)
}

// runVersion 运行新程序的 version 子命令，确认它能在本机执行且版本号一致。
func runVersion(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("新程序无法在本机运行（数据目录是否禁止执行？）：%v", err)
	}
	if got := strings.TrimSpace(out.String()); got != version {
		return fmt.Errorf("新程序版本为 %q，与 version.json 中的 %s 不一致", got, version)
	}
	return nil
}

// Activate 在下载完成后记录状态：下次启动（或立即替换进程时）运行新版本。
// running 为当前进程的程序路径，用于判断回退目标。
func Activate(dataDir, version, self, running string) error {
	prev := ""
	dir, err1 := filepath.Abs(Dir(dataDir))
	exe, err2 := filepath.Abs(running)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("无法解析程序路径：%v %v", err1, err2)
	}
	if rel, err := filepath.Rel(dir, exe); err == nil && !strings.HasPrefix(rel, "..") && filepath.Base(rel) == "pve-forward" {
		prev = self // 当前运行的是在线更新版本，失败时回退到它
	}
	// 进程将直接替换为新版本，计为第一次启动尝试：新版本若崩溃退出，
	// 被 Docker / systemd 重启后由 Launch 回退。
	return WriteState(dataDir, &State{Current: version, Previous: prev, Pending: true, Attempts: 1})
}
