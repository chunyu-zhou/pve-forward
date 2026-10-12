// Package update 实现控制端的版本检测与在线更新。
//
// 版本来源是 GitHub 仓库中的 version.json；对应版本的程序文件发布在
// GitHub Release（v<版本号>）中，并附带 SHA256SUMS。在线更新把新程序
// 下载到数据目录的 updates/<版本号>/，再以新程序替换当前进程。程序每次
// 启动时（Launch）检查数据目录中是否有更新的版本并切换过去，因此 Docker
// 只读镜像、systemd 安装都能在不改动镜像/安装目录的情况下完成更新。
package update

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var versionRe = regexp.MustCompile(`^(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})(?:-([0-9A-Za-z.-]{1,32}))?$`)

// Valid 判断版本号是否为 主.次.修订[-预发布] 格式。
func Valid(v string) bool { return versionRe.MatchString(v) }

// Compare 比较两个版本号：a<b 返回 -1，相等返回 0，a>b 返回 1。
// 预发布版本低于同号正式版（1.2.0-rc.1 < 1.2.0）；无法解析的版本视为最低。
func Compare(a, b string) int {
	ma, mb := versionRe.FindStringSubmatch(a), versionRe.FindStringSubmatch(b)
	switch {
	case ma == nil && mb == nil:
		return 0
	case ma == nil:
		return -1
	case mb == nil:
		return 1
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return sign(x - y)
		}
	}
	pa, pb := ma[4], mb[4]
	switch {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	}
	// 预发布标识逐段比较：数字按数值，其余按字典序，数字段低于字母段
	sa, sb := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(sa) && i < len(sb); i++ {
		na, ea := strconv.Atoi(sa[i])
		nb, eb := strconv.Atoi(sb[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				return sign(na - nb)
			}
		case ea == nil:
			return -1
		case eb == nil:
			return 1
		default:
			if c := strings.Compare(sa[i], sb[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(sa) - len(sb))
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}

// Info 为 version.json 的内容。
type Info struct {
	Version string `json:"version"`
	Date    string `json:"date,omitempty"`
	Notes   string `json:"notes,omitempty"`
}

func (i *Info) validate() error {
	i.Version = strings.TrimPrefix(strings.TrimSpace(i.Version), "v")
	if !Valid(i.Version) {
		return fmt.Errorf("version.json 中的版本号 %q 格式无效", i.Version)
	}
	if len(i.Notes) > 8000 {
		i.Notes = i.Notes[:8000]
	}
	return nil
}
