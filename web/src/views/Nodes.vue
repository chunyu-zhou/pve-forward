<template>
  <div class="container" style="padding-top:16px">
    <a-card class="general-card" title="节点列表">
      <a-row style="margin-bottom: 16px" :gutter="8">
        <a-col :span="14"><a-space wrap>
          <a-button type="primary" @click="$router.push('/nodes/new')"><template #icon><icon-plus /></template>添加节点</a-button>
          <a-input-search v-model="kw" placeholder="名称 / 主机名 / 公网 IP" style="width:240px" allow-clear />
          <a-select v-model="st" placeholder="全部状态" allow-clear style="width:140px" :options="Object.entries(nodeBadge).map(([k, v]) => ({ label: v.text, value: k }))" /></a-space></a-col>
        <a-col :span="10" style="display:flex;align-items:center;justify-content:end">
          <span class="muted">控制端 v{{ version }}</span>
          <a-tooltip content="刷新"><div class="action-icon" @click="load"><icon-refresh size="18" /></div></a-tooltip>
        </a-col>
      </a-row>
      <a-table row-key="id" :data="rows" :loading="loading && !rows.length" :pagination="false" :bordered="false" :scroll="{ x: 1420 }">
        <template #columns>
          <a-table-column title="节点" :width="160"><template #cell="{ record }">
            <div style="font-weight:500">{{ record.name }}</div><div class="muted">{{ record.remark || record.hostname }}</div></template></a-table-column>
          <a-table-column title="连接方式" :width="190"><template #cell="{ record }">
            <template v-if="record.mode === 'ssh'"><a-tag size="small" color="purple">SSH</a-tag> <span class="mono">{{ record.sshHost }}:{{ record.sshPort }}</span>
              <div class="muted">{{ record.sshAuth === 'cred' ? '凭证 · ' + (record.credName || '-') : record.sshAuth === 'key' ? '密钥 · ' + record.sshUser : '密码 · ' + record.sshUser }}</div></template>
            <template v-else><a-tag size="small" color="green">Agent</a-tag> <span class="muted">WSS</span><div class="muted">v{{ record.agentVersion || '-' }}</div></template></template></a-table-column>
          <a-table-column title="状态" :width="150"><template #cell="{ record }">
            <a-tooltip v-if="record.error && record.status !== 'online'" :content="record.error"><a-badge :status="nodeBadge[record.status].badge" :text="nodeBadge[record.status].text" /></a-tooltip>
            <a-badge v-else :status="nodeBadge[record.status].badge" :text="nodeBadge[record.status].text" />
            <div class="muted">心跳 {{ fmtAgo(record.lastSeen) }}</div>
            <div v-if="record.applyError" class="muted" style="color:rgb(var(--red-6))">下发失败</div></template></a-table-column>
          <a-table-column title="公网地址" :width="200"><template #cell="{ record }">
            <template v-if="record.initialized"><div class="mono">{{ record.net.wanIface }} · {{ record.net.wanIp4 }}</div><div v-if="record.net.wanIp6" class="mono muted">{{ record.net.wanIp6 }}</div></template>
            <span v-else class="muted">-</span></template></a-table-column>
          <a-table-column title="内网" :width="180"><template #cell="{ record }">
            <template v-if="record.initialized"><div class="mono">{{ record.net.bridge }} · {{ record.net.cidr4 }}</div>
              <div class="muted">DHCP {{ { managed: 'dnsmasq', external: '外部', none: '无' }[record.net.dhcp] }}</div></template>
            <span v-else class="muted">-</span></template></a-table-column>
          <a-table-column title="虚拟机 / 规则" :width="120"><template #cell="{ record }">{{ record.vms }} 台 / {{ record.rules }} 条</template></a-table-column>
          <a-table-column title="资源" :width="150"><template #cell="{ record }">
            <template v-if="record.status === 'online' && record.metrics">
              <div class="muted">CPU {{ record.metrics.cpu }}%</div><a-progress :percent="record.metrics.cpu / 100" :show-text="false" size="small" :color="lvl(record.metrics.cpu)" />
              <div class="muted">内存 {{ pct(record.metrics.memUsed, record.metrics.memTotal) }}%</div><a-progress :percent="pct(record.metrics.memUsed, record.metrics.memTotal) / 100" :show-text="false" size="small" :color="lvl(pct(record.metrics.memUsed, record.metrics.memTotal))" /></template>
            <span v-else class="muted">-</span></template></a-table-column>
          <a-table-column title="版本" :width="130"><template #cell="{ record }">
            <div class="muted">PVE {{ record.pveVersion || '-' }}</div>
            <div v-if="record.mode === 'agent'" class="muted">Agent {{ record.agentVersion || '-' }}
              <a-tag v-if="record.agentVersion && record.agentVersion !== version" size="small" color="orange">可升级</a-tag></div></template></a-table-column>
          <a-table-column title="操作" :width="240"><template #cell="{ record }"><a-space :size="0">
            <a-button v-if="!record.initialized" type="text" size="small" @click="$router.push(`/nodes/${record.id}/setup`)">{{ record.status === 'waiting' ? '继续接入' : '初始化网络' }}</a-button>
            <a-button v-else type="text" size="small" @click="$router.push(`/nodes/${record.id}`)">配置</a-button>
            <a-button v-if="record.mode === 'agent'" type="text" size="small" @click="showCmd(record)">安装命令</a-button>
            <a-button v-else type="text" size="small" @click="testSSH(record)">测试连接</a-button>
            <a-dropdown @select="a => more(record, a)">
              <a-button type="text" size="small">更多 <icon-down /></a-button>
              <template #content>
                <a-doption value="sync" :disabled="record.status !== 'online'">同步虚拟机</a-doption>
                <a-doption value="push" :disabled="record.status !== 'online'">重新下发规则</a-doption>
                <a-doption value="delete" style="color:rgb(var(--red-6))">删除节点</a-doption>
              </template>
            </a-dropdown></a-space></template></a-table-column>
        </template>
        <template #empty><a-empty description="还没有节点"><a-button type="primary" @click="$router.push('/nodes/new')">添加节点</a-button></a-empty></template>
      </a-table>
    </a-card>

    <a-modal v-model:visible="cmd.visible" :title="'Agent 安装命令 · ' + cmd.name" :width="720" :footer="false">
      <div class="muted" style="margin-bottom:8px">在该节点的 PVE 宿主机以 root 执行（用于首次安装、重装或迁移到新主机）：</div>
      <div class="cmd">{{ cmd.command }}<a-button class="copy" size="mini" @click="copyText(cmd.command)"><template #icon><icon-copy /></template>复制</a-button></div>
      <div class="muted" style="margin-top:8px">令牌 30 分钟内有效、仅可使用一次；重装后节点上的规则会自动重新下发。</div>
    </a-modal>
    <a-modal v-model:visible="del.visible" title="删除节点" :width="460" @ok="doDelete" ok-text="删除" :ok-button-props="{ status: 'danger' }">
      <p>确定删除节点 <b>{{ del.node?.name }}</b>？该节点上的规则、端口分配和虚拟机记录将一并删除。</p>
      <a-checkbox v-model="del.cleanup" :disabled="del.node?.status !== 'online'">同时清理节点上的转发规则与 DHCP 配置（节点需在线）</a-checkbox>
    </a-modal>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { Message, Modal } from '@arco-design/web-vue'
import { api, nodeBadge, fmtAgo, lvl, pct, copyText } from '../api'

const nodes = ref([])
const version = ref('')
const loading = ref(false)
const kw = ref('')
const st = ref()
async function load() {
  loading.value = true
  try { const d = await api.get('/api/nodes'); nodes.value = d.nodes; version.value = d.version } finally { loading.value = false }
}
let timer
onMounted(() => { load(); timer = setInterval(load, 15000) })
onUnmounted(() => clearInterval(timer))

const rows = computed(() => nodes.value.filter(n => (!st.value || n.status === st.value) &&
  (!kw.value || `${n.name} ${n.hostname} ${n.net?.wanIp4} ${n.remark}`.includes(kw.value))))

const cmd = reactive({ visible: false, name: '', command: '' })
async function showCmd(n) {
  const r = await api.post(`/api/nodes/${n.id}/token`)
  Object.assign(cmd, { visible: true, name: n.name, command: r.command })
}
async function testSSH(n) {
  const r = await api.post(`/api/nodes/${n.id}/ssh-test`)
  if (r.ok) Message.success(`${n.name}：SSH 连接正常（${r.steps[0].detail}）`)
  else if (r.needTrust) Modal.warning({ title: '需要确认主机指纹', content: `首次连接或尚未信任：${r.fingerprint}。请在「初始化网络 / 继续接入」中确认。` })
  else Modal.error({ title: 'SSH 连接失败', content: r.steps.filter(s => !s.ok).map(s => `${s.name}：${s.detail}`).join('\n') })
  load()
}
const del = reactive({ visible: false, node: null, cleanup: true })
async function more(n, a) {
  if (a === 'delete') { Object.assign(del, { visible: true, node: n, cleanup: n.status === 'online' }); return }
  await api.post(`/api/nodes/${n.id}/sync${a === 'push' ? '?push=1' : ''}`)
  Message.success(a === 'push' ? '已重新下发' : '已同步')
  load()
}
async function doDelete() {
  const r = await api.del(`/api/nodes/${del.node.id}${del.cleanup ? '?cleanup=1' : ''}`)
  Message.success(del.cleanup && !r.cleaned ? '节点已删除（节点上的配置未能清理）' : '节点已删除')
  load()
}
</script>
