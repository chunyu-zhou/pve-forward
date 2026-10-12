# PVE Forward

面向 Proxmox VE 的端口转发控制端：Go + SQLite 后端、Vue 3 / Arco Design 前端，节点可通过 SSH 或主动连接控制端的 Agent 接入。规则在 **PVE 节点**上使用 nftables 执行，不是由控制端中转业务流量。

> **验收状态：真实 PVE 尚未实机验收，不应直接用于生产网络。** 当前单元测试、模拟 SSH/Agent/API 流程可验证部分逻辑，但不证明真实 PVE 的命令兼容性、nftables 内核行为、网络初始化/回滚、DHCP/IPv6 RA、宿主防火墙兼容性、重启持久化及转发性能正确。`tools/devnode` 和测试中的 fake executor 不会真实实施这些网络操作；模拟测试成功不等于实机成功。

## GitHub 自动构建 Docker（推荐）

仓库：[chunyu-zhou/pve-forward](https://github.com/chunyu-zhou/pve-forward)。控制端镜像：`ghcr.io/chunyu-zhou/pve-forward`，支持 `linux/amd64`、`linux/arm64`。**构建、测试和发布全部在 GitHub Actions 执行，部署机只拉取镜像，不需要 Go、Node 或源码构建。**

工作流位于 [`.github/workflows/docker.yml`](.github/workflows/docker.yml)：

- `main` / `master` 上的程序、前端、依赖、`version.json` 或 Docker 构建配置变更会自动触发；通过检查后发布镜像。默认分支发布 `latest`，并提供提交 SHA 标签。
- **仅修改 README、Markdown 文档、`docs/` 或 `design/` 不触发构建**。使用生产代码路径白名单，普通文档不会消耗镜像构建资源。
- Pull request 执行检查但不发布镜像，也不使用发布凭据。
- 当 `version.json` 中的版本号还没有对应的 GitHub Release 时，同一次运行还会发布版本镜像（如 `1.2.3`、`1.2`）和 GitHub Release `v1.2.3`（两种架构的程序、Agent、`SHA256SUMS`）。也可以在 GitHub Actions 页面手动执行工作流。
- 使用仓库自带的 `GITHUB_TOKEN` 登录 GHCR，不需要把 Docker Hub 密码或管理员密码交给 GitHub。仓库/组织需允许 Actions 写入 Packages。

首次推送后，到仓库 Actions 查看执行结果。**配置文件存在不等于镜像已发布；必须等 GitHub 构建成功后再部署。** 若希望任何服务器匿名拉取，在 GitHub Packages 中把该包设为 Public（仓库公开不一定自动使包公开）；保留私有时，部署机需用具备 `read:packages` 的凭据登录 GHCR。`latest` 镜像不会自动更新正在运行的容器。

### 配置端口、管理员账号和密码

只需将 [compose.yaml](compose.yaml) 和 [.env.example](.env.example) 放在部署目录。Compose 中没有 `build:`，不会进行本地构建。

```bash
cp .env.example .env
```

编辑 `.env`，例如：

```dotenv
IMAGE_TAG=latest
BIND_ADDRESS=0.0.0.0
PORT=8443
PUBLIC_URL=https://panel.example.com:8443
TLS_MODE=self
SITE_NAME='PVE Forward'
ADMIN_USER=admin
ADMIN_PASSWORD='填写独立的高强度密码'
```

- `PORT` 是**宿主机映射端口**，容器内固定监听 5000。示例为 `8443:5000`。
- `PUBLIC_URL` 是浏览器和 PVE Agent 实际可达的完整外部地址，需包含非标准端口；不要填写容器名或 `localhost`（除非仅做本机试验），不支持路径、查询参数或内嵌账号密码。
- `TLS_MODE=self` 默认直接提供自签名 HTTPS，首次访问须核验并信任证书。`proxy` 为明文 HTTP，只应用在可信反代后，不要直接公网暴露。
- 管理员用户名为 3–20 位字母、数字或下划线；密码 8–72 字节。密码用单引号包裹，避免 `$` 被 Compose 插值。`.env` 已被 Git 忽略，不要提交真实密码。
- **账号、密码、站点及公网地址仅在数据卷首次初始化时使用**，成功后直接进入登录页，不再需要网页 setup token。重启、拉取新镜像或修改初始化密码变量都不会覆盖已有管理员；请在个人中心修改密码。已有部署的公网地址在系统设置中修改，同时保持 Docker 的端口/TLS 配置一致。

```bash
chmod 600 .env
```

```bash
docker compose pull
```

```bash
docker compose up -d
```

```bash
docker compose logs --tail=100 controller
```

### 持久化、安全与升级

命名卷 `controller-data` 挂载整个 `/var/lib/pve-forward`，保存 SQLite/WAL、主密钥及证书。镜像以 UID/GID `10001:10001` 非 root 运行，不需要 `privileged`、宿主网络或 `NET_ADMIN`。若改用 bind mount，先确保目录由该 UID/GID 读写；旧数据库必须和原 `master.key` 一起迁移。不要单独挂载 `data.db`，不要用 `docker compose down -v` 升级（这会删除数据卷）。

支持直接给容器传入 `PVE_FORWARD_ADMIN_PASSWORD_FILE=/run/secrets/admin_password` 读取 Docker secret；使用此方式时从 Compose environment 删除 `PVE_FORWARD_ADMIN_PASSWORD`，同时配置 secrets 挂载。不要同时设置两种密码来源。Docker 环境变量可被有 Docker 管理权限的人查看，secret 方式更适合正式部署。

| 容器环境变量 | 作用 |
| --- | --- |
| `PVE_FORWARD_LISTEN` | 实际监听地址，Compose 设置为 `:5000`，每次启动生效 |
| `PVE_FORWARD_TLS` | `self` / `proxy` / `acme`，每次启动生效 |
| `PVE_FORWARD_PUBLIC_URL` | 首次初始化的外部完整 URL，与内部监听端口独立 |
| `PVE_FORWARD_ADMIN_USER` | 首次管理员用户名 |
| `PVE_FORWARD_ADMIN_PASSWORD` | 首次管理员密码，与 PASSWORD_FILE 二选一 |
| `PVE_FORWARD_ADMIN_PASSWORD_FILE` | 首次管理员密码文件路径 |
| `PVE_FORWARD_SITE` | 首次站点名称（可选） |

命令行显式 `-listen` / `-tls` 优先于环境变量；未配置初始化账号和密码时仍保留原网页初始化流程。升级前停服并备份整个数据卷；升级仅执行以下两步，不会在部署机构建：

```bash
docker compose pull
```

```bash
docker compose up -d
```

建议生产部署固定版本标签（如 `IMAGE_TAG=1.0.1`）或镜像 digest；固定标签时请在“系统设置 → 版本更新”中在线更新，或修改 `IMAGE_TAG` 后执行上面的命令。容器镜像包含两种架构的 Agent 供节点下载；控制端镜像更新不会自动升级节点 Agent。数据卷 `agent/` 下的旧二进制会优先于镜像中的版本，迁移时应检查是否有手工覆盖文件。

## 版本发布与在线更新

### 发布新版本

版本号只有一个来源：仓库根目录的 [version.json](version.json)。程序编译时嵌入它，控制台也读取 GitHub 上的这个文件判断是否有新版本。

```json
{
  "version": "1.0.1",
  "date": "2026-10-20",
  "notes": "本次更新内容，会显示在控制台的更新提示中"
}
```

发布步骤：修改代码后，把 `version` 改为更高的 `主.次.修订`（可带 `-rc.1` 等预发布后缀），提交并推送到 `main`。GitHub Actions 先测试并发布 Docker 镜像，再创建 GitHub Release `v<版本>`。**只改代码不改版本号**时只会更新 `latest` 镜像，控制台不会提示新版本；同一版本号不会重复发布。

### 控制台检测与更新

控制端启动 1 分钟后检测一次，之后**每小时**读取 GitHub 上的 `version.json`（可在“系统设置 → 版本更新”关闭或手动检测）。只有对应 Release 已发布（GitHub 构建完成）才会提示可更新；发现新版本时管理员顶部栏出现提示，并写入事件通知。检测只提示，不会自动安装。

两种更新方式任选：

1. **在线更新程序**（后台点击“立即更新”）：从 GitHub Release 下载本机架构的控制端和两种架构的 Agent，逐个核对 `SHA256SUMS`，运行新程序核对版本号，备份数据库到数据目录 `backups/`，然后以新程序原地重启控制端。新程序保存在数据目录 `updates/<版本>/`，不修改镜像或安装目录，因此 Docker（只读根文件系统、非 root）和 systemd 安装都可使用。新版本启动后约 20 秒确认正常；如果它崩溃退出，被 Docker / systemd 重启时自动回退到更新前的版本，并在控制台显示回退提示。
2. **更新 Docker 镜像**：在部署目录执行

   ```bash
   docker compose pull && docker compose up -d
   ```

   拉取的镜像版本不低于在线更新的版本时，控制端自动改用镜像内的程序并清理 `updates/`。

注意：

- 在线更新仅支持 Linux amd64 / arm64。数据目录所在文件系统不能挂载为 `noexec`。
- 无法直接访问 GitHub 时，可在设置中填写下载加速前缀（拼接在 GitHub 地址前），或给容器设置 `HTTPS_PROXY`。加速前缀只接受 https 地址；校验文件 `SHA256SUMS` 会先尝试直连 GitHub 获取，只有直连失败时才经由加速服务，此时加速服务可同时提供程序和校验和，**请只使用可信的加速服务**。
- 在线更新只更新控制端。节点上已安装的 Agent 不会自动升级，新接入的节点会下载新版本 Agent。
- 数据库仅在更新前做一次备份（保留最近 5 份）；新版本若改变数据库结构，回退到旧版本时需要从 `backups/` 手动恢复。

## Docker 的 HTTPS 反代部署

已有 Nginx/Caddy 时：设置 `TLS_MODE=proxy`，`PUBLIC_URL=https://panel.example.com`（外部 443），在宿主机反代场景设置 `BIND_ADDRESS=127.0.0.1`，反代到 `http://127.0.0.1:5000`。若反代也是容器，可加入同一 Docker 网络并反代到 `http://controller:5000`，无需公开后端端口。容器内部始终监听 `:5000`，不要改为仅监听容器 loopback。

反代须支持 WSS/Upgrade 并保留 Agent Authorization；后面的 Nginx 片段同样适用。示例 Compose 仅映射业务端口，直接 ACME 还需要额外开放容器的 TCP 80，并处理非 root 低端口绑定；推荐由反代自动管理公网证书。

## 可选：二进制 release / systemd 部署

下面保留不使用 Docker 的源码构建和 systemd 安装方法；**它们不是上面 Docker 部署的前置步骤，也不会在本次开发中本地执行。**

构建机需要 Bash、Go、Node.js/npm、tar、sha256sum（Linux 或 Windows Git Bash）。`go.mod` 当前声明 **Go 1.27.1**，构建脚本读取实际 `go` directive 并显示选中的工具链。请安装满足该版本的 Go；若使用 `GOTOOLCHAIN=auto`，工具链自动下载仍依赖网络。不要为绕过构建错误擅自降低 go.mod 版本。Vite 7 需要 Node 20.19+ 或 22.12+，建议 Node 22/24 LTS。

```bash
# 在项目根目录，Linux Bash 或 Windows Git Bash
bash tools/build-release.sh --version local-1
```

脚本先执行 `npm ci`（使用 `web/package-lock.json`）和 `npm run build`，再以 `CGO_ENABLED=0` 交叉编译 Linux amd64/arm64 的 controller、agent。前端 `web/dist` 嵌入控制端，目标机无需 Node/Go。输出目录已存在时拒绝覆盖；需使用新的 label。label 只是归档名称，**不会修改 Go 源码中的应用版本**（当前为 1.0.0）。依赖未缓存时需要访问 npm/Go 模块源。

产物位于 `release/`，不应提交到 Git：

```text
pve-forward-local-1.tar.gz
pve-forward-local-1.tar.gz.sha256
pve-forward-local-1/
  pve-forward-linux-amd64
  pve-forward-linux-arm64
  pve-forward-agent-linux-amd64
  pve-forward-agent-linux-arm64
  install.sh
  README.md
  BUILD-INFO.txt
  SHA256SUMS
```

校验和检测损坏，**不提供发布者身份认证**。请通过可信渠道取得源码、安装脚本、归档和校验和，核对构建来源；不要认为攻击者随包附上的 SHA256SUMS 能证明安全。

## 控制端安装（非交互、systemd）

目标为 Linux amd64/arm64、正在运行的 systemd；建议独立 Debian/Ubuntu 控制机。安装程序需要 root、Bash、coreutils、tar 和 util-linux 的 `flock`。它不安装系统软件、不打开防火墙、不操作 PVE 网络、不从网上下载程序。控制端的 SSH 实现是 Go 客户端，不依赖系统 ssh。

将整个 `.tar.gz` 和 `.sha256` 传到目标机。先以普通用户在**新的空目录**验证、解包自己的可信归档；不要在旧程序或数据目录上解包：

```bash
sha256sum -c pve-forward-local-1.tar.gz.sha256
mkdir release-unpack-local-1
tar -xzf pve-forward-local-1.tar.gz -C release-unpack-local-1
cd release-unpack-local-1/pve-forward-local-1
sha256sum -c SHA256SUMS
# 先检查脚本，再使用 sudo；不要 sudo curl | bash
sudo bash ./install.sh --release-dir "$PWD"
```

安装无需回答问题，但**网页初始化仍需管理员完成**，不是自动创建账号。目录布局：

| 路径 | 用途 |
| --- | --- |
| `/opt/pve-forward/releases/<校验和>/` | 完整只读使用的版本目录，两种架构 Agent 和本机 controller |
| `/opt/pve-forward/current` | 原子切换的当前版本链接 |
| `/usr/local/bin/pve-forward` | controller 命令链接 |
| `/etc/systemd/system/pve-forward.service` | systemd 单元；升级保留 |
| `/var/lib/pve-forward` | 固定数据路径，目录权限 0700 |
| `/var/backups/pve-forward/<时间-随机后缀>/` | 停服后的数据归档、旧 unit/drop-in、旧版本链接 |

安装单元以 root 运行但使用 `ProtectSystem=strict`、`ProtectHome=true`、`NoNewPrivileges=true`、私有临时目录和仅绑定低端口的 capability 限制；可写持久目录为数据目录。仍应把控制端视为拥有 PVE 高权限凭证的敏感系统，限制其管理入口。**此脚本只管理上述固定目录**：拒绝接管已有非本脚本安装、可疑符号链接或数据目录中覆盖 release 的 Agent 文件。旧部署请先人工备份、制定迁移方案，不要删掉检查文件强行覆盖。

### 首次访问和令牌

不提供任何覆盖参数时，实际程序默认监听 `:5000`、TLS 为 `self`：访问 `https://控制端IP:5000`，不是 HTTP。初始自签名证书会触发浏览器警告，请从可信服务器日志核对指纹，不要盲目信任。

```bash
sudo systemctl status pve-forward.service --no-pager
sudo cat /var/lib/pve-forward/setup-token
sudo journalctl -u pve-forward.service -n 80 --no-pager
```

初始化令牌还会写入首次启动日志；将日志视为敏感信息。网页初始化完成后 `setup-token` 文件被删除。只有尚未初始化时，可以停服重新生成（旧令牌失效）：

```bash
sudo systemctl stop pve-forward.service
sudo /usr/local/bin/pve-forward setup-token -data /var/lib/pve-forward
sudo systemctl start pve-forward.service
```

初始化设置访问地址（如 `https://panel.example.com`，**不含端口或路径**）、单独的端口、TLS 方式以及管理员密码。之后 URL/端口设置影响生成的 Agent 命令。不要把初始化令牌、Agent 接入令牌、主密钥或日志放入工单/公开仓库。

## TLS、反向代理和端口

| 连接/模式 | 端口/要求 |
| --- | --- |
| 控制端默认 | 入站 TCP 5000，HTTPS 自签名；浏览器和 Agent 共用 |
| ACME | 公网域名指向控制端、入站 TCP 80 用于 HTTP-01；HTTPS 使用设置的监听端口，常用 443 |
| 反向代理 | 常用公网 443 → 本机 HTTP `127.0.0.1:5000`；WSS 同走公网入口 |
| SSH 节点 | 控制端 → PVE 的 SSH 端口（默认通常 22） |
| Agent 节点 | PVE 主动出站 HTTPS/WSS 到控制端；不需要额外开放 Agent 入站端口 |
| 转发流量 | 客户端 → PVE 公网端口（管理员分配的 TCP/UDP 端口），不是控制端 5000 |

程序 `-listen`、`-tls` 覆盖数据库设置。默认安装不设置这两个覆盖参数，因此网页修改端口/TLS 可生效。首次安装反代模式可用：

```bash
sudo bash ./install.sh --release-dir "$PWD" --listen 127.0.0.1:5000 --tls proxy
```

**proxy 模式后端是明文 HTTP，必须限制在 loopback/可信专用链路，不得直接暴露公网。** 浏览器也应通过 HTTPS 反代入口初始化。网页里仍填写外部 `https://panel.example.com`、端口 `443`、TLS `proxy`；若填写后端 5000，生成的 Agent 命令会指向错误入口。已有安装升级不接受上述覆盖参数；保留原 unit，可用 `sudo systemctl edit pve-forward.service` 重设 ExecStart（保留同一 `-data` 和二进制路径）：

```ini
[Service]
ExecStart=
ExecStart=/opt/pve-forward/current/pve-forward run -data /var/lib/pve-forward -listen 127.0.0.1:5000 -tls proxy
```

然后 `sudo systemctl daemon-reload && sudo systemctl restart pve-forward.service`。网页 TLS 模式应与覆盖值一致；当前自签名 pin 的提供还依赖数据库中的 TLS 设置。不要更改安装器固定的数据路径、服务用户或关闭文件保护而不制定独立迁移方案。

Nginx 最小反代片段（证书、DNS、站点安全设置需自己准备；使用根路径，不支持任意 URL 子路径部署）：

```nginx
# http {} 内
map $http_upgrade $pf_connection {
    default upgrade;
    ''      close;
}
server {
    listen 443 ssl;
    server_name panel.example.com;
    ssl_certificate     /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:5000;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $pf_connection;
        proxy_read_timeout 300s;
    }
}
```

必须允许 `/agent/join`、`/agent/ws`、`/agent/install.sh`、`/agent/download/amd64`、`/agent/download/arm64`；不能让额外网页登录/认证网关拦截 Agent，也不能丢弃 `Authorization`。WSS 是 WebSocket 升级连接，不是普通短轮询。反代须覆盖转发头而非接受公网伪造值。ACME 也需监控签发/续期失败；安装器仅检查进程存活，不验证证书签发。

自签名模式使用 `sha256//...` **证书公钥 pin**，Agent 及下载 curl 都应保留页面生成的 pin 参数；单独 `curl -k` 不是安全替代。更换/删除 `tls`、修改 URL 导致证书重新生成、改换反代证书公钥时，原 pin 可能失效，须核实新证书并更新节点配置或重新接入。公共 CA 模式按系统信任链验证。请勿在公网使用明文 `http://` Agent 连接。

## 接入节点及分配使用

1. **先备份 PVE 并准备带外控制台。** 建立隔离测试节点，记录管理口、网桥、路由、已有 DHCP/RA 和防火墙策略。不要把公网管理网桥直接改成试验内网。
2. SSH 接入：提供可达地址、端口、密码或私钥。需要 root，或能执行 **`sudo -n bash -s`** 的免交互 sudo 用户；这是远程 root shell 能力，不是 PVE API 的只读账号，也不是给几个只读命令的有限 sudo。凭证只授予可信管理员，推荐专用密钥并限制来源 IP。首次连接/主机密钥变化须从带外渠道核对 SSH 指纹，再在界面信任，不能为消除告警盲目接受。
3. Agent 接入：在界面新增 Agent 节点，取得一次性接入令牌（有效期 30 分钟）和安装命令。先审阅控制端返回的 `/agent/install.sh` 再在 PVE 上以 root 执行；节点需 curl、Bash、systemd。它安装 `/usr/local/bin/pve-forward-agent`、`pve-forward-agent.service`，配置/长期密钥位于 `/etc/pve-forward-agent/agent.json`（0600）。这与本仓库根目录的 **控制端** `install.sh` 不同。重新接入会更换节点密钥，务必备份 Agent 配置；控制端升级不会自动升级所有已运行的 Agent。
4. Agent 二进制的服务端搜索顺序为 **`<数据目录>/agent/pve-forward-agent-linux-<arch>` → controller 实际程序目录 → `/usr/local/lib/pve-forward`**。本 release 安装放在实际版本目录。旧的数据目录覆盖文件会导致仍下载旧 Agent，所以安装器明确拒绝此情况，不悄悄删除它们。两种架构都应可下载；编译 arm64 不代表该平台 PVE 已受支持或已验收。
5. 执行节点检测，核对实际公网网口、桥及 VM 网卡，然后再初始化网络。初始化会安装 nftables/conntrack（按选择安装 dnsmasq），变更网桥、IP 转发、DHCP/RA、systemd 服务并下发规则，**可导致 SSH/PVE 管理及 VM 网络中断**。
6. 管理员创建普通用户，为其分配**指定节点**的公网端口（支持范围如 `20000-20009`），并设置规则数上限。默认保留端口为 `22`、`8006`；还应主动保留实际 SSH 端口、控制端端口及宿主机其它服务端口。一个节点的端口不能重复分配，TCP/UDP 并非独立端口配额。收回端口会删除使用该端口的规则。
7. 同步 VM/LXC，管理员将接入所选内网桥的 VM 分配给用户，或设为共享。用户只能给有权限的 VM 配置转发；没有该桥网卡/MAC 的 VM 不能分配。VM 分配不是创建虚拟机，也不会自动修改所有 guest 网络配置。当前常见 `/24` 场景分配池为网络基址偏移 10–99，动态 DHCP 为 100–250；池按网段大小截断。IP 选择只检查应用数据库，不等同于扫描网络冲突，外部固定地址必须自己排除。
8. 自管 DHCP 将 MAC/IP 写入 `/etc/pve-forward/dhcp-hosts`，guest 需正确启用 DHCP/续租；外部 DHCP 或无 DHCP 时，自己在外部 DHCP/guest 中配置匹配的 IP、网关和 DNS。确认实际 guest IP 与界面一致，不能仅凭“分配成功”认定可达。
9. 普通用户创建“PVE 公网端口 → VM 目标端口”的 TCP/UDP、IPv4/IPv6 规则，等待下发并检查错误。管理员代建也需指定普通用户。PVE/guest 防火墙、上游安全组和运营商限制仍须单独检查；本系统不是完整防火墙。从**外部主机**测试双向流量，不能只用控制端健康或同机 curl 代替 DNAT 验收。

### DHCP、网络变更与删除风险

不要在同一二层网络启用第二个 DHCP 或冲突的 IPv6 RA 服务。使用现有 DHCP 时需人工协调地址池；`dnsmasq` 的重启可能影响该宿主上其它用途。IPv6 转发/RA、NAT 与 PVE 防火墙链顺序也必须实测。

节点初始化会备份部分文件到 `/etc/pve-forward/backup-<时间>`，`backup-last` 指向最近一次；这不是完整系统快照。失败恢复也不保证撤销已安装软件、运行时路由、网桥/API 状态、所有 nftables 规则或恢复远程可达性。应用自身的“已恢复”不等同于网络已恢复。必须保留带外控制台，并在维护窗口准备人工恢复。

生成文件包括 `/etc/pve-forward/{rules.nft,boot.sh,dhcp-hosts}`、`/etc/dnsmasq.d/pve-forward.conf`、`/etc/sysctl.d/99-pve-forward.conf` 和 `pve-forward-rules.service`。删除节点/清理不是完整卸载或回到原网络：网桥地址、已安装软件等可能保留；停控制端也不会自动清除已下发规则。不要通过删除数据库来“卸载网络”。

## 升级、备份与恢复

### 升级

重新构建一个新 label，在目标机的新目录校验/解包，运行同一安装命令（不再传 `--listen/--tls`）。安装器先验证全部 release 文件并准备版本目录，再停服务、备份**整个**数据目录和原 unit/drop-in，随后原子切换 `current`、启动服务。不覆盖 `data.db`、`master.key`、证书或初始化状态，也不清理旧版本/备份。请保证足够磁盘空间；备份含秘密，限制读取并加密离机保存。

如果启动/中间步骤失败，脚本尝试回指原二进制并**保持服务停止**，不自动把数据库覆盖回去，更不自动用旧程序启动可能已升级的数据库。查看打印的备份目录和 journal 后人工恢复。`SIGKILL`、断电、磁盘故障无法由 shell trap 完整回滚，须检查 `current`、unit 和数据。安装器的存活检查仅是 `systemctl is-active`，不宣称 HTTP/TLS、Agent 或 PVE 已通过验收。

### 手动一致性备份

先停控制端并确认没有第二个进程/维护命令写同一数据库。SQLite 使用 WAL，**不要只复制正在使用的 `data.db`**。例如由 root 在维护窗口：

```bash
sudo -i
systemctl stop pve-forward.service
b=/var/backups/pve-forward/manual-$(date -u +%Y%m%dT%H%M%SZ)
install -d -m 700 "$b"
tar -cpf "$b/data.tar" -C /var/lib/pve-forward .
cp -a /etc/systemd/system/pve-forward.service "$b/"
# 若存在 drop-in，另行复制整个 pve-forward.service.d
readlink /opt/pve-forward/current > "$b/previous-release"
chmod 600 "$b/data.tar"
systemctl start pve-forward.service
exit
```

数据目录含 `data.db`、`data.db-wal/shm`（若存在）、**32 字节 `master.key`**、`tls` 或 `acme` 缓存及可能的令牌/Agent 文件。`master.key` 加密 SSH 密码、私钥等，丢失后不能靠重置登录密码恢复；不可单独生成新 key 搭配旧数据库。安装器发现已有数据库而缺 key 会拒绝安装。

此外分别备份 PVE 的 `/etc/network/interfaces` 及 include 文件、路由/nftables 状态、上述生成文件、dnsmasq/防火墙配置、Agent `agent.json` 和服务。控制端数据备份不是 PVE 或 VM 备份。

### 恢复流程

1. 停控制端，保存故障现场；不要直接在现有数据目录上覆盖解压，以免混入新 WAL/旧密钥。
2. 将当前数据目录**改名隔离**，新建 0700 的 `/var/lib/pve-forward`；从可信且完整的停服备份将整个 `data.tar` 恢复进去，保持文件权限/所有权，核对原 `master.key` 为 32 字节、0600。备份也要先在独立空目录检查，拒绝未知归档及路径穿越。
3. 根据该备份的 `previous-release` 选择匹配的旧程序目录（不要盲目执行文件内容），恢复同一时期的 unit 和 drop-in，重新建立 `current` 链接。版本与数据库配对恢复；没有通用的数据库降级保证。
4. `systemctl daemon-reload`，启动控制端并检查 HTTPS、登录、凭证解密和节点连接。旧备份之后新接入/换过密钥的 Agent 可能需要重新接入；恢复前已变更的 PVE 网络不会自动“跟着恢复”。避免同时运行新旧两个控制端向同一节点下发冲突状态。
5. 分别核对节点、VM、用户端口分配、规则及外部连通性，再结束维护窗口。

## 验证与排障

```bash
bash -n install.sh tools/build-release.sh
# 先构建前端，否则 Go embed 缺少 web/dist
npm --prefix web ci
npm --prefix web run build
go test ./...
# release 脚本完成两种架构交叉编译；Linux 上再执行安装/启动验收
```

`go test` 内部可能启动临时 HTTP/SSH 模拟端点，不是 dev server，也不是 PVE 实机测试。Windows 上 Linux 交叉编译成功只证明编译链可用，不证明 systemd 或真实 Linux 二进制运行正常。

生产前至少在隔离的真实 PVE 上验证：SSH 主机密钥拒绝/更新、Agent join/WSS 断线重连、两种下载路径、证书轮换、网络初始化失败与人工回滚、DHCP 冲突保护、VM 归属和配额、TCP/UDP/IPv6 外部连通、宿主重启持久化、升级失败恢复及备份重建。监控数据/计数器重置和性能也需单独验收。

常见检查：控制端 `journalctl -u pve-forward`，节点 `journalctl -u pve-forward-agent` / `pve-forward-rules`；确认入口协议和端口、反代 WebSocket 升级、SSH root/sudo 权限、pin 是否过期、Agent 文件查找位置、VM 实际租约、PVE/guest/上游防火墙。公开状态页链接本质上是分享凭据，应只发给合适对象。
