package update

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// Repo 为发布仓库。
	Repo = "chunyu-zhou/pve-forward"
	// Image 为 Docker 镜像地址。
	Image = "ghcr.io/" + Repo
	// DefaultVersionURL 为默认的版本描述文件地址。
	DefaultVersionURL = "https://raw.githubusercontent.com/" + Repo + "/main/version.json"
	// DefaultReleaseBase 为 GitHub Release 下载地址前缀，完整地址为 <前缀>/v<版本>/<文件>。
	DefaultReleaseBase = "https://github.com/" + Repo + "/releases/download"
)

// ErrNotPublished 表示 version.json 已更新，但该版本的程序文件尚未发布（GitHub 仍在构建）。
var ErrNotPublished = errors.New("新版本的程序文件尚未发布，GitHub 可能仍在构建")

// Source 描述从哪里获取版本信息与程序文件。
type Source struct {
	VersionURL  string
	ReleaseBase string
	// Mirror 为可选的下载加速前缀（如 https://ghproxy.example.com/），拼接在 GitHub 地址之前。
	Mirror string
	Client *http.Client
}

// DefaultSource 返回指向官方仓库的来源。
func DefaultSource(mirror string) Source {
	return Source{VersionURL: DefaultVersionURL, ReleaseBase: DefaultReleaseBase, Mirror: mirror}
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	// 默认 Transport 会读取 HTTPS_PROXY / NO_PROXY 环境变量
	return &http.Client{Timeout: 10 * time.Minute}
}

func (s Source) url(u string) string { return s.Mirror + u }

// AssetURL 返回某个版本的发布文件地址。
func (s Source) AssetURL(version, name string) string {
	return s.url(strings.TrimRight(s.ReleaseBase, "/") + "/v" + version + "/" + name)
}

func (s Source) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pve-forward-updater")
	req.Header.Set("Cache-Control", "no-cache")
	return s.client().Do(req)
}

// Latest 读取最新的 version.json。
func (s Source) Latest(ctx context.Context) (*Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := s.get(ctx, s.url(s.VersionURL))
	if err != nil {
		return nil, fmt.Errorf("获取版本信息失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取版本信息失败：HTTP %d", resp.StatusCode)
	}
	var info Info
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return nil, fmt.Errorf("version.json 格式无效：%w", err)
	}
	if err := info.validate(); err != nil {
		return nil, err
	}
	return &info, nil
}

// Checksums 读取某个版本的 SHA256SUMS（文件名 → 小写十六进制摘要）。
// 文件不存在时返回 ErrNotPublished。配置了加速前缀时，先尝试直接从 GitHub 读取
// （校验文件很小），这样下载加速服务无法同时伪造程序与校验和；直连失败才经由加速前缀。
func (s Source) Checksums(ctx context.Context, version string) (map[string]string, error) {
	if s.Mirror != "" {
		direct := s
		direct.Mirror = ""
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		sums, err := direct.checksums(dctx, version)
		cancel()
		if err == nil || errors.Is(err, ErrNotPublished) {
			return sums, err
		}
	}
	return s.checksums(ctx, version)
}

func (s Source) checksums(ctx context.Context, version string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := s.get(ctx, s.AssetURL(version, "SHA256SUMS"))
	if err != nil {
		return nil, fmt.Errorf("获取校验文件失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotPublished
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取校验文件失败：HTTP %d", resp.StatusCode)
	}
	return parseChecksums(io.LimitReader(resp.Body, 64<<10))
}

func parseChecksums(r io.Reader) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		sum, name := strings.ToLower(f[0]), strings.TrimPrefix(f[1], "*")
		if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 || strings.ContainsAny(name, `/\`) {
			continue
		}
		sums[name] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, errors.New("校验文件 SHA256SUMS 为空或格式无效")
	}
	return sums, nil
}
