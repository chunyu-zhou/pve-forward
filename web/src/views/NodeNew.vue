<template>
  <div class="container" style="padding-top:16px">
    <a-card class="general-card">
      <template #title>{{ id ? '节点接入' : '添加节点' }}<span v-if="f.name" class="muted" style="margin-left:8px">{{ f.name }}</span></template>
      <template #extra><span class="muted">网络配置由 Agent 或 SSH 在节点上检测与执行，确认前不会改动节点</span></template>
      <div class="step-wrapper" style="padding:40px 0 48px">
        <a-steps :current="step" style="width: 940px; max-width: 100%" class="steps">
          <a-step description="名称与连接方式">节点信息</a-step>
          <a-step :description="f.mode === 'ssh' ? '测试 SSH 连接' : '在 PVE 上安装 Agent'">建立连接</a-step>
          <a-step description="公网网口与地址">公网网络</a-step>
          <a-step description="网桥与 DHCP">内网与 DHCP</a-step>
          <a-step description="检查并应用">确认</a-step>
        </a-steps>

        <!-- 1. 节点信息与连接方式 -->
        <a-form v-if="step === 1" :model="f" class="step-form" style="width:680px;max-width:100%" :label-col-props="{ span: 5 }" :wrapper-col-props="{ span: 19 }">
          <a-form-item label="节点名称" required :validate-status="err.name ? 'error' : undefined" :help="err.name || '用于界面展示，例如 pve-hk'">
            <a-input v-model="f.name" placeholder="pve-xx" :max-length="40" /></a-form-item>
          <a-form-item label="备注"><a-input v-model="f.remark" placeholder="地域、机房等，选填" /></a-form-item>
          <a-form-item label="可分配端口" required :validate-status="portHint.error ? 'error' : undefined"
            :help="portHint.error || `只有此范围内的端口能分配给用户，当前可分配 ${portHint.count} 个；支持多段，如 10000-20000,30000-40000`">
            <a-input v-model="f.portRange" placeholder="10000-60000" /></a-form-item>
          <a-form-item label="保留端口" extra="不会分配给用户，支持单个与范围，如 22,8006,3000-3100">
            <a-input v-model="f.reservedPorts" placeholder="22,8006" allow-clear /></a-form-item>
          <a-form-item label="连接方式">
            <a-space direction="vertical" fill style="width:100%">
            <a-alert v-if="agentWhy" type="warning" title="无法使用 Agent 方式">{{ agentWhy }}</a-alert>
            <a-radio-group v-model="f.mode" direction="vertical" style="width:100%">
              <a-radio value="agent" class="nic-radio" :disabled="!!agentWhy"><div class="nic-item"><b>Agent（WSS）</b><a-tag size="small" color="green">推荐</a-tag>
                <span class="muted">在 PVE 上安装 Agent，主动连接控制端；PVE 无需开放端口，适合内网 / NAT</span></div></a-radio>
              <a-radio value="ssh" class="nic-radio"><div class="nic-item"><b>SSH</b>
                <span class="muted">控制端通过 SSH 管理，PVE 无需安装程序；需能从控制端访问 PVE 的 SSH 端口</span></div></a-radio>
            </a-radio-group>
            </a-space>
          </a-form-item>
          <template v-if="f.mode === 'ssh'">
            <SSHFields :f="f" :err="err" :creds="creds" @new-cred="$router.push('/credentials')" />
          </template>
          <a-form-item><a-button type="primary" :loading="busy" @click="createNode">{{ f.mode === 'agent' ? '生成安装命令' : '测试连接' }}</a-button></a-form-item>
        </a-form>

        <!-- 2a. SSH 连接测试 -->
        <div v-else-if="step === 2 && f.mode === 'ssh'" class="step-form" style="width:760px;max-width:100%">
          <a-spin :loading="busy" style="width:100%">
            <a-steps direction="vertical" small :current="sshRes.steps?.length || 1">
              <a-step v-for="s in sshRes.steps || []" :key="s.name" :status="s.ok ? 'finish' : 'error'" :description="s.detail">{{ s.name }}</a-step>
            </a-steps>
          </a-spin>
          <a-alert v-if="sshRes.needTrust" type="warning" style="margin-top:12px">
            首次连接，请核对主机指纹：<span class="mono">{{ sshRes.fingerprint }}</span>（{{ sshRes.keyAlgo }}）。可在 PVE 上执行
            <span class="mono">ssh-keygen -lf /etc/ssh/ssh_host_{{ (sshRes.keyAlgo || 'ed25519').replace('ssh-', '').replace(/-.*/, '') }}_key.pub</span> 查看
            <template #action><a-button size="mini" type="primary" :loading="busy" @click="trust">信任并继续</a-button></template>
          </a-alert>
          <div v-if="detect" class="wait-box"><icon-check-circle-fill style="color:rgb(var(--green-6));font-size:24px" />
            <div style="flex:1"><div>连接成功</div><a-descriptions :column="3" size="small" style="margin-top:6px" :data="detectRows" /></div></div>
          <div style="text-align:center;margin-top:32px"><a-space>
            <a-button type="secondary" @click="step = 1">上一步</a-button>
            <a-button v-if="!sshRes.ok && !sshRes.needTrust" :loading="busy" @click="runSSHTest">重新测试</a-button>
            <a-button type="primary" :disabled="!detect" @click="step = 3">下一步：配置网络</a-button>
            <a-button type="text" @click="$router.push('/nodes')">稍后初始化</a-button></a-space></div>
        </div>

        <!-- 2b. 安装 Agent -->
        <div v-else-if="step === 2" class="step-form" style="width:780px;max-width:100%">
          <div class="muted" style="margin-bottom:8px">在 <b>{{ f.name }}</b> 的 PVE 宿主机上以 root 执行：</div>
          <div class="cmd">{{ install.command }}<a-button class="copy" size="mini" @click="copyText(install.command)"><template #icon><icon-copy /></template>复制</a-button></div>
          <a-space direction="vertical" :size="4" style="margin-top:12px" class="muted">
            <span>· Agent 主动连接控制端，PVE 无需开放入站端口</span>
            <span>· 接入令牌 30 分钟内有效（至 {{ fmtTime(install.expiresAt) }}）、仅可使用一次</span>
            <span>· 安装过程不修改网络配置，网络在下一步由你确认后再应用</span>
          </a-space>
          <div class="wait-box">
            <template v-if="!detect"><a-spin :size="20" /><div><div>等待 Agent 连接…</div><div class="muted">已等待 {{ waited }} 秒</div></div>
              <a-space style="margin-left:auto"><a-button size="small" @click="regenToken">重新生成令牌</a-button></a-space></template>
            <template v-else><icon-check-circle-fill style="color:rgb(var(--green-6));font-size:24px" />
              <div style="flex:1"><div>Agent 已连接</div><a-descriptions :column="3" size="small" style="margin-top:6px" :data="detectRows" /></div></template>
          </div>
          <div style="text-align:center;margin-top:32px"><a-space>
            <a-button type="primary" :disabled="!detect" @click="step = 3">下一步：配置网络</a-button>
            <a-button type="text" @click="$router.push('/nodes')">稍后初始化</a-button></a-space></div>
        </div>

        <!-- 3. 公网网络 -->
        <a-form v-else-if="step === 3" :model="net" class="step-form" style="width:780px;max-width:100%" :label-col-props="{ span: 5 }" :wrapper-col-props="{ span: 19 }">
          <a-form-item label="公网网口" extra="已按默认路由自动识别，一般无需修改">
            <a-radio-group v-model="net.wanIface" direction="vertical" style="width:100%" @change="onWanChange">
              <a-radio v-for="n in detect.nics" :key="n.name" :value="n.name" :disabled="!n.up" class="nic-radio">
                <div class="nic-item"><b class="mono">{{ n.name }}</b>
                  <a-tag v-if="n.default" size="small" color="green">默认路由</a-tag><a-tag v-if="!n.up" size="small">未连接</a-tag>
                  <span class="muted mono">{{ [...(n.ip4 || []), ...(n.ip6 || [])].join(' · ') || '无地址' }}</span>
                  <span v-if="n.speedMbps" class="muted" style="margin-left:auto">{{ n.speedMbps >= 1000 ? n.speedMbps / 1000 + ' Gbps' : n.speedMbps + ' Mbps' }}</span></div>
              </a-radio>
            </a-radio-group>
          </a-form-item>
          <a-form-item label="公网 IPv4" extra="在规则入口中展示；若为内网地址（云厂商 NAT），已改用出口探测结果"><a-input v-model="net.wanIp4" /></a-form-item>
          <a-form-item label="公网 IPv6" extra="留空则该节点不启用 IPv6 转发"><a-input v-model="net.wanIp6" allow-clear /></a-form-item>
          <a-form-item><a-space><a-button type="secondary" @click="step = 2">上一步</a-button><a-button type="primary" @click="step = 4">下一步</a-button></a-space></a-form-item>
        </a-form>

        <!-- 4. 内网与 DHCP -->
        <a-form v-else-if="step === 4" :model="net" class="step-form" style="width:780px;max-width:100%" :label-col-props="{ span: 5 }" :wrapper-col-props="{ span: 19 }">
          <a-form-item label="内网网桥">
            <a-radio-group v-model="bridgeChoice" direction="vertical" style="width:100%" @change="onBridgeChange">
              <a-radio v-for="b in detect.bridges.filter(b => b.name !== net.wanIface)" :key="b.name" :value="b.name" class="nic-radio">
                <div class="nic-item"><b class="mono">{{ b.name }}</b><span class="muted mono">{{ (b.cidrs || []).join(' · ') || '未配置地址' }}</span>
                  <span class="muted">{{ b.ports.length ? '桥接 ' + b.ports.join(',') : '无物理端口' }} · {{ b.vms }} 台虚拟机</span></div>
              </a-radio>
              <a-radio value="__new" class="nic-radio"><div class="nic-item"><b>新建网桥</b>
                <a-input v-if="bridgeChoice === '__new'" v-model="newBridge" size="small" style="width:120px" @click.stop /><span class="muted">无物理端口的内部网桥</span></div></a-radio>
            </a-radio-group>
          </a-form-item>
          <a-form-item label="内网网段" :extra="bridgeCidrs.length ? `默认取网桥 ${bridgeChoice} 现有网段` : '默认 172.16.0.0/24，可自定义'"><a-input v-model="net.cidr4" /></a-form-item>
          <a-form-item v-if="subnetDiffers" label="网段处理">
            <div style="width:100%">
              <a-radio-group v-model="net.addrMode"><a-radio value="append">追加地址（推荐）</a-radio><a-radio value="replace">替换原地址</a-radio></a-radio-group>
              <a-alert :type="net.addrMode === 'replace' ? 'warning' : 'info'" style="margin-top:8px">
                {{ bridgeChoice }} 当前为 {{ bridgeCidrs.join('、') }}。{{ net.addrMode === 'replace' ? '替换后使用旧网段的虚拟机将断网，需重新获取地址。' : `在 ${bridgeChoice} 上追加 ${gw(net.cidr4)}，原网段虚拟机不受影响。` }}</a-alert>
            </div>
          </a-form-item>
          <a-form-item label="网关"><a-input :model-value="gw(net.cidr4)" disabled /></a-form-item>
          <a-form-item label="DHCP 服务">
            <div style="width:100%">
              <a-alert :type="detect.dhcp ? 'info' : 'warning'" style="margin-bottom:8px">
                {{ detect.dhcp ? `检测到 ${detect.dhcp} 正在运行。可继续使用它（虚拟机 IP 需自行在其中配置），或改由本系统管理 dnsmasq（需先停用 ${detect.dhcp} 以免冲突）。` : '未检测到 DHCP 服务（dnsmasq / isc-dhcp-server / kea）' }}</a-alert>
              <a-switch v-model="net.dhcp" /> {{ detect.dhcp ? '由本系统管理 dnsmasq' : '安装并启用 dnsmasq（仅 DHCP，不提供 DNS）' }}
            </div>
          </a-form-item>
          <template v-if="net.dhcp">
            <a-form-item label="DHCP 动态范围"><a-input :model-value="range(net.cidr4, 100, 250)" disabled /></a-form-item>
            <a-form-item label="DNS"><a-input-tag v-model="net.dns" /></a-form-item>
          </template>
          <a-form-item label="虚拟机分配池" extra="分配虚拟机时从此范围取 IP，与 DHCP 动态范围错开"><a-input :model-value="range(net.cidr4, 10, 99)" disabled /></a-form-item>
          <a-form-item label="IPv6">
            <a-radio-group v-model="ipv6Mode" :disabled="!net.wanIp6"><a-radio value="ula">内网 ULA + NAT</a-radio><a-radio value="off">不启用</a-radio></a-radio-group>
            <template #extra>{{ !net.wanIp6 ? '未配置公网 IPv6，已禁用' : ipv6Mode === 'ula' ? '内网自动生成随机 ULA /64 网段，经公网 IPv6 转发' : '规则仅支持 IPv4' }}</template>
          </a-form-item>
          <div v-if="err.net" style="color:rgb(var(--red-6));margin-bottom:8px">{{ err.net }}</div>
          <a-form-item><a-space><a-button type="secondary" @click="step = 3">上一步</a-button><a-button type="primary" :loading="busy" @click="preview">下一步</a-button></a-space></a-form-item>
        </a-form>

        <!-- 5. 确认并应用 -->
        <div v-else class="step-form" style="width:860px;max-width:100%">
          <template v-if="!applying && !applied">
            <a-descriptions :column="{ xs: 1, md: 2 }" bordered title="配置摘要" :data="summary" />
            <div style="text-align:center;margin-top:32px"><a-space>
              <a-button type="secondary" @click="step = 4">上一步</a-button>
              <a-button type="primary" @click="apply"><template #icon><icon-check /></template>下发并初始化</a-button></a-space></div>
          </template>
          <template v-else>
            <a-result v-if="applied && !applyErr" status="success" :title="`${f.name} 已就绪`" subtitle="可以在用户管理中为用户分配该节点的端口和虚拟机">
              <template #extra><a-space><a-button type="primary" @click="$router.push('/nodes')">查看节点列表</a-button><a-button @click="$router.push('/users/new')">添加用户</a-button></a-space></template>
            </a-result>
            <a-result v-else-if="applyErr" status="error" title="初始化失败" :subtitle="applyErr">
              <template #extra><a-button type="primary" @click="applying = false; applied = false; step = 4">返回修改</a-button></template>
            </a-result>
            <a-steps direction="vertical" small style="margin:0 auto;width:560px;max-width:100%">
              <a-step v-for="s in progress" :key="s.name + s.ok" :status="s.ok ? 'finish' : 'error'" :description="s.error || '完成'">{{ s.name }}</a-step>
              <a-step v-if="applying" status="process" description="执行中…">请稍候</a-step>
            </a-steps>
          </template>
        </div>
      </div>
    </a-card>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, copyText, fmtTime, portPolicyHint, streamNDJSON } from '../api'
import SSHFields from '../components/SSHFields.vue'

const props = defineProps({ id: String })
const router = useRouter()
const step = ref(1)
const busy = ref(false)
const err = reactive({})
const creds = ref([])
const nodeId = ref(props.id ? Number(props.id) : 0)
const f = reactive({ name: '', remark: '', portRange: '', reservedPorts: '', mode: 'agent', sshHost: '', sshPort: 22, sshAuth: 'password', sshUser: 'root', sshPassword: '', sshKey: '', sshPassphrase: '', credId: undefined, saveCred: false, credName: '' })
const install = reactive({ command: '', expiresAt: 0 })
const sshRes = ref({})
const detect = ref(null)
const waited = ref(0)
let timer

const net = reactive({ wanIface: '', wanIp4: '', wanIp6: '', cidr4: '', addrMode: 'append', dhcp: true, dns: ['223.5.5.5', '1.1.1.1'] })
const portHint = computed(() => portPolicyHint(f.portRange, f.reservedPorts))
// 访问地址不是 HTTPS 时不能使用 Agent（WSS）方式
const agentWhy = ref('')
const bridgeChoice = ref('')
const newBridge = ref('vmbr9')
const ipv6Mode = ref('ula')
const planned = ref(null)
const applying = ref(false)
const applied = ref(false)
const applyErr = ref('')
const progress = ref([])

onMounted(async () => {
  creds.value = await api.get('/api/credentials')
  if (!nodeId.value) {
    const d = await api.get('/api/settings')
    Object.assign(f, { portRange: d.settings.defaultPortRange, reservedPorts: d.settings.defaultReservedPorts })
    agentWhy.value = d.agentUnavailable || ''
    if (agentWhy.value) f.mode = 'ssh'
    return
  }
  // 继续接入已存在的节点
  const d = await api.get(`/api/nodes/${nodeId.value}`)
  const n = d.node
  agentWhy.value = n.mode === 'agent' ? '' : d.agentUnavailable || ''
  Object.assign(f, { name: n.name, remark: n.remark, portRange: n.ports.range, reservedPorts: n.ports.reserved, mode: n.mode, sshHost: n.sshHost, sshPort: n.sshPort, sshAuth: n.sshAuth || 'password', sshUser: n.sshUser, credId: n.credId || undefined })
  step.value = 2
  if (n.mode === 'ssh') await runSSHTest()
  else if (['pending', 'online'].includes(n.status)) await runDetect()
  else await regenToken()
})
onUnmounted(() => clearInterval(timer))

const clear = () => Object.keys(err).forEach(k => delete err[k])
const connBody = () => ({ name: f.name.trim(), remark: f.remark, portRange: f.portRange, reservedPorts: f.reservedPorts, mode: f.mode, sshHost: f.sshHost, sshPort: f.sshPort, sshAuth: f.sshAuth, sshUser: f.sshUser,
  sshPassword: f.sshPassword, sshKey: f.sshKey, sshPassphrase: f.sshPassphrase, credId: f.credId, saveCred: f.saveCred, credName: f.credName })

async function createNode() {
  clear()
  if (!f.name.trim()) err.name = '请输入节点名称'
  if (portHint.value.error) return
  if (f.mode === 'ssh') {
    if (!f.sshHost.trim()) err.host = '请输入 SSH 地址'
    if (f.sshAuth === 'cred' && !f.credId) err.cred = '请选择凭证'
    if (f.sshAuth === 'password' && !f.sshPassword && !nodeId.value) err.secret = '请输入密码'
    if (f.sshAuth === 'key' && !f.sshKey.trim() && !nodeId.value) err.secret = '请粘贴私钥'
  }
  if (Object.keys(err).length) return
  busy.value = true
  try {
    if (nodeId.value) {
      await api.put(`/api/nodes/${nodeId.value}`, connBody())
      if (f.mode === 'agent') await regenToken()
    } else {
      const r = await api.post('/api/nodes', connBody())
      nodeId.value = r.node.id
      if (r.install) Object.assign(install, r.install)
      router.replace(`/nodes/${nodeId.value}/setup`)
    }
    step.value = 2
    if (f.mode === 'ssh') await runSSHTest()
    else startWait()
  } catch (e) { err.name = e.message } finally { busy.value = false }
}

async function runSSHTest() {
  busy.value = true
  detect.value = null
  try {
    sshRes.value = await api.post(`/api/nodes/${nodeId.value}/ssh-test`)
    if (sshRes.value.ok) await runDetect()
  } finally { busy.value = false }
}
async function trust() {
  busy.value = true
  try {
    await api.post(`/api/nodes/${nodeId.value}/trust`, { hostKey: sshRes.value.hostKey })
  } finally { busy.value = false }
  await runSSHTest()
}

async function regenToken() {
  Object.assign(install, await api.post(`/api/nodes/${nodeId.value}/token`))
  startWait()
}
function startWait() {
  clearInterval(timer)
  waited.value = 0
  timer = setInterval(async () => {
    waited.value += 2
    if (waited.value % 4) return
    try {
      const d = await api.get(`/api/nodes/${nodeId.value}`, { silent: true })
      if (['pending', 'online'].includes(d.node.status)) { clearInterval(timer); await runDetect() }
    } catch { /* ignore */ }
  }, 2000)
}

async function runDetect() {
  const d = await api.post(`/api/nodes/${nodeId.value}/detect`, {}, { silent: true }).catch(e => { sshRes.value = { ...sshRes.value, detectErr: e.message }; return null })
  if (!d) return
  detect.value = d
  const n = d.nics.find(x => x.default) || d.nics.find(x => x.up)
  net.wanIface = n?.name || ''
  onWanChange()
  const br = d.bridges.find(b => b.name !== net.wanIface && b.cidrs.length) || d.bridges.find(b => b.name !== net.wanIface)
  bridgeChoice.value = br ? br.name : '__new'
  onBridgeChange()
  net.dhcp = !d.dhcp
  const used = new Set(d.bridges.map(b => b.name))
  for (let i = 1; i < 100; i++) if (!used.has('vmbr' + i)) { newBridge.value = 'vmbr' + i; break }
}
function onWanChange() {
  const n = detect.value?.nics.find(x => x.name === net.wanIface)
  if (!n) return
  net.wanIp4 = n.default && detect.value.publicIp4 ? detect.value.publicIp4 : ((n.ip4 || [])[0] || '').split('/')[0]
  net.wanIp6 = n.default && detect.value.publicIp6 ? detect.value.publicIp6 : ((n.ip6 || [])[0] || '').split('/')[0]
  ipv6Mode.value = net.wanIp6 ? 'ula' : 'off'
}

const detectRows = computed(() => detect.value ? [
  { label: '主机名', value: detect.value.hostname }, { label: 'PVE', value: detect.value.pveVersion || '未检测到' },
  { label: f.mode === 'ssh' ? '权限' : 'Agent', value: f.mode === 'ssh' ? (detect.value.root ? 'root' : '非 root') : detect.value.agentVersion },
  { label: '网口', value: detect.value.nics.length + ' 个' }, { label: '网桥', value: detect.value.bridges.length + ' 个' }, { label: '虚拟机', value: detect.value.vmCount + ' 台' },
] : [])

// ---- 网段计算 ----
const ipToInt = s => s.split('.').reduce((a, b) => (a << 8) + Number(b), 0) >>> 0
const intToIp = n => [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join('.')
function netOf(cidr) {
  const [ip, bits] = (cidr || '').split('/')
  if (!ip || !bits) return ''
  const mask = bits === '0' ? 0 : (~0 << (32 - Number(bits))) >>> 0
  return intToIp(ipToInt(ip) & mask) + '/' + bits
}
const host = (cidr, n) => { const nw = netOf(cidr); if (!nw) return '-'; return intToIp(ipToInt(nw.split('/')[0]) + n) }
const gw = cidr => (netOf(cidr) ? host(cidr, 1) + '/' + cidr.split('/')[1] : '-')
const range = (cidr, a, b) => {
  const bits = Number((cidr || '').split('/')[1])
  const size = 2 ** (32 - bits)
  return netOf(cidr) ? `${host(cidr, a)} – ${host(cidr, Math.min(b, size - 2))}` : '-'
}
const bridgeCidrs = computed(() => detect.value?.bridges.find(b => b.name === bridgeChoice.value)?.cidrs || [])
const subnetDiffers = computed(() => bridgeChoice.value !== '__new' && bridgeCidrs.value.length > 0 && !bridgeCidrs.value.some(c => netOf(c) === netOf(net.cidr4)))
function onBridgeChange() { net.cidr4 = bridgeCidrs.value.length ? netOf(bridgeCidrs.value[0]) : '172.16.0.0/24' }

const initBody = () => ({
  wanIface: net.wanIface, wanIp4: net.wanIp4.trim(), wanIp6: net.wanIp6.trim(), bridge: bridgeChoice.value === '__new' ? newBridge.value : bridgeChoice.value,
  newBridge: bridgeChoice.value === '__new', cidr4: net.cidr4.trim(), addrMode: net.addrMode, dhcp: net.dhcp, installDhcp: net.dhcp && !detect.value.dhcp,
  dns: net.dns, ipv6: ipv6Mode.value === 'ula' && !!net.wanIp6,
})

async function preview() {
  delete err.net
  busy.value = true
  try {
    planned.value = await api.post(`/api/nodes/${nodeId.value}/init?preview=1`, initBody(), { silent: true })
    step.value = 5
  } catch (e) { err.net = e.message } finally { busy.value = false }
}
const summary = computed(() => {
  const p = planned.value
  if (!p) return []
  const modeText = { existing: '使用现有地址', append: '追加地址', replace: '替换原地址', new: '新建网桥' }[p.addrMode]
  return [
    { label: '节点', value: f.name + (f.remark ? `（${f.remark}）` : '') },
    { label: '主机', value: `${detect.value.hostname} · PVE ${detect.value.pveVersion}` },
    { label: '连接方式', value: f.mode === 'ssh' ? `SSH · ${f.sshHost}:${f.sshPort}` : 'Agent（WSS）' },
    { label: '公网网口', value: p.wanIface },
    { label: '公网地址', value: p.wanIp4 + (p.wanIp6 ? ' / ' + p.wanIp6 : '') },
    { label: '内网网桥', value: `${p.bridge}（${modeText}，网关 ${p.gw4}）` },
    { label: '内网网段', value: p.cidr4 },
    { label: 'DHCP', value: p.dhcp === 'managed' ? `dnsmasq · ${range(p.cidr4, 100, 250)}` : p.dhcp === 'external' ? `沿用 ${detect.value.dhcp}` : '不启用' },
    { label: '虚拟机分配池', value: range(p.cidr4, 10, 99) },
    { label: 'IPv6', value: p.cidr6 ? `${p.cidr6} + NAT` : '不启用' },
    { label: '可分配端口', value: `${f.portRange}（共 ${portHint.value.count} 个）` },
    { label: '保留端口', value: f.reservedPorts || '无' },
  ]
})

async function apply() {
  applying.value = true
  applyErr.value = ''
  progress.value = []
  try {
    await streamNDJSON(`/api/nodes/${nodeId.value}/init`, initBody(), m => {
      if (m.step) progress.value.push(m.step)
      if (m.error) applyErr.value = m.error
    })
  } catch (e) { applyErr.value = e.message }
  applying.value = false
  applied.value = true
}
</script>
