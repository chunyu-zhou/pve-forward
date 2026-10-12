# PVE Forward 设计说明（待确认）

多节点 PVE 端口转发与监控平台：**控制端**统一管理用户、端口、虚拟机和规则；每台 PVE 节点通过 **Agent（WSS）** 或 **SSH** 两种方式之一接入，由管理员在添加节点时选择。

## 1. 总体架构

```text
                 浏览器（管理员 / 用户）
                         │ HTTPS
              ┌──────────┴──────────┐
              │        控制端        │  Web + API + SQLite
              │  用户 / 端口 / 规则   │  期望状态生成、监控汇总
              └───┬─────────────┬───┘
       WSS + mTLS │（Agent 主动连）│ SSH（控制端主动连）
          ┌───────┴──┐      ┌───┴───────┐
          │ PVE + Agent│     │ PVE（无 Agent）│
          └────────────┘     └────────────┘
     各节点本地执行：nftables DNAT、dnsmasq、网桥配置、流量计数、健康检查
```

| 组件 | 职责 | 部署 |
| --- | --- | --- |
| 控制端 `pve-forward` | Web 界面、API、SQLite；用户 / 端口 / 虚拟机 / 规则管理；按节点生成期望状态并下发；汇总监控与流量 | 任意 Linux 主机（VPS 或某台 PVE），单个二进制 |
| Agent `pve-forward-agent` | 在 PVE 本机执行：应用期望状态、上报虚拟机 / 指标 / 流量 / 健康检查 | 仅 Agent 方式的节点需要安装 |
| SSH 驱动（控制端内置） | 控制端通过 SSH 在 PVE 上完成同样的事情 | 无需在 PVE 安装程序 |

控制端内部把“如何管理一个节点”抽象为统一接口 `NodeDriver`（应用状态、读取检测信息、采集指标、执行健康检查），Agent 与 SSH 各一个实现，上层业务逻辑完全相同。

## 2. 技术选型

| 项 | 选择 | 理由 |
| --- | --- | --- |
| 语言 | **Go**（控制端与 Agent） | 静态二进制，交叉编译，无需在 PVE 上编译 |
| 数据库 | SQLite（`modernc.org/sqlite`，纯 Go） | 控制端单文件 `/var/lib/pve-forward/data.db` |
| 转发 | **nftables DNAT**，独立表 `inet pve_forward` | PVE 8 默认 nftables；不干扰 pve-firewall；`nft -f` 原子替换；自带计数器 |
| DHCP | dnsmasq（仅 DHCP，`port=0`） | 轻量，按虚拟机 MAC 写静态绑定 |
| 前端 | Vue 3 + Arco Design Vue，`embed` 打包进控制端 | 部署只有一个文件 |
| 认证 | bcrypt + HttpOnly Cookie 会话 | 角色 admin / user |
| Agent 通信 | **WSS + mTLS**，Agent 主动连接 | 见第 4 节 |
| SSH | `golang.org/x/crypto/ssh` + SFTP | 见第 5 节 |
| 凭证加密 | AES-256-GCM，主密钥在 `/etc/pve-forward/master.key`（600） | 密码 / 私钥不以明文落库 |

## 3. 节点上的执行内容（两种方式相同）

```nft
table inet pve_forward {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    # rule:1  alice  IPv4 + IPv6 → web01
    iifname "enp3s0f1" ip  daddr 203.0.113.10    tcp dport 20000 counter dnat ip  to 172.16.0.10:22
    iifname "enp3s0f1" ip6 daddr 2001:db8:10::10 tcp dport 20000 counter dnat ip6 to [fd00:16::10]:22
  }
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip  saddr 172.16.0.0/24 oifname "enp3s0f1" masquerade
    ip6 saddr fd00:16::/64  oifname "enp3s0f1" masquerade
  }
}
```

- **期望状态**：控制端为每个节点生成一份完整状态（nft 规则、dnsmasq 静态绑定、网桥与 sysctl 配置）并带版本号；节点侧先 `nft -c -f` 校验，再 `nft -f` 原子替换，回报结果。版本号相同的重复下发无副作用。
- **持久化**：规则文件写入 `/etc/pve-forward/rules.nft`，并安装一个开机加载它的 systemd 服务；控制端或 Agent 不在线时，已生效的转发不受影响。
- **流量统计**：读取 `nft -j list table` 的计数器，按小时汇总到控制端；用户超出流量上限（所有节点合计）后状态变为“流量超限”，其全部规则停用。用户无到期时间。
- **健康检查**（每 60 秒）：目标不可达（连续 3 次探测失败）、端口被宿主机进程占用（`ss -lntu`）、规则加载失败。异常时记录原因，恢复后自动回到“正常”。节点离线时，其上规则在界面显示为异常（状态未知）。
- **虚拟机同步**：通过 `pvesh` / PVE API 列出虚拟机，只保留网卡接在内网网桥上的，记录 VMID、名称、类型、状态、MAC。
- **监控指标**：节点与虚拟机的 CPU、内存、磁盘、磁盘 IO、网络；历史曲线优先使用 PVE 自带的 RRD 数据（`/nodes/<node>/qemu/<vmid>/rrddata`），无需控制端长期存储。
- **磁盘繁忙度**：
  - 节点：每块物理磁盘按 `/proc/diskstats` 两次采样计算 %util、读写 IOPS、吞吐、平均延迟（等同 `iostat -x`），并采集 CPU 的 IO 等待（`/proc/stat` iowait）；磁盘型号和所属存储取自 `lsblk` 与 PVE 存储配置。
  - 虚拟机：VM 取 QEMU blockstats（`rd/wr_operations`、`rd/wr_total_time_ns`），CT 取 cgroup `io.stat`，计算 IOPS 与平均延迟。
  - 分级：%util ≥ 90% 或延迟 ≥ 40 ms 为“饱和”，≥ 70% 或 ≥ 20 ms 为“繁忙”，≥ 30% 为“正常”，否则“空闲”；虚拟机没有 %util，按 IOPS 与延迟估算。阈值可在系统设置中调整，饱和时产生告警事件。

## 4. 连接方式一：Agent（WSS）

- **方向**：Agent 主动连接 `wss://<控制端>:<端口>/agent`，与 Web 共用端口；PVE 不需要开放入站端口，适合内网 / NAT，可经过 Nginx、Caddy、Cloudflare 等反向代理。
- **接入**：
  1. 添加节点时生成一次性接入令牌（30 分钟有效、只能用一次），安装命令中同时带控制端证书指纹，Agent 据此校验证书，自签名也不怕中间人；
  2. Agent 本地生成密钥对，用令牌换取控制端签发的客户端证书；之后每次连接做双向证书认证（mTLS），令牌作废；
  3. 删除节点即吊销证书，Agent 立即断开。
- **权限边界**：控制端只能下发期望状态，没有执行任意命令的通道；控制端被入侵也拿不到 PVE 的 shell。
- **消息**（JSON，带类型与序号）：
  - 下发：完整期望状态 + 版本号 → Agent 应用后回报成功 / 失败及原因；
  - 上报：每 15 秒心跳（含负载）；每 60 秒批量上报虚拟机、指标、流量计数、健康检查；异常立即上报；
  - 断线：指数退避重连（1 秒 → 60 秒）；重连后先报当前版本，不一致则补发；离线期间指标本地暂存，恢复后补报。
- **安装命令**：

  ```bash
  curl -fsSL https://panel.example.com:5000/agent/install.sh | bash -s -- \
    --server wss://panel.example.com:5000/agent --token XXXX-XXXX-XXXX
  ```

## 5. 连接方式二：SSH

- **方向**：控制端主动连接 PVE 的 SSH 端口（默认 22），与每个节点保持一条长连接，按需开会话执行命令、通过 SFTP 上传文件。需要控制端能直接访问 PVE 的 SSH 端口。
- **认证方式**（三选一）：
  - **密码**：直接填写；
  - **密钥**：粘贴私钥（OpenSSH / PEM），可带口令；
  - **已保存凭证**：从“SSH 凭证”库中选择。
  - 选密码或密钥时，可勾选“保存为凭证”，存入凭证库供其他节点复用，节点改为引用该凭证。
- **用户权限**：需为 root，或具备免密 sudo 的用户（命令前自动加 `sudo -n`）。
- **主机指纹**：首次连接时显示 PVE 的主机密钥指纹，管理员确认后保存（TOFU）；之后指纹变化会拒绝连接并告警。
- **连接测试**（添加节点第 2 步，逐项显示）：TCP 连通 → 校验主机指纹 → 身份认证 → 检查权限 → 检测环境（`pveversion`、`nft`、`ip`）。
- **执行方式**：规则与配置通过 SFTP 上传到临时文件，校验后原子替换；流量、健康检查、虚拟机同步由控制端定时通过 SSH 执行采集（默认 60 秒）。
- **与 Agent 的差异**：统计和检查依赖控制端轮询；控制端宕机期间转发不受影响，但统计明细与健康检查暂停（累计流量不丢）；用户修改 SSH 端口或禁用该账号会导致节点失联。
- **安全提示**：控制端保存可登录 PVE 的凭证，界面上建议优先使用专用 sudo 账号或 Agent 方式。

### 5.1 SSH 凭证库

- 页面：“PVE 节点 → SSH 凭证”。可添加多个凭证，字段：名称、类型（密码 / 密钥）、用户名、密码或私钥（含口令）、备注。
- 密钥可**粘贴已有私钥**，也可**由控制端生成 ED25519 密钥对**，页面给出把公钥写入 `authorized_keys` 的命令。
- 密码与私钥 AES-GCM 加密保存，**保存后不可查看**，编辑时留空表示不修改。
- 列表显示类型、用户名、密钥指纹、使用该凭证的节点；被节点使用的凭证不能删除。
- 修改凭证后，所有引用它的节点在下次连接时使用新凭证。

## 6. 控制端安装与初始化

### 6.1 安装（无交互）

1. 下载控制端二进制，注册 systemd 服务，默认监听 `0.0.0.0:5000`（占用则顺延）。
2. 生成一次性初始化令牌并输出：

   ```text
   PVE Forward 控制端已安装
   请在浏览器打开 https://<本机 IP>:5000 完成初始化
   初始化令牌：7KQ2-M9XD-4TPA   （忘记可执行 pve-forward setup-token 重新生成）
   ```

3. 重复执行即升级；`install.sh uninstall` 卸载控制端（不影响各节点上已生效的规则）。

### 6.2 控制端初始化向导（首次访问，只填控制端信息）

| 步骤 | 内容 |
| --- | --- |
| 1 验证 | 初始化令牌；管理员账号与密码 |
| 2 控制端设置 | 站点名称；控制端访问地址（Agent 连接用，默认取当前浏览器地址）；面板端口；HTTPS 证书（自签名 / Let's Encrypt / 由反向代理处理） |
| 3 完成 | 显示访问地址与 Agent 连接地址，进入登录 |

网络相关配置不在这里，而是在每个节点接入后单独初始化（第 7 节）。

## 7. 添加节点（含网络初始化）

| 步骤 | Agent 方式 | SSH 方式 |
| --- | --- | --- |
| 1 节点信息 | 名称、备注；连接方式选择 Agent | 名称、备注；连接方式选择 SSH，填写地址与端口，选择认证方式（密码 / 密钥 / 已保存凭证） |
| 2 建立连接 | 显示安装命令，等待 Agent 连接；可重新生成令牌，或“稍后初始化” | 逐项测试连接，首次需确认主机指纹 |
| 3 公网网络 | 列出节点网口，默认选默认路由所在网口；公网 IPv4 / IPv6；保留端口 | 同左 |
| 4 内网与 DHCP | 网桥（已有或新建）；网段默认取网桥现有网段，新建默认 `172.16.0.0/24`，不同时选“追加 / 替换”；DHCP 检测，未安装可开关安装 dnsmasq；虚拟机分配池 `.10 – .99`、DHCP 动态范围 `.100 – .250`；IPv6 方案（ULA + NAT / 不启用） | 同左 |
| 5 确认 | 配置摘要 → 下发并逐项显示进度 | 同左，额外写入开机加载规则的 systemd 服务 |

- 第 5 步任一项失败会回滚已做的改动并显示错误。
- 完成前不会改动节点；节点状态：等待连接 → 待初始化 → 在线 / 离线。
- 节点配置页可修改网络参数、切换连接方式（Agent ↔ SSH），保存后重新下发。

## 8. 权限与校验（后端强制）

- **端口按节点独立**：同一节点内端口不能分给两个用户，不同节点可以用相同端口号。分配方式：单个 / 多个 / 范围。
- **所属用户**：普通用户只能在自己名下建规则（界面禁用该字段，后端忽略请求中的 owner）。
- **节点**：添加规则时只列出该用户分配了端口的节点；离线节点不可选。
- **监听端口**：只能下拉选择 = 该节点上分配给用户的端口 − 已被规则使用 − 宿主机占用 − 保留端口；编辑时保留当前端口；后端以 `(node_id, listen_port)` 唯一约束兜底。
- **目标虚拟机**：只能选该用户在此节点上的虚拟机或同节点的公共虚拟机。
- **IP 版本**：节点没有公网 IPv6，或所选虚拟机没有 IPv6 地址时，“IPv6”与“IPv4 + IPv6”禁用，只能选 IPv4。
- **虚拟机分配**：在用户表单中勾选（可跨节点），已归属他人的不可选；保存时为无 IP 的虚拟机从该节点分配池取地址并写入 DHCP 静态绑定；取消已被规则使用的虚拟机时对应规则停用。

## 9. 公开监控页（免登录）

- **开关**：“系统设置 → 公开监控页”，默认关闭。开启后，任何人访问 `https://<控制端>/status/<路径后缀>` 无需登录即可查看。
- **页面形态**：与“资源监控”同一套组件，但**不显示顶部导航和左侧菜单**，只有页面标题栏（站点 Logo、标题、说明、整体状态、更新时间）和监控内容。
- **可配置项**：
  - 路径后缀：可自定义或随机生成，避免被猜到；
  - 访问控制：完全公开，或设置访问密码（输入正确后在浏览器会话内有效）；
  - 页面标题、说明、刷新间隔；
  - 展示节点：可选择公开哪些节点；
  - 展示内容：节点概况、虚拟机列表、虚拟机历史曲线、内网 IP、归属用户、只显示公共虚拟机（默认不显示 IP 和归属用户）。
- **安全边界**：公开接口是独立的只读接口，后端按上述设置裁剪数据；永远不返回 MAC、转发规则、端口分配、凭证、节点公网地址以外的管理信息；接口限流，关闭开关后立即返回 404。

## 10. 数据表

```
users(id, username, password_hash, role, enabled, quota_bytes, max_rules, remark, created_at)
nodes(id, name UNIQUE, remark, mode[agent|ssh], status, hostname, pve_version, agent_version, cert_serial,
      ssh_host, ssh_port, ssh_auth[password|key|cred], ssh_user, ssh_secret_enc, cred_id NULL, host_key_fp,
      wan_iface, wan_ip, wan_ip6, lan_bridge, lan_cidr, lan_cidr6, dhcp_mode, reserved_ports,
      state_version, last_seen, created_at)
credentials(id, name, type[password|key], username, secret_enc, passphrase_enc, key_fingerprint, remark, updated_at)
join_tokens(token_hash, node_id, expires_at, used_at)
port_allocations(id, user_id, node_id, kind[single|multi|range], spec_text)
allocated_ports(node_id, port, user_id, allocation_id, PRIMARY KEY(node_id, port))
vms(id, node_id, vmid, type[qemu|lxc], name, status, mac, user_id NULL, shared, ipv4, ipv6, synced_at, UNIQUE(node_id, vmid))
rules(id, user_id, node_id, listen_port, proto, ip_version, target_vm_id, target_port, remark, enabled, error_msg, checked_at,
      UNIQUE(node_id, listen_port))
traffic(rule_id, ts_hour, bytes_in, bytes_out)
settings(key, value)   -- 含 public_status_*：开关、路径后缀、访问密码哈希、可见节点、展示内容
```

## 11. 页面

见 `index.html`（右下角切换：控制端初始化 / 登录 / 用户端 / 管理端）。整体对齐 Arco Design Pro。

| 菜单 | 页面 | 样式 |
| --- | --- | --- |
| 仪表盘 | 在线节点、用户、规则（正常 / 异常 / 停用）、今日流量；流量趋势（可按节点筛选）；流量排行；规则构成（按节点 / 协议 / IP 版本）；右侧快捷操作、最近访问、节点状态（用户为“我的配额”）、事件通知、帮助文档 | 工作台 |
| 资源监控 | 汇总（在线节点、运行中虚拟机、平均 CPU、内存、磁盘繁忙、资源告警）；节点卡片（CPU / 内存 / 磁盘容量 / IO 等待的进度条与趋势，物理磁盘列表显示繁忙度、IOPS、延迟，负载、网络、虚拟机数；离线节点卡片与在线节点等高）；虚拟机与容器表（按节点分组，CPU / 内存 / 磁盘 / 磁盘 IO（繁忙度 + IOPS + 延迟）/ 网络，进度条或 1 小时趋势，可排序，自动刷新）；点击查看详情抽屉（CPU、内存、网络、磁盘吞吐、IOPS、延迟的 1 小时 / 24 小时 / 7 天曲线）。普通用户只看自己的虚拟机 | 参考 Pulse，Arco 组件 |
| 转发管理 / 转发规则 | 筛选：关键词、节点、所属用户、协议、IP 版本、状态；复选框批量启用 / 停用 / 删除；按用户分组的树形表格；异常提示条 | 查询表格 |
| 用户管理 / 用户列表、添加用户、编辑用户 | 添加用户分 4 步：账号 → 端口（按节点）→ 虚拟机（跨节点勾选）→ 完成；编辑为分组表单 | 查询表格 / 分步表单 / 分组表单 |
| PVE 节点 / 节点列表 | 连接方式（Agent / SSH + 地址 + 凭证）、状态与心跳、公网、内网、虚拟机 / 规则数、资源、版本；操作：初始化网络、继续接入、配置、安装命令或测试连接、同步、升级 Agent、切换连接方式、删除 | 查询表格 |
| PVE 节点 / 添加节点 | 第 7 节的 5 步 | 分步表单 |
| PVE 节点 / 虚拟机 | 全部节点的虚拟机；分配给用户、设为公共、收回 | 查询表格 |
| PVE 节点 / SSH 凭证 | 第 5.1 节 | 查询表格 + 弹窗 |
| 系统设置 | 控制端（站点、地址、端口）、Agent（心跳、离线判定、自动升级、默认保留端口）、统计与检查间隔、公开监控页、安全（证书、会话、登录锁定） | 分组表单 |
| 公开监控页（`/status/<后缀>`） | 无顶部导航和左侧菜单；按设置展示节点概况与虚拟机；可设访问密码 | 独立页面 |
| 个人中心 | 基本信息、修改密码 | 卡片 |
