// Package webbundle 获取 Web 转发「二进制安装」使用的 OpenResty 包（由 tools/openresty 构建并发布到 GitHub Release）。
// 控制端下载后按 SHA256SUMS 校验并缓存，再上传到宿主机或入口虚拟机，目标机器不需要访问外网。
package webbundle

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Version 为当前使用的 OpenResty 包（与 tools/openresty/versions.env 一致：版本-修订）。
	Version = "1.31.1.1-1"
	// Prefix 为目标机器上的安装目录（与 tools/openresty/build.sh 一致）。
	Prefix = "/usr/local/pve-forward-openresty"
	// DefaultMirror 是可显式选择的第三方 GitHub 加速前缀，绝不默认启用。
	DefaultMirror = "https://ghfast.top/"
	repo          = "chunyu-zhou/pve-forward"
)

// Base 为 Release 下载地址（测试可替换）。
var Base = "https://github.com/" + repo + "/releases/download/openresty-" + Version

// Name 返回某个架构的包文件名。
func Name(arch string) string { return "openresty-" + Version + "-linux-" + arch + ".tar.gz" }

// Arch 把 uname -m 转换为包的架构名。
func Arch(machine string) (string, error) {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("二进制安装只支持 x86_64 与 aarch64（当前为 %s）", strings.TrimSpace(machine))
}

func get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s 返回 HTTP %d", u, resp.StatusCode)
	}
	return resp, nil
}

// checksum 读取 SHA256SUMS 中文件的摘要：先直接访问 GitHub，失败时经加速前缀。
func checksum(ctx context.Context, mirror, name string) (string, error) {
	resp, err := get(ctx, Base+"/SHA256SUMS")
	if err != nil && mirror != "" {
		resp, err = get(ctx, mirror+Base+"/SHA256SUMS")
	}
	if err != nil {
		return "", fmt.Errorf("无法获取 OpenResty 包的校验文件: %w", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			sum, err := hex.DecodeString(f[0])
			if err != nil || len(sum) != sha256.Size {
				return "", errors.New("SHA256SUMS 中的摘要无效")
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS 中没有 %s", name)
}

// Fetch 返回本地缓存的包路径（不存在或校验不符时下载）。mirror 为 GitHub 下载加速前缀，可为空。
func Fetch(ctx context.Context, dir, arch, mirror string) (string, error) {
	name := Name(arch)
	sum, err := checksum(ctx, mirror, name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if fileSum(path) == sum {
		return path, nil
	}
	resp, err := get(ctx, mirror+Base+"/"+name)
	if err != nil {
		return "", fmt.Errorf("下载 OpenResty 失败: %w", err)
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(dir, name+"-*.part")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	h := sha256.New()
	var size int64
	size, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, (200<<20)+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if size > 200<<20 {
		err = errors.New("OpenResty 包超过 200 MiB")
	}
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("下载 OpenResty 失败: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		os.Remove(tmp)
		return "", errors.New("OpenResty 包校验失败（SHA256 不一致），请检查下载加速地址")
	}
	return path, os.Rename(tmp, path)
}

func fileSum(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
