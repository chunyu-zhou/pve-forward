<template>
  <div>
    <a-row>
      <a-col :flex="1">
        <a-form :model="q" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }" label-align="left">
          <a-row :gutter="16">
            <a-col :span="8"><a-form-item label="关键词"><a-input v-model="q.kw" placeholder="域名 / IP / 备注 / 虚拟机" allow-clear /></a-form-item></a-col>
            <a-col :span="8"><a-form-item label="节点"><a-select v-model="q.node" placeholder="全部" allow-clear :options="nodeOptions" /></a-form-item></a-col>
            <a-col :span="8"><a-form-item label="所属用户">
              <a-select v-model="q.owner" :disabled="!admin" placeholder="全部" allow-clear :options="ownerOptions" /></a-form-item></a-col>
            <a-col :span="8"><a-form-item label="入口"><a-select v-model="q.port" placeholder="全部" allow-clear :options="portOptions" /></a-form-item></a-col>
            <a-col :span="8"><a-form-item label="状态">
              <a-select v-model="q.status" placeholder="全部" allow-clear :options="[{ label: '正常', value: 'on' }, { label: '异常', value: 'err' }, { label: '已停用', value: 'off' }]" /></a-form-item></a-col>
          </a-row>
        </a-form>
      </a-col>
      <a-divider style="height: 84px" direction="vertical" />
      <a-col flex="86px" style="text-align: right">
        <a-space direction="vertical" :size="18">
          <a-button type="primary" @click="load"><template #icon><icon-search /></template>查询</a-button>
          <a-button @click="reset"><template #icon><icon-refresh /></template>重置</a-button>
        </a-space>
      </a-col>
    </a-row>
    <a-divider style="margin-top: 0" />
    <a-row style="margin-bottom: 16px">
      <a-col :span="16">
        <a-space wrap>
          <a-button type="primary" @click="$emit('add')"><template #icon><icon-plus /></template>添加规则</a-button>
          <a-button :disabled="!selectedIds.length" @click="batch('enable')">批量启用</a-button>
          <a-button :disabled="!selectedIds.length" @click="batch('disable')">批量停用</a-button>
          <a-popconfirm :content="`确定删除选中的 ${selectedIds.length} 个站点？自动添加的 DNS 解析也会删除`" type="warning" @ok="batch('delete')">
            <a-button :disabled="!selectedIds.length" status="danger">批量删除</a-button></a-popconfirm>
          <span v-if="selectedIds.length" class="muted">已选 {{ selectedIds.length }} 个 <a-link @click="selected = []">取消选择</a-link></span>
        </a-space>
      </a-col>
      <a-col :span="8" style="display:flex;align-items:center;justify-content:end">
        <a-space v-if="admin" style="margin-right:12px"><span class="muted">按用户分组</span><a-switch v-model="grouped" size="small" /></a-space>
        <a-tooltip content="刷新"><div class="action-icon" @click="load"><icon-refresh size="18" /></div></a-tooltip>
      </a-col>
    </a-row>

    <a-alert v-if="errSites.length" type="error" style="margin-bottom:16px">
      {{ errSites.length }} 个站点运行异常（{{ errSites.slice(0, 3).map(s => `${firstHost(s)} ${s.error}`).join('；') }}{{ errSites.length > 3 ? ' 等' : '' }}）
      <template #action><a-button size="mini" type="text" @click="q.status = 'err'">只看异常</a-button></template>
    </a-alert>

    <a-table row-key="key" :data="tableRows" :columns="columns" :pagination="false" :bordered="false" :loading="loading && !sites.length"
             :row-selection="{ type: 'checkbox', showCheckedAll: true, checkStrictly: false }" v-model:selected-keys="selected"
             v-model:expanded-keys="expanded" :indent-size="0" :span-method="spanMethod" :row-class="r => (r.group ? 'grp-row' : '')" :scroll="{ x: 1300 }">
      <template #status="{ record }">
        <template v-if="!record.group">
          <a-tooltip v-if="record.enabled && record.error" :content="record.error">
            <span style="color:rgb(var(--red-6));cursor:help"><span class="circle err"></span>异常 <icon-question-circle /></span></a-tooltip>
          <template v-else><span class="circle" :class="{ pass: record.enabled }"></span>{{ record.enabled ? '正常' : '已停用' }}</template>
        </template>
        <div v-else class="grp">
          <a-avatar :size="24" :style="{ backgroundColor: record.public ? '#86909c' : color(record.owner) }"><icon-public v-if="record.public" /><template v-else>{{ record.owner[0].toUpperCase() }}</template></a-avatar>
          <span class="nm">{{ record.owner }}</span>
          <a-tag size="small">{{ record.children.length }} 个</a-tag>
          <a-tag v-if="record.children.some(s => s.enabled && s.error)" size="small" color="red">异常 {{ record.children.filter(s => s.enabled && s.error).length }}</a-tag>
          <div class="info"><span v-if="record.public">由管理员维护，普通用户不可见</span></div>
        </div>
      </template>
      <template #node="{ record }"><a-tag size="small" :color="record.nodeStatus === 'online' ? 'arcoblue' : 'gray'">{{ record.nodeName }}</a-tag></template>
      <template #domains="{ record }"><div class="lines">
        <div v-for="h in record.hosts" :key="h.host"><div v-for="e in entries(record)" :key="entryKey(e)"><a :href="url(e, h)" target="_blank" rel="noopener" class="site-link">{{ url(e, h) }}</a></div>
          <a-tooltip v-if="h.dnsState && h.dnsState !== 'ok'" :content="dnsTip(h)"><icon-exclamation-circle :style="{ color: h.dnsState === 'failed' ? 'rgb(var(--orange-6))' : 'var(--color-text-3)', marginLeft: '4px' }" /></a-tooltip></div>
        <div v-if="record.remark" class="muted">{{ record.remark }}</div></div></template>
      <template #entry="{ record }"><a-space wrap :size="4"><a-tag v-for="e in entries(record)" :key="entryKey(e)" size="small" :color="e.https ? 'green' : 'arcoblue'">{{ e.https ? 'HTTPS' : 'HTTP' }} {{ e.port }}</a-tag></a-space></template>
      <template #target="{ record }"><div class="lines">
        <div class="mono">{{ record.scheme }}://{{ record.target4 || '-' }}:{{ record.targetPort }}</div>
        <a-space :size="4"><span class="muted"><span class="mono">{{ record.vmVmid }}</span> {{ record.vmName }}</span><a-tag v-if="record.vmShared" size="small" color="arcoblue">公共</a-tag></a-space>
      </div></template>
      <template #cert="{ record }">
        <span v-if="!entries(record).some(e => e.https)" class="muted">无（HTTP）</span>
        <div v-else class="lines"><div><icon-safe style="color:rgb(var(--green-6))" /> {{ record.certName || '-' }}</div>
          <div class="muted" :style="{ color: expColor(record.certDays) }">{{ record.certDays < 0 ? '已过期' : `${record.certDays} 天后到期` }}</div></div>
      </template>
      <template #traffic="{ record }"><div class="lines">
        <div>{{ record.req24.toLocaleString() }} 次</div>
        <a-tooltip content="近 24 小时：下行（站点 → 客户端） / 上行"><div class="updown"><span class="d"><icon-arrow-down /> {{ fmtBytes(record.down24) }}</span>&nbsp; <span class="u"><icon-arrow-up /> {{ fmtBytes(record.up24) }}</span></div></a-tooltip>
      </div></template>
      <template #switch="{ record }"><a-switch :model-value="record.enabled" size="small" @change="v => toggle(record, v)" /></template>
      <template #operations="{ record }">
        <a-space :size="0"><a-button type="text" size="small" @click="$emit('edit', record)">编辑</a-button>
          <a-popconfirm content="删除后该域名立即停止转发，并删除自动添加的 DNS 解析，确定删除？" type="warning" @ok="remove(record)">
            <a-button type="text" size="small" status="danger">删除</a-button></a-popconfirm></a-space>
      </template>
      <template #empty><a-empty :description="sites.length ? '没有符合条件的站点' : '还没有 Web 转发'" /></template>
    </a-table>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api, isAdmin, fmtBytes, expColor, dnsStateText } from '../api'

const dnsTip = h => dnsStateText(h.dnsState, h.dnsError)

const emit = defineEmits(['add', 'edit', 'count'])
const admin = computed(isAdmin)
const sites = ref([])
const users = ref([])
const loading = ref(false)
const q = reactive({})
const grouped = ref(true)
const selected = ref([])
const expanded = ref([])

const palette = ['#00b42a', '#ff7d00', '#722ed1', '#165dff', '#f53f3f', '#14c9c9', '#eb0aa4', '#7bc616']
const color = name => palette[[...name].reduce((s, c) => s + c.charCodeAt(0), 0) % palette.length]

async function load() {
  loading.value = true
  try {
    const [s, u] = await Promise.all([api.get('/api/sites'), admin.value ? api.get('/api/users') : Promise.resolve([])])
    sites.value = s
    users.value = u
    emit('count', s.length)
    if (!expanded.value.length) expanded.value = ['gpub', ...u.map(x => 'g' + x.id)]
  } finally { loading.value = false }
}
let timer
onMounted(() => { load(); timer = setInterval(load, 30000) })
onUnmounted(() => clearInterval(timer))
defineExpose({ load })

function reset() { for (const k of Object.keys(q)) delete q[k] }
const entries = s => s.entries || (s.port ? [{ port: s.port, https: s.https }] : [])
const entryKey = e => `${e.https ? 'https' : 'http'}:${e.port}`
const url = (e, h) => `${e.https ? 'https' : 'http'}://${h.host}${e.port === (e.https ? 443 : 80) ? '' : ':' + e.port}`
const firstHost = s => s.hosts[0]?.host || `站点 ${s.id}`
const state = s => (!s.enabled ? 'off' : s.error ? 'err' : 'on')
const filtered = computed(() => sites.value.filter(s =>
  (!q.node || s.nodeId === q.node) && (!q.owner || s.owner === q.owner) && (!q.port || entries(s).some(e => entryKey(e) === q.port)) && (!q.status || state(s) === q.status) &&
  (!q.kw || [s.hosts.map(h => h.host).join(' '), s.remark, s.target4, s.vmName, s.vmVmid].join(' ').toLowerCase().includes(q.kw.toLowerCase()))))
const errSites = computed(() => sites.value.filter(s => s.enabled && s.error))
const nodeOptions = computed(() => [...new Map(sites.value.map(s => [s.nodeId, { label: s.nodeName, value: s.nodeId }])).values()])
const ownerOptions = computed(() => (admin.value ? [{ label: '公共', value: '公共' }, ...users.value.filter(u => u.role !== 'admin').map(u => ({ label: u.username, value: u.username }))] : []))
const portOptions = computed(() => [...new Map(sites.value.flatMap(s => entries(s)).map(e => [entryKey(e), { label: `${e.https ? 'HTTPS' : 'HTTP'} ${e.port}`, value: entryKey(e), port: e.port }])).values()].sort((a, b) => a.port - b.port))

const tableRows = computed(() => {
  const list = filtered.value.map(s => Object.assign(s, { key: 's' + s.id }))
  if (!admin.value || !grouped.value) return list
  const pub = list.filter(s => s.public)
  const groups = users.value.filter(u => u.role !== 'admin')
    .map(u => ({ key: 'g' + u.id, group: true, owner: u.username, children: list.filter(s => !s.public && s.userId === u.id) }))
    .filter(g => g.children.length)
  return pub.length ? [{ key: 'gpub', group: true, public: true, owner: '公共', children: pub }, ...groups] : groups
})
const selectedIds = computed(() => selected.value.filter(k => String(k).startsWith('s')).map(k => Number(String(k).slice(1))))

const columns = computed(() => [
  { title: '状态', slotName: 'status', width: 100 },
  { title: '节点', slotName: 'node', width: 100 },
  ...(admin.value && !grouped.value ? [{ title: '所属用户', dataIndex: 'owner', width: 100 }] : []),
  { title: '域名', slotName: 'domains', width: 280 },
  { title: '入口', slotName: 'entry', width: 110 },
  { title: '转发到', slotName: 'target', width: 210 },
  { title: '证书', slotName: 'cert', width: 170 },
  { title: '请求 / 流量（24h）', slotName: 'traffic', width: 170 },
  { title: '启用', slotName: 'switch', width: 60 },
  { title: '操作', slotName: 'operations', width: 120 },
])
const spanMethod = ({ record, columnIndex }) => {
  if (!record.group) return
  return columnIndex === 0 ? { rowspan: 1, colspan: columns.value.length } : { rowspan: 0, colspan: 0 }
}

async function toggle(s, v) {
  await api.put(`/api/sites/${s.id}`, { enabled: v })
  s.enabled = v
  Message.success(v ? '已启用' : '已停用')
  load()
}
async function remove(s) {
  const r = await api.del(`/api/sites/${s.id}`)
  if (r.dnsFailed?.length) Message.warning({ content: `已删除，但删除解析失败：${r.dnsFailed.join('、')}，请手动删除`, duration: 8000 })
  else Message.success('已删除')
  load()
}
async function batch(action) {
  const r = await api.post('/api/sites/batch', { ids: selectedIds.value, action })
  Message.success(`已${{ enable: '启用', disable: '停用', delete: '删除' }[action]} ${r.count} 个站点`)
  selected.value = []
  load()
}
</script>

<style scoped>
.site-link { color: rgb(var(--primary-6)); text-decoration: none; }
.site-link:hover { text-decoration: underline; }
</style>
