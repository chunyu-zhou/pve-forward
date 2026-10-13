package nodeops

import "strings"

// Docker 安装源与镜像加速列表，参考 LinuxMirrors（https://linuxmirrors.cn/docker.sh）。
// 只允许从列表中选择：安装脚本直接拼接这些地址，且安装 Docker CE 时会校验 GPG 密钥指纹是否为 Docker 官方密钥。

type Mirror struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

const (
	// DockerSourceDistro 表示通过系统软件源安装 docker.io（仅 Debian / Ubuntu）
	DockerSourceDistro = "distro"
	// DockerSourceOfficial 为 Docker 官方源
	DockerSourceOfficial = "download.docker.com"
)

// DockerCESources 为 Docker CE 软件源（地址不含协议，脚本使用 https）。
var DockerCESources = []Mirror{
	{DockerSourceOfficial, "官方源（download.docker.com）"},
	{"mirrors.aliyun.com/docker-ce", "阿里云"},
	{"mirrors.tencent.com/docker-ce", "腾讯云"},
	{"mirrors.huaweicloud.com/docker-ce", "华为云"},
	{"mirrors.cmecloud.cn/docker-ce", "移动云"},
	{"mirrors.163.com/docker-ce", "网易"},
	{"mirrors.volces.com/docker", "火山引擎"},
	{"mirror.azure.cn/docker-ce", "微软 Azure 中国"},
	{"mirrors.tuna.tsinghua.edu.cn/docker-ce", "清华大学"},
	{"mirrors.pku.edu.cn/docker-ce", "北京大学"},
	{"mirrors.zju.edu.cn/docker-ce", "浙江大学"},
	{"mirrors.nju.edu.cn/docker-ce", "南京大学"},
	{"mirror.sjtu.edu.cn/docker-ce", "上海交通大学"},
	{"mirrors.hust.edu.cn/docker-ce", "华中科技大学"},
	{"mirrors.ustc.edu.cn/docker-ce", "中国科学技术大学"},
	{"mirror.iscas.ac.cn/docker-ce", "中国科学院软件研究所"},
	{DockerSourceDistro, "系统软件源（docker.io，仅 Debian / Ubuntu）"},
}

// RegistryMirrors 为 Docker Hub 镜像加速（拉取时作为镜像地址前缀）。LinuxMirrors 中的阿里云、腾讯云等
// 仓库只能作为 daemon.json 的 registry-mirrors 或仅限其云内网使用，不能直接作为前缀拉取，因此不列出；
// 本系统也不修改 Docker 的全局配置。
var RegistryMirrors = []Mirror{
	{"", "官方 Docker Hub"},
	{"docker.1ms.run", "毫秒镜像"},
	{"dockerproxy.net", "Docker Proxy"},
	{"docker.m.daocloud.io", "DaoCloud 道客"},
	{"docker.1panel.live", "1Panel 镜像"},
}

func inMirrors(list []Mirror, v string) bool {
	for _, m := range list {
		if m.Value == v {
			return true
		}
	}
	return false
}

func ValidDockerSource(v string) bool   { return inMirrors(DockerCESources, v) }
func ValidRegistryMirror(v string) bool { return inMirrors(RegistryMirrors, v) }

// DefaultWebImageDigest 为 DefaultWebImage 在 Docker Hub 上的摘要（多架构索引）。
// 通过第三方加速拉取默认镜像时按摘要拉取，内容与官方镜像不一致会被 Docker 拒绝。
const DefaultWebImageDigest = "sha256:5ad048d09dc948a4167a5c509fcc95c938b77bbd9ccdec96fc18d644b9404ef2"

// PullImage 返回实际拉取的镜像引用：选择加速时给 Docker Hub 镜像加上前缀（自带仓库地址的镜像不变），
// 默认镜像同时固定摘要。
func PullImage(image, registry string) string {
	if registry == "" || hasRegistryHost(image) {
		return image
	}
	if image == DefaultWebImage {
		return registry + "/" + strings.SplitN(image, ":", 2)[0] + "@" + DefaultWebImageDigest
	}
	if !strings.Contains(strings.SplitN(image, ":", 2)[0], "/") {
		image = "library/" + image // 官方镜像（如 openresty:tag）需要补全命名空间
	}
	return registry + "/" + image
}

// hasRegistryHost 判断镜像是否自带仓库地址（第一段含 . 或 : 或为 localhost）。
func hasRegistryHost(image string) bool {
	i := strings.Index(image, "/")
	if i < 0 {
		return false
	}
	first := image[:i]
	return strings.ContainsAny(first, ".:") || first == "localhost"
}
