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
	Conns    map[int64]int     `json:"conns"`
	Health   map[int64]string  `json:"health"` // 规则 ID → 异常原因（空表示正常）
	Listen   []int             `json:"listen"` // 宿主机进程正在监听的端口
	Loaded   bool              `json:"loaded"` // 规则表是否已加载
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
