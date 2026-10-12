<template>
  <div>
    <a-row justify="space-between" align="center" style="margin-bottom:12px" :gutter="8">
      <a-space wrap>
        <a-select v-model="node" style="width:150px" :options="[{ label: '全部节点', value: 0 }, ...nodes.map(n => ({ label: n.name, value: n.id }))]" />
        <a-input-search v-model="kw" placeholder="VMID / 名称 / MAC" style="width:200px" allow-clear />
        <a-checkbox v-model="onlyFree">只看可分配</a-checkbox>
      </a-space>
      <a-space><span class="muted">由各节点上报，仅含接入内网网桥的虚拟机</span>
        <a-button size="small" :loading="syncing" @click="sync"><template #icon><icon-sync /></template>立即同步</a-button></a-space>
    </a-row>
    <a-table row-key="id" :data="rows" :pagination="false" :bordered="false" size="small" :scroll="{ y: 320, x: 900 }"
             :row-selection="{ type: 'checkbox', showCheckedAll: true }" :selected-keys="modelValue" @update:selected-keys="v => $emit('update:modelValue', v)">
      <template #columns>
        <a-table-column title="节点" :width="100"><template #cell="{ record }"><a-tag size="small" color="arcoblue">{{ record.nodeName }}</a-tag></template></a-table-column>
        <a-table-column title="VMID" :width="70"><template #cell="{ record }"><span class="mono">{{ record.vmid }}</span></template></a-table-column>
        <a-table-column title="名称" data-index="name" :width="130" :ellipsis="true" :tooltip="true" />
        <a-table-column title="类型" :width="60"><template #cell="{ record }"><a-tag size="small" :color="record.type === 'qemu' ? 'arcoblue' : 'cyan'">{{ record.type === 'qemu' ? 'VM' : 'CT' }}</a-tag></template></a-table-column>
        <a-table-column title="状态" :width="90"><template #cell="{ record }"><span class="circle" :class="{ pass: record.status === 'running' }"></span>{{ record.status === 'running' ? '运行中' : '已停止' }}</template></a-table-column>
        <a-table-column title="MAC" :width="150"><template #cell="{ record }"><span v-if="record.mac" class="mono">{{ record.mac }}</span><span v-else class="muted">未接入内网网桥</span></template></a-table-column>
        <a-table-column title="内网 IP"><template #cell="{ record }">
          <template v-if="record.ipv4"><span class="mono">{{ record.ipv4 }}</span><span v-if="record.ipv6" class="muted mono"> / {{ record.ipv6 }}</span></template>
          <a-tooltip v-else content="虚拟机开机并获得地址后自动识别（DHCP 租约、宿主机邻居表或 LXC 静态配置）">
            <span class="muted">尚未识别到 IP</span></a-tooltip></template></a-table-column>
        <a-table-column title="归属" :width="160"><template #cell="{ record }">
          <a-tag v-if="record.userId && record.userId !== userId" size="small">{{ record.owner }}</a-tag>
          <span v-else-if="record.userId" class="muted">当前用户</span><span v-else class="muted">未分配</span>
          <a-tag v-if="record.rules" size="small" color="orange" style="margin-left:4px">{{ record.rules }} 条规则</a-tag></template></a-table-column>
      </template>
      <template #empty><a-empty description="没有可分配的虚拟机，请确认节点已初始化并同步" /></template>
    </a-table>
    <a-alert style="margin-top:12px">
      已选 {{ selected.length }} 台<template v-if="selected.length">：{{ selected.map(v => `${v.nodeName}/${v.vmid} ${v.name}`).join('，') }}</template>。
      用户使用所选虚拟机现有的内网 IP，不会重新分配；由本系统管理 DHCP 时会固定该地址。归属其他用户的虚拟机不可选；取消已被规则使用的虚拟机，对应规则会停止生效。
    </a-alert>
  </div>
</template>

<script setup>
import { computed, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api } from '../api'

const props = defineProps({ modelValue: Array, vms: Array, nodes: Array, userId: Number })
const emit = defineEmits(['update:modelValue', 'reload'])
const node = ref(0)
const kw = ref('')
const onlyFree = ref(false)
const syncing = ref(false)

const rows = computed(() => props.vms
  .filter(v => v.present && !v.shared)
  .filter(v => !node.value || v.nodeId === node.value)
  .filter(v => !onlyFree.value || !v.userId || v.userId === props.userId)
  .filter(v => !kw.value || `${v.vmid} ${v.name} ${v.mac}`.toLowerCase().includes(kw.value.toLowerCase()))
  .map(v => ({ ...v, disabled: (!!v.userId && v.userId !== props.userId) || !v.mac })))
const selected = computed(() => props.modelValue.map(id => props.vms.find(v => v.id === id)).filter(Boolean))

async function sync() {
  syncing.value = true
  try {
    const ids = node.value ? [node.value] : props.nodes.map(n => n.id)
    const res = await Promise.allSettled(ids.map(id => api.post(`/api/nodes/${id}/sync`, {}, { silent: true })))
    const bad = res.filter(r => r.status === 'rejected').length
    if (bad) Message.warning(`${ids.length - bad} 个节点已同步，${bad} 个节点不可用`)
    else Message.success('已同步')
    emit('reload')
  } finally { syncing.value = false }
}
</script>
