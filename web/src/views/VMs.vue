<template>
  <div class="container" style="padding-top:16px">
    <a-card class="general-card" title="虚拟机">
      <a-row style="margin-bottom: 16px" :gutter="8">
        <a-col :span="18"><a-space wrap>
          <a-select v-model="q.node" style="width:150px" :options="[{ label: '全部节点', value: 0 }, ...nodes.map(n => ({ label: n.name, value: n.id }))]" />
          <a-input-search v-model="q.kw" placeholder="VMID / 名称 / IP / MAC" style="width:220px" allow-clear />
          <a-select v-model="q.owner" placeholder="全部归属" allow-clear style="width:140px">
            <a-option value="__free">未分配</a-option><a-option value="__public">公共</a-option>
            <a-option v-for="u in users" :key="u.id" :value="u.id">{{ u.username }}</a-option></a-select>
          <a-radio-group v-model="q.type" type="button"><a-radio value="">全部</a-radio><a-radio value="qemu">VM</a-radio><a-radio value="lxc">CT</a-radio></a-radio-group>
        </a-space></a-col>
        <a-col :span="6" style="display:flex;align-items:center;justify-content:end;gap:12px">
          <span class="muted">共 {{ vms.length }} 台，未分配 {{ vms.filter(v => !v.userId && !v.shared).length }} 台</span>
          <a-button size="small" :loading="syncing" @click="syncAll"><template #icon><icon-sync /></template>同步</a-button>
        </a-col>
      </a-row>
      <a-table row-key="id" :data="rows" :loading="loading" :pagination="{ showTotal: true, pageSize: 20 }" :bordered="false" :scroll="{ x: 1280 }">
        <template #columns>
          <a-table-column title="节点" :width="100"><template #cell="{ record }"><a-tag size="small" color="arcoblue">{{ record.nodeName }}</a-tag></template></a-table-column>
          <a-table-column title="VMID" :width="80"><template #cell="{ record }"><span class="mono">{{ record.vmid }}</span></template></a-table-column>
          <a-table-column title="名称" data-index="name" :width="140" :ellipsis="true" :tooltip="true" />
          <a-table-column title="类型" :width="70"><template #cell="{ record }"><a-tag size="small" :color="record.type === 'qemu' ? 'arcoblue' : 'cyan'">{{ record.type === 'qemu' ? 'VM' : 'CT' }}</a-tag></template></a-table-column>
          <a-table-column title="状态" :width="90"><template #cell="{ record }">
            <template v-if="!record.present"><span class="circle err"></span>已删除</template>
            <template v-else><span class="circle" :class="{ pass: record.status === 'running' }"></span>{{ record.status === 'running' ? '运行中' : '已停止' }}</template></template></a-table-column>
          <a-table-column title="归属" :width="110"><template #cell="{ record }">
            <a-tag v-if="record.shared" color="arcoblue" size="small">公共</a-tag><a-space v-else-if="record.owner" :size="6"><icon-user />{{ record.owner }}</a-space><span v-else class="muted">未分配</span></template></a-table-column>
          <a-table-column title="MAC（内网网桥）" :width="160"><template #cell="{ record }"><span v-if="record.mac" class="mono">{{ record.mac }}</span><span v-else class="muted">未接入</span></template></a-table-column>
          <a-table-column title="IPv4" :width="120"><template #cell="{ record }"><span class="mono">{{ record.ipv4 || '-' }}</span></template></a-table-column>
          <a-table-column title="IPv6" :width="150"><template #cell="{ record }"><span class="mono">{{ record.ipv6 || '-' }}</span></template></a-table-column>
          <a-table-column title="规则" :width="70"><template #cell="{ record }">{{ record.rules }} 条</template></a-table-column>
          <a-table-column title="操作" :width="230"><template #cell="{ record }"><a-space :size="0">
            <a-dropdown @select="uid => act(record, 'assign', uid)">
              <a-button type="text" size="small" :disabled="!record.mac || !record.present">分配给 <icon-down /></a-button>
              <template #content><a-doption v-for="u in users" :key="u.id" :value="u.id" :disabled="u.id === record.userId">{{ u.username }}</a-doption></template>
            </a-dropdown>
            <a-button type="text" size="small" :disabled="!record.mac || !record.present" @click="act(record, record.shared ? 'unshare' : 'share')">{{ record.shared ? '取消公共' : '设为公共' }}</a-button>
            <a-popconfirm v-if="record.userId || record.shared" content="收回后 IP 保留，引用它的规则会停止生效，确定收回？" type="warning" @ok="act(record, 'reclaim')">
              <a-button type="text" size="small" status="danger">收回</a-button></a-popconfirm></a-space></template></a-table-column>
        </template>
      </a-table>
    </a-card>
  </div>
</template>

<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api } from '../api'

const vms = ref([])
const nodes = ref([])
const users = ref([])
const loading = ref(false)
const syncing = ref(false)
const q = reactive({ node: 0, kw: '', owner: undefined, type: '' })

async function load() {
  loading.value = true
  try {
    const [v, n, u] = await Promise.all([api.get('/api/vms'), api.get('/api/nodes'), api.get('/api/users')])
    vms.value = v; nodes.value = n.nodes.filter(x => x.initialized); users.value = u
  } finally { loading.value = false }
}
onMounted(load)

const rows = computed(() => vms.value.filter(v => (!q.node || v.nodeId === q.node) && (!q.type || v.type === q.type) &&
  (!q.owner || (q.owner === '__free' ? !v.userId && !v.shared : q.owner === '__public' ? v.shared : v.userId === q.owner)) &&
  (!q.kw || `${v.vmid} ${v.name} ${v.mac} ${v.ipv4} ${v.ipv6}`.toLowerCase().includes(q.kw.toLowerCase()))))

async function act(v, action, userId) {
  const r = await api.put(`/api/vms/${v.id}`, { action, userId })
  Message.success(action === 'assign' ? `已分配，IP ${r.ipv4}` : '操作成功')
  load()
}
async function syncAll() {
  syncing.value = true
  try {
    const ids = q.node ? [q.node] : nodes.value.map(n => n.id)
    const res = await Promise.allSettled(ids.map(id => api.post(`/api/nodes/${id}/sync`, {}, { silent: true })))
    const bad = res.filter(r => r.status === 'rejected').length
    bad ? Message.warning(`${bad} 个节点同步失败`) : Message.success('已同步')
    load()
  } finally { syncing.value = false }
}
</script>
