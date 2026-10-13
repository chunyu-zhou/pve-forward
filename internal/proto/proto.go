// Package proto 定义控制端与节点（Agent 或 SSH）之间交换的数据结构。
package proto

import "time"

// ---------------- 检测 ----------------

type NIC struct {
	Name      string   `json:"name"`
	IP4       []string `json:"ip4"` // CIDR 形式
	IP6       []string `json:"ip6"` // 仅 global 地址
	Up        bool     `json:"up"`
	Default   bool     `json:"default"` // 默认路由所在网口
	SpeedMbps int      `json:"speedMbps"`
}

type Bridge struct {
	Name  string   `json:"name"`
	CIDRs []string `json:"cidrs"`
	Ports []string `json:"ports"` // 桥接的物理端口
	VMs   int      `json:"vms"`   // 网卡接在此网桥的虚拟机数
}

type Detect struct {
	Hostname     string   `json:"hostname"`
	PVENode      string   `json:"pveNode"`
	PVEVersion   string   `json:"pveVersion"`
	NFTVersion   string   `json:"nftVersion"`
	AgentVersion string   `json:"agentVersion,omitempty"`
	Root         bool     `json:"root"`
	NICs         []NIC    `json:"nics"`
	Bridges      []Bridge `json:"bridges"`
	DHCP         string   `json:"dhcp"` // 正在运行的 DHCP 服务；空表示没有
	VMCount      int      `json:"vmCount"`
	DefaultIface string   `json:"defaultIface"`
	PublicIP4    string   `json:"publicIp4"`
	PublicIP6    string   `json:"publicIp6"`
}

// ---------------- 网络初始化 ----------------

// NetConfig 是节点的网络配置，初始化时确定，之后随期望状态一起下发。
type NetConfig struct {
	WANIface string   `json:"wanIface"`
	WANIP4   string   `json:"wanIp4"` // 对外展示的公网地址
	WANIP6   string   `json:"wanIp6"`
	Bridge   string   `json:"bridge"`
	CIDR4    string   `json:"cidr4"` // 网段，如 172.16.0.0/24
	CIDR6    string   `json:"cidr6"` // 为空表示不启用 IPv6
	GW4      string   `json:"gw4"`   // 网桥上的网关地址（CIDR 形式），如 172.16.0.1/24
	GW6      string   `json:"gw6"`
	AddrMode string   `json:"addrMode"` // existing | append | replace | new
	DHCP     string   `json:"dhcp"`     // managed（由我们管理 dnsmasq）| external（沿用已有）| none
	DNS      []string `json:"dns"`
	Reserved []int    `json:"reserved"`
}

type NetInit struct {
	Net         NetConfig `json:"net"`
	InstallDHCP bool      `json:"installDhcp"`
}

type Step struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ---------------- 期望状态 ----------------

type Rule struct {
	ID         int64  `json:"id"`
	Port       int    `json:"port"`
	Proto      string `json:"proto"` // tcp | udp | both
	IPVer      string `json:"ipVer"` // v4 | v6 | both
	Target4    string `json:"target4"`
	Target6    string `json:"target6"`
	TargetPort int    `json:"targetPort"`
	// Public 为管理员创建的公共规则：允许使用宿主机也在监听的端口（公网网口的流量由 DNAT 接管）
	Public bool `json:"public,omitempty"`
}

type DHCPHost struct {
	MAC  string `json:"mac"`
	IP4  string `json:"ip4"`
	IP6  string `json:"ip6"`
	Name string `json:"name"`
}

type DesiredState struct {
	Version int64      `json:"version"`
	Net     NetConfig  `json:"net"`
	Rules   []Rule     `json:"rules"`
	Hosts   []DHCPHost `json:"hosts"`
	// Web 转发：OpenResty 统计接口地址（为空表示未启用），采集时读取各站点的请求数与流量
	WebStatus string `json:"webStatus,omitempty"`
	// Web 转发站点的目标地址，采集时检测是否可达
	Sites []SiteTarget `json:"sites,omitempty"`
}

// SiteTarget 为 Web 转发站点的转发目标。
type SiteTarget struct {
	ID     int64  `json:"id"`
	Target string `json:"target"`
	Port   int    `json:"port"`
}

// ---------------- Web 转发（OpenResty） ----------------

// WebTarget 为 OpenResty 的运行位置：宿主机，或经由宿主机 SSH 连接的入口虚拟机。
type WebTarget struct {
	Mode      string `json:"mode"` // host | vm
	VMIP      string `json:"vmIp,omitempty"`
	SSHUser   string `json:"sshUser,omitempty"`
	SSHPort   int    `json:"sshPort,omitempty"`
	SSHAuth   string `json:"sshAuth,omitempty"` // node（使用宿主机 root 的密钥）| password | key
	SSHSecret string `json:"sshSecret,omitempty"`
}

// WebReq 为对 OpenResty 的一次操作。
type WebReq struct {
	Op     string    `json:"op"` // check | upload | install | apply | remove | pubkey
	Target WebTarget `json:"target"`
	Ports  []int     `json:"ports,omitempty"`  // check：需要空闲的 Web 端口
	Deploy string    `json:"deploy,omitempty"` // check / install：binary（二进制）| docker | manual（已自行安装）
	// install：二进制包的 SHA256（包已通过 upload 上传）；Docker 镜像（可带国内镜像仓库前缀）
	SHA256 string `json:"sha256,omitempty"`
	Image  string `json:"image,omitempty"`
	// upload：二进制包的一段内容，Offset 为 0 时重新开始
	Data   []byte `json:"data,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	// apply：版本目录名与其中的文件（相对路径 → 内容，含证书私钥）
	Dir   string            `json:"dir,omitempty"`
	Files map[string]string `json:"files,omitempty"`
}

type WebCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type WebResult struct {
	Checks    []WebCheck `json:"checks,omitempty"`
	Version   string     `json:"version,omitempty"`   // openresty -v
	PublicKey string     `json:"publicKey,omitempty"` // pubkey：宿主机 Web 转发专用密钥的公钥
	Arch      string     `json:"arch,omitempty"`      // check：uname -m
	Runtime   string     `json:"runtime,omitempty"`   // check：当前的运行方式 binary | docker | system，未安装为空
}

// WebCounter 为站点的累计请求数与流量（OpenResty 重启后清零）。
type WebCounter struct {
	Requests uint64 `json:"requests"`
	Up       uint64 `json:"up"`   // 客户端 → 站点（请求大小）
	Down     uint64 `json:"down"` // 站点 → 客户端（响应大小）
}

// WebReport 为采集时读取的 OpenResty 状态。
type WebReport struct {
	Up    bool                 `json:"up"`
	Error string               `json:"error,omitempty"`
	Stats map[int64]WebCounter `json:"stats,omitempty"`
}

type ApplyResult struct {
	Version int64 `json:"version"`
	// 刷新规则表之前的计数器快照，避免整表替换造成流量丢失
	Before map[int64]Counter `json:"before"`
}

// ---------------- 采集 ----------------

type Counter struct {
	Up   uint64 `json:"up"`   // 客户端 → 虚拟机
	Down uint64 `json:"down"` // 虚拟机 → 客户端
}

type NodeMetrics struct {
	CPU       float64    `json:"cpu"` // 0-100
	Cores     int        `json:"cores"`
	IOWait    float64    `json:"iowait"`
	MemUsed   uint64     `json:"memUsed"`
	MemTotal  uint64     `json:"memTotal"`
	DiskUsed  uint64     `json:"diskUsed"`
	DiskTotal uint64     `json:"diskTotal"`
	Load      [3]float64 `json:"load"`
	Uptime    int64      `json:"uptime"`
	NetIn     float64    `json:"netIn"` // 公网网口 字节/秒
	NetOut    float64    `json:"netOut"`
}

type DiskMetrics struct {
	Dev   string  `json:"dev"`
	Kind  string  `json:"kind"` // NVMe | SSD | HDD
	Model string  `json:"model"`
	Util  float64 `json:"util"` // %util
	RIOPS float64 `json:"rIops"`
	WIOPS float64 `json:"wIops"`
	RBps  float64 `json:"rBps"`
	WBps  float64 `json:"wBps"`
	Await float64 `json:"await"` // ms
}

type VMInfo struct {
	VMID       int     `json:"vmid"`
	Type       string  `json:"type"` // qemu | lxc
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	MAC        string  `json:"mac"`           // 接在内网网桥上的网卡；为空表示未接入
	IP4        string  `json:"ip4,omitempty"` // 当前实际使用的内网地址（静态配置、DHCP 租约或邻居表）；未识别为空
	IP6        string  `json:"ip6,omitempty"`
	CPU        float64 `json:"cpu"` // 0-100（相对分配的核数）
	Cores      int     `json:"cores"`
	MemUsed    uint64  `json:"memUsed"`
	MemTotal   uint64  `json:"memTotal"`
	DiskUsed   uint64  `json:"diskUsed"`
	DiskTotal  uint64  `json:"diskTotal"`
	Uptime     int64   `json:"uptime"`
	NetIn      float64 `json:"netIn"` // 字节/秒
	NetOut     float64 `json:"netOut"`
	DiskR      float64 `json:"diskR"`
	DiskW      float64 `json:"diskW"`
	IOPS       float64 `json:"iops"`
	IOPressure float64 `json:"ioPressure"` // cgroup io.pressure some avg10，0-100
}

type Report struct {
	Time     time.Time         `json:"time"`
	Node     NodeMetrics       `json:"node"`
	Disks    []DiskMetrics     `json:"disks"`
	VMs      []VMInfo          `json:"vms"`
	Counters map[int64]Counter `json:"counters"`
	Conns    map[int64]int     `json:"conns"`    // 规则的当前连接数（TCP 已建立 + UDP）
	ConnsTCP map[int64]int     `json:"connsTcp"` // 其中 TCP（已建立）
	ConnsUDP map[int64]int     `json:"connsUdp"` // 其中 UDP
	Health   map[int64]string  `json:"health"`   // 规则 ID → 异常原因（空表示正常）
	Listen   []int             `json:"listen"`   // 宿主机进程正在监听的端口
	Loaded   bool              `json:"loaded"`   // 规则表是否已加载
	// Web 转发：OpenResty 状态与站点目标是否可达（站点 ID → 异常原因）
	Web        *WebReport       `json:"web,omitempty"`
	SiteHealth map[int64]string `json:"siteHealth,omitempty"`
}

// RRDPoint 为 PVE rrddata 的一个点
type RRDPoint map[string]float64

type RRDReq struct {
	Type      string `json:"type"` // node | qemu | lxc
	VMID      int    `json:"vmid"`
	Timeframe string `json:"timeframe"` // hour | day | week
}

// ---------------- Agent RPC ----------------

type RPCMessage struct {
	ID     int64  `json:"id,omitempty"`
	Method string `json:"method,omitempty"`
	Params []byte `json:"params,omitempty"`
	Result []byte `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	// 网络初始化时的进度推送
	Progress *Step `json:"progress,omitempty"`
}

type Hello struct {
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
}

type JoinRequest struct {
	Token    string `json:"token"`
	Hostname string `json:"hostname"`
}

type JoinResponse struct {
	NodeID int64  `json:"nodeId"`
	Secret string `json:"secret"`
}
