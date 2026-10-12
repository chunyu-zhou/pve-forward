<template>
  <div class="container" style="padding-top:16px">
    <a-card class="general-card" title="用户列表">
      <a-row>
        <a-col :flex="1">
          <a-form :model="q" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }" label-align="left">
            <a-row :gutter="16">
              <a-col :span="8"><a-form-item label="用户名"><a-input v-model="q.name" placeholder="请输入用户名" allow-clear /></a-form-item></a-col>
              <a-col :span="8"><a-form-item label="节点"><a-select v-model="q.node" placeholder="全部" allow-clear :options="nodes.map(n => ({ label: n.name, value: n.id }))" /></a-form-item></a-col>
              <a-col :span="8"><a-form-item label="状态"><a-select v-model="q.status" placeholder="全部" allow-clear :options="['正常', '已禁用', '流量超限']" /></a-form-item></a-col>
              <a-col :span="8"><a-form-item label="包含端口"><a-input-number v-model="q.port" placeholder="例如 20001" :min="1" :max="65535" allow-clear /></a-form-item></a-col>
              <a-col :span="8"><a-form-item label="虚拟机"><a-input v-model="q.vm" placeholder="VMID / 名称 / IP" allow-clear /></a-form-item></a-col>
            </a-row>
          </a-form>
        </a-col>
        <a-divider style="height: 84px" direction="vertical" />
        <a-col flex="86px" style="text-align: right">
          <a-space direction="vertical" :size="18">
            <a-button type="primary" @click="load"><template #icon><icon-search /></template>查询</a-button>
            <a-button @click="Object.keys(q).forEach(k => delete q[k])"><template #icon><icon-refresh /></template>重置</a-button>
          </a-space>
        </a-col>
      </a-row>
      <a-divider style="margin-top: 0" />
      <a-row style="margin-bottom: 16px">
        <a-col :span="12"><a-button type="primary" @click="$router.push('/users/new')"><template #icon><icon-plus /></template>添加用户</a-button></a-col>
        <a-col :span="12" style="display:flex;align-items:center;justify-content:end">
          <span class="muted">已分配 {{ users.reduce((s, u) => s + u.allocations.reduce((t, a) => t + a.count, 0), 0) }} 个端口 · {{ vms.filter(v => v.userId).length }} 台虚拟机</span>
          <a-tooltip content="刷新"><div class="action-icon" @click="load"><icon-refresh size="18" /></div></a-tooltip>
        </a-col>
      </a-row>
      <a-table row-key="id" :data="rows" :loading="loading" :pagination="{ showTotal: true, pageSize: 20 }" :bordered="false" :scroll="{ x: 1260 }">
        <template #columns>
          <a-table-column title="用户" :width="140"><template #cell="{ record }">
            <a-space><a-avatar :size="24" :style="{ backgroundColor: color(record.username) }">{{ record.username[0].toUpperCase() }}</a-avatar>{{ record.username }}</a-space>
            <div v-if="record.remark" class="muted">{{ record.remark }}</div></template></a-table-column>
          <a-table-column title="状态" :width="100"><template #cell="{ record }">
            <span class="circle" :class="{ pass: status(record) === '正常', err: status(record) === '流量超限' }"></span>{{ status(record) }}</template></a-table-column>
          <a-table-column title="端口分配（按节点）" :width="300"><template #cell="{ record }">
            <div v-for="g in byNode(record)" :key="g.node" style="line-height:1.9;white-space:normal">
              <a-tag size="small" color="arcoblue">{{ g.name }}</a-tag> <span class="mono">{{ g.specs }}</span>
              <span class="muted"> 已用 {{ (record.usedPorts[g.node] || []).length }}/{{ g.total }}</span></div>
            <span v-if="!record.allocations.length" class="muted">未分配</span></template></a-table-column>
          <a-table-column title="虚拟机" :width="260"><template #cell="{ record }">
            <div v-for="v in vmsOf(record)" :key="v.id" style="line-height:1.9"><span class="muted">{{ v.nodeName }}/</span><span class="mono">{{ v.vmid }}</span> {{ v.name }} <span class="muted mono">{{ v.ipv4 }}</span></div>
            <span v-if="!record.vmIds.length" class="muted">未分配</span></template></a-table-column>
          <a-table-column title="规则" :width="120"><template #cell="{ record }">{{ record.rules }} / {{ record.maxRules || '不限' }}
            <a-tag v-if="record.errRules" size="small" color="red" style="margin-left:4px">异常 {{ record.errRules }}</a-tag></template></a-table-column>
          <a-table-column title="流量" :width="170"><template #cell="{ record }">{{ fmtBytes(record.usedBytes) }} / {{ record.quotaBytes ? fmtBytes(record.quotaBytes) : '不限' }}
            <a-progress v-if="record.quotaBytes" :percent="Math.min(record.usedBytes / record.quotaBytes, 1)" :show-text="false" size="small" :color="lvl(pct(record.usedBytes, record.quotaBytes))" /></template></a-table-column>
          <a-table-column title="操作" :width="220"><template #cell="{ record }"><a-space :size="0">
            <a-button type="text" size="small" @click="$router.push(`/users/${record.id}`)">编辑</a-button>
            <a-button type="text" size="small" @click="$router.push({ path: '/rules', query: { owner: record.username } })">查看规则</a-button>
            <a-dropdown @select="a => action(record, a)">
              <a-button type="text" size="small">更多 <icon-down /></a-button>
              <template #content>
                <a-doption value="reset-password">重置密码</a-doption>
                <a-doption value="reset-traffic">清零已用流量</a-doption>
                <a-doption :value="record.enabled ? 'disable' : 'enable'">{{ record.enabled ? '禁用' : '启用' }}</a-doption>
                <a-doption value="delete" style="color:rgb(var(--red-6))">删除用户</a-doption>
              </template>
            </a-dropdown></a-space></template></a-table-column>
        </template>
      </a-table>
    </a-card>
    <a-modal v-model:visible="pwd.visible" title="重置密码" :width="400" @ok="resetPwd">
      <a-input-password v-model="pwd.value" placeholder="新密码，至少 8 位" />
    </a-modal>
  </div>
</template>

<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { Message, Modal } from '@arco-design/web-vue'
import { api, fmtBytes, lvl, pct, expandAlloc } from '../api'

const users = ref([])
const nodes = ref([])
const vms = ref([])
const loading = ref(false)
const q = reactive({})
const palette = ['#00b42a', '#ff7d00', '#722ed1', '#165dff', '#f53f3f', '#14c9c9', '#eb0aa4', '#7bc616']
const color = name => palette[[...name].reduce((s, c) => s + c.charCodeAt(0), 0) % palette.length]

async function load() {
  loading.value = true
  try {
    const [u, n, v] = await Promise.all([api.get('/api/users'), api.get('/api/nodes'), api.get('/api/vms')])
    users.value = u
    nodes.value = n.nodes
    vms.value = v
  } finally { loading.value = false }
}
onMounted(load)

const status = u => (!u.enabled ? '已禁用' : u.quotaBytes && u.usedBytes >= u.quotaBytes ? '流量超限' : '正常')
const vmsOf = u => vms.value.filter(v => u.vmIds.includes(v.id))
function byNode(u) {
  const m = new Map()
  for (const a of u.allocations) {
    if (!m.has(a.nodeId)) m.set(a.nodeId, [])
    m.get(a.nodeId).push(a)
  }
  return [...m].map(([node, list]) => ({ node, name: nodes.value.find(n => n.id === node)?.name || node, specs: list.map(a => a.spec).join(' · '), total: list.reduce((s, a) => s + a.count, 0) }))
}
const rows = computed(() => users.value.filter(u =>
  (!q.name || u.username.includes(q.name)) && (!q.status || status(u) === q.status) &&
  (!q.node || u.allocations.some(a => a.nodeId === q.node)) &&
  (!q.port || u.allocations.some(a => expandAlloc(a).includes(q.port))) &&
  (!q.vm || vmsOf(u).some(v => `${v.vmid} ${v.name} ${v.ipv4}`.includes(q.vm)))))

const pwd = reactive({ visible: false, value: '', user: null })
async function action(u, a) {
  if (a === 'reset-password') { Object.assign(pwd, { visible: true, value: '', user: u }); return }
  if (a === 'delete') {
    Modal.warning({ title: '删除用户', content: `确定删除 ${u.username}？其全部规则会被删除，端口与虚拟机将被收回。`, hideCancel: false,
      onOk: async () => { await api.del(`/api/users/${u.id}`); Message.success('已删除'); load() } })
    return
  }
  await api.post(`/api/users/${u.id}/action`, { action: a })
  Message.success('操作成功')
  load()
}
async function resetPwd() {
  if (pwd.value.length < 8) { Message.warning('密码至少 8 位'); return false }
  await api.post(`/api/users/${pwd.user.id}/action`, { action: 'reset-password', password: pwd.value })
  Message.success('密码已重置')
}
</script>
