<template>
  <div class="container" style="padding-top:16px">
    <a-card v-if="!admin && me" class="general-card" title="我的配额" style="margin-bottom:16px">
      <a-descriptions :column="{ xs: 1, md: 2, lg: 3 }" :data="quotaRows" />
    </a-card>

    <a-card class="general-card" title="转发规则">
      <a-row>
        <a-col :flex="1">
          <a-form :model="q" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }" label-align="left">
            <a-row :gutter="16">
              <a-col :span="8"><a-form-item label="关键词"><a-input v-model="q.kw" placeholder="端口 / IP / 备注 / 虚拟机" allow-clear /></a-form-item></a-col>
              <a-col :span="8"><a-form-item label="节点">
                <a-select v-model="q.node" placeholder="全部" allow-clear :options="nodeOptions" /></a-form-item></a-col>
              <a-col :span="8"><a-form-item label="所属用户">
                <a-select v-model="q.owner" :disabled="!admin" placeholder="全部" allow-clear :options="ownerOptions" /></a-form-item></a-col>
              <a-col :span="8"><a-form-item label="协议">
                <a-select v-model="q.proto" placeholder="全部" allow-clear :options="[{ label: 'TCP', value: 'tcp' }, { label: 'UDP', value: 'udp' }, { label: 'TCP + UDP', value: 'both' }]" /></a-form-item></a-col>
              <a-col :span="8"><a-form-item label="IP 版本">
                <a-select v-model="q.ipVer" placeholder="全部" allow-clear :options="[{ label: 'IPv4', value: 'v4' }, { label: 'IPv6', value: 'v6' }, { label: 'IPv4 + IPv6', value: 'both' }]" /></a-form-item></a-col>
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
            <a-button type="primary" @click="openRule()"><template #icon><icon-plus /></template>添加规则</a-button>
            <a-button :disabled="!selectedRules.length" @click="batch('enable')">批量启用</a-button>
            <a-button :disabled="!selectedRules.length" @click="batch('disable')">批量停用</a-button>
            <a-popconfirm :content="`确定删除选中的 ${selectedRules.length} 条规则？`" type="warning" @ok="batch('delete')">
              <a-button :disabled="!selectedRules.length" status="danger">批量删除</a-button></a-popconfirm>
            <span v-if="selectedRules.length" class="muted">已选 {{ selectedRules.length }} 条 <a-link @click="selected = []">取消选择</a-link></span>
          </a-space>
        </a-col>
        <a-col :span="8" style="display:flex;align-items:center;justify-content:end">
          <a-space v-if="admin" style="margin-right:12px"><span class="muted">按用户分组</span><a-switch v-model="grouped" size="small" /></a-space>
          <a-tooltip content="刷新"><div class="action-icon" @click="load"><icon-refresh size="18" /></div></a-tooltip>
        </a-col>
      </a-row>

      <a-alert v-if="errRules.length" type="error" style="margin-bottom:16px">
        {{ errRules.length }} 条规则运行异常（{{ errRules.slice(0, 3).map(r => `${r.nodeName}:${r.port} ${r.error}`).join('；') }}{{ errRules.length > 3 ? ' 等' : '' }}）
        <template #action><a-button size="mini" type="text" @click="q.status = 'err'">只看异常</a-button></template>
      </a-alert>

      <a-table row-key="key" :data="tableRows" :columns="columns" :pagination="false" :bordered="false" :loading="loading"
               :row-selection="{ type: 'checkbox', showCheckedAll: true, checkStrictly: false }" v-model:selected-keys="selected"
               v-model:expanded-keys="expanded" :indent-size="0" :span-method="spanMethod" :row-class="r => (r.group ? 'grp-row' : '')"
               :scroll="{ x: 1300 }">
        <template #status="{ record }">
          <template v-if="!record.group">
            <a-tooltip v-if="record.enabled && record.error" :content="record.error">
              <span style="color:rgb(var(--red-6));cursor:help"><span class="circle err"></span>异常 <icon-question-circle /></span></a-tooltip>
            <template v-else><span class="circle" :class="{ pass: record.enabled }"></span>{{ record.enabled ? '正常' : '已停用' }}</template>
          </template>
          <div v-else class="grp">
            <a-avatar :size="24" :style="{ backgroundColor: color(record.user.username) }">{{ record.user.username[0].toUpperCase() }}</a-avatar>
            <span class="nm">{{ record.user.username }}</span>
            <a-tag size="small">{{ record.children.length }} 条</a-tag>
            <a-tag v-if="record.children.some(r => r.enabled && r.error)" size="small" color="red">异常 {{ record.children.filter(r => r.enabled && r.error).length }}</a-tag>
            <a-badge :status="userBadge(record.user).badge" :text="userBadge(record.user).text" />
            <div class="info">
              <span v-for="g in allocGroups(record.user)" :key="g.node">{{ g.name }} <b class="mono">{{ g.specs }}</b></span>
              <span>虚拟机 <b>{{ record.user.vmIds?.length || 0 }}</b> 台</span>
              <span>流量 <b>{{ fmtBytes(record.user.usedBytes) }}</b> / {{ record.user.quotaBytes ? fmtBytes(record.user.quotaBytes) : '不限' }}</span>
            </div>
            <a-space style="margin-left:auto" @click.stop>
              <a-button type="text" size="mini" @click="$router.push(`/users/${record.user.id}`)">编辑用户</a-button>
              <a-button type="text" size="mini" @click="openRule(null, record.user.id)">代加规则</a-button>
            </a-space>
          </div>
        </template>
        <template #node="{ record }"><a-tag size="small" :color="record.nodeStatus === 'online' ? 'arcoblue' : 'gray'">{{ record.nodeName }}</a-tag></template>
        <template #remark="{ record }">{{ record.remark || '-' }}</template>
        <template #entry="{ record }"><div class="lines">
          <div v-if="record.ipVer !== 'v6'" class="mono">{{ record.entry4 }}:{{ record.port }}</div>
          <div v-if="record.ipVer !== 'v4'" class="mono">[{{ record.entry6 }}]:{{ record.port }}</div></div></template>
        <template #exit="{ record }"><div class="lines">
          <div v-if="record.ipVer !== 'v6'" class="mono">{{ record.target4 || '-' }}:{{ record.targetPort }}</div>
          <div v-if="record.ipVer !== 'v4'" class="mono">[{{ record.target6 || '-' }}]:{{ record.targetPort }}</div>
          <a-space :size="4"><span class="muted"><span class="mono">{{ record.vmVmid }}</span> {{ record.vmName }}</span><a-tag v-if="record.vmShared" size="small" color="arcoblue">公共</a-tag></a-space>
        </div></template>
        <template #ipver="{ record }"><a-tag :color="{ v4: 'arcoblue', v6: 'purple', both: 'green' }[record.ipVer]">{{ ipverName[record.ipVer] }}</a-tag></template>
        <template #proto="{ record }"><a-tag>{{ protoName[record.proto] }}</a-tag></template>
        <template #traffic="{ record }"><div class="updown"><div>累计 {{ fmtBytes(record.bytesUp + record.bytesDown) }}</div>
          <a-tooltip content="近 24 小时：下行（虚拟机 → 客户端） / 上行"><span><span class="d"><icon-arrow-down /> {{ fmtBytes(record.down24) }}</span>&nbsp; <span class="u"><icon-arrow-up /> {{ fmtBytes(record.up24) }}</span></span></a-tooltip></div></template>
        <template #conn="{ record }">{{ record.enabled && !record.error ? record.conns : '-' }}</template>
        <template #switch="{ record }"><a-switch :model-value="record.enabled" size="small" @change="v => toggle(record, v)" /></template>
        <template #operations="{ record }">
          <a-space :size="0"><a-button type="text" size="small" @click="openRule(record)">编辑</a-button>
            <a-popconfirm content="删除后该端口立即停止转发，确定删除？" type="warning" @ok="remove(record)"><a-button type="text" size="small" status="danger">删除</a-button></a-popconfirm></a-space>
        </template>
        <template #empty><a-empty :description="rules.length ? '没有符合条件的规则' : '还没有转发规则'" /></template>
      </a-table>
    </a-card>
    <RuleModal v-model:visible="modal" :rule="editing" :user-id="modalUser" @saved="load" />
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { useRoute } from 'vue-router'
import { api, isAdmin, fmtBytes, protoName, ipverName } from '../api'
import RuleModal from '../components/RuleModal.vue'

const route = useRoute()
const admin = computed(isAdmin)
const rules = ref([])
const users = ref([])
const me = ref(null)
const loading = ref(false)
const q = reactive({ owner: route.query.owner || undefined, status: route.query.status || undefined })
const grouped = ref(true)
const selected = ref([])
const expanded = ref([])
const modal = ref(false)
const editing = ref(null)
const modalUser = ref(undefined)

const palette = ['#00b42a', '#ff7d00', '#722ed1', '#165dff', '#f53f3f', '#14c9c9', '#eb0aa4', '#7bc616']
const color = name => palette[[...name].reduce((s, c) => s + c.charCodeAt(0), 0) % palette.length]

async function load() {
  loading.value = true
  try {
    const tasks = [api.get('/api/rules')]
    if (admin.value) tasks.push(api.get('/api/users'))
    else tasks.push(api.get('/api/me'))
    const [r, u] = await Promise.all(tasks)
    rules.value = r
    if (admin.value) {
      users.value = u
      if (!expanded.value.length) expanded.value = u.map(x => 'g' + x.id)
    } else me.value = u
  } finally { loading.value = false }
}
let timer
onMounted(() => { load(); timer = setInterval(() => { if (!modal.value) load() }, 30000) })
onUnmounted(() => clearInterval(timer))

function reset() { for (const k of Object.keys(q)) delete q[k] }

const state = r => (!r.enabled ? 'off' : r.error ? 'err' : 'on')
const filtered = computed(() => rules.value.filter(r =>
  (!q.node || r.nodeId === q.node) && (!q.owner || r.owner === q.owner) && (!q.proto || r.proto === q.proto) &&
  (!q.ipVer || r.ipVer === q.ipVer) && (!q.status || state(r) === q.status) &&
  (!q.kw || [r.port, r.remark, r.target4, r.target6, r.vmName, r.vmVmid, r.entry4].join(' ').toLowerCase().includes(q.kw.toLowerCase()))))
const errRules = computed(() => rules.value.filter(r => r.enabled && r.error))
const nodeOptions = computed(() => [...new Map(rules.value.map(r => [r.nodeId, { label: r.nodeName, value: r.nodeId }])).values()])
const ownerOptions = computed(() => (admin.value ? users.value.map(u => ({ label: u.username, value: u.username })) : []))

const tableRows = computed(() => {
  const list = filtered.value.map(r => Object.assign(r, { key: 'r' + r.id }))
  if (!admin.value || !grouped.value) return list
  return users.value.filter(u => !q.owner || u.username === q.owner)
    .map(u => ({ key: 'g' + u.id, group: true, user: u, children: list.filter(r => r.userId === u.id) }))
    .filter(g => g.children.length || (!q.status && !q.kw && !q.node && !q.proto && !q.ipVer))
})
const selectedRules = computed(() => selected.value.filter(k => String(k).startsWith('r')).map(k => Number(String(k).slice(1))))

const columns = computed(() => [
  { title: '状态', slotName: 'status', width: 100 },
  { title: '节点', slotName: 'node', width: 100 },
  { title: '规则备注', slotName: 'remark', width: 130, ellipsis: true, tooltip: true },
  ...(admin.value && !grouped.value ? [{ title: '所属用户', dataIndex: 'owner', width: 100 }] : []),
  { title: '转发入口', slotName: 'entry', width: 220 },
  { title: '转发出口', slotName: 'exit', width: 200 },
  { title: 'IP 版本', slotName: 'ipver', width: 110 },
  { title: '协议', slotName: 'proto', width: 100 },
  { title: '流量', slotName: 'traffic', width: 160 },
  { title: '连接', slotName: 'conn', width: 60 },
  { title: '启用', slotName: 'switch', width: 60 },
  { title: '操作', slotName: 'operations', width: 120 },
])
const spanMethod = ({ record, columnIndex }) => {
  if (!record.group) return
  return columnIndex === 0 ? { rowspan: 1, colspan: columns.value.length } : { rowspan: 0, colspan: 0 }
}

const userBadge = u => (!u.enabled ? { badge: 'normal', text: '已禁用' } : u.quotaBytes && u.usedBytes >= u.quotaBytes ? { badge: 'danger', text: '流量超限' } : { badge: 'success', text: '正常' })
const nodeName = id => rules.value.find(r => r.nodeId === id)?.nodeName || `节点 ${id}`
function allocGroups(u) {
  const m = new Map()
  for (const a of u.allocations || []) {
    if (!m.has(a.nodeId)) m.set(a.nodeId, [])
    m.get(a.nodeId).push(a.spec)
  }
  return [...m].map(([node, specs]) => ({ node, name: nodeName(node), specs: specs.join(' · ') }))
}

const quotaRows = computed(() => {
  const m = me.value
  if (!m) return []
  const rows = []
  const byNode = new Map()
  for (const a of m.allocations) {
    if (!byNode.has(a.nodeId)) byNode.set(a.nodeId, [])
    byNode.get(a.nodeId).push(a)
  }
  for (const [nid, list] of byNode) {
    const total = list.reduce((s, a) => s + a.count, 0)
    rows.push({ label: (m.nodeNames[nid] || '节点') + ' 端口', value: `${list.map(a => a.spec).join(' · ')}（已用 ${(m.usedPorts[nid] || []).length}/${total}）` })
  }
  rows.push({ label: '规则', value: `${m.rules} / ${m.user.maxRules || '不限'}` })
  rows.push({ label: '我的虚拟机', value: `${m.vms} 台` })
  rows.push({ label: '流量', value: `${fmtBytes(m.user.usedBytes)} / ${m.user.quotaBytes ? fmtBytes(m.user.quotaBytes) : '不限'}` })
  return rows
})

function openRule(r = null, uid) { editing.value = r; modalUser.value = uid; modal.value = true }
async function toggle(r, v) {
  await api.put(`/api/rules/${r.id}`, { enabled: v })
  r.enabled = v
  Message.success(v ? '已启用' : '已停用')
  load()
}
async function remove(r) { await api.del(`/api/rules/${r.id}`); Message.success('已删除'); load() }
async function batch(action) {
  const res = await api.post('/api/rules/batch', { ids: selectedRules.value, action })
  Message.success(`已${{ enable: '启用', disable: '停用', delete: '删除' }[action]} ${res.count} 条规则`)
  selected.value = []
  load()
}
</script>
