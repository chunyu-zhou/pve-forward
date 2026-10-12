<template>
  <div>
    <a-row :gutter="24" align="start">
      <a-col :xs="24" :md="6"><a-form-item label="节点" :label-col-flex="compact ? '60px' : undefined">
        <a-select v-model="f.nodeId" :options="nodes.map(n => ({ label: n.name, value: n.id }))" placeholder="选择节点" /></a-form-item></a-col>
      <a-col :xs="24" :md="7"><a-form-item label="分配方式">
        <a-radio-group v-model="f.kind" type="button"><a-radio value="single">单个</a-radio><a-radio value="multi">多个</a-radio><a-radio value="range">范围</a-radio></a-radio-group></a-form-item></a-col>
      <a-col :xs="24" :md="11"><a-form-item :label="{ single: '端口', multi: '端口列表', range: '端口范围' }[f.kind]" :validate-status="err ? 'error' : undefined"
          :help="err || '同一节点内端口不可重复，不同节点可使用相同端口号'">
        <a-space wrap>
          <a-input-number v-if="f.kind === 'single'" v-model="f.single" model-event="input" :min="1" :max="65535" placeholder="例如 10022" style="width:200px" />
          <a-input-tag v-if="f.kind === 'multi'" v-model="f.multi" placeholder="输入后回车，可粘贴 8001,8002" style="width:300px" allow-clear :retain-input-value="false" />
          <template v-if="f.kind === 'range'">
            <a-input-number v-model="f.from" model-event="input" :min="1" :max="65535" placeholder="起始端口" style="width:130px" /><span>~</span>
            <a-input-number v-model="f.to" model-event="input" :min="1" :max="65535" placeholder="结束端口" style="width:130px" />
          </template>
          <a-button type="primary" :loading="checking" @click="add"><template #icon><icon-plus /></template>添加</a-button>
        </a-space>
      </a-form-item></a-col>
    </a-row>
    <a-table :data="modelValue" :pagination="false" :bordered="false" size="small" row-key="_k">
      <template #columns>
        <a-table-column title="节点" :width="120"><template #cell="{ record }"><a-tag size="small" color="arcoblue">{{ nodeName(record.nodeId) }}</a-tag></template></a-table-column>
        <a-table-column title="类型" :width="90"><template #cell="{ record }"><a-tag size="small">{{ allocKindName[record.kind] }}</a-tag></template></a-table-column>
        <a-table-column title="端口"><template #cell="{ record }"><span class="mono">{{ record.spec }}</span></template></a-table-column>
        <a-table-column title="数量" :width="80"><template #cell="{ record }">{{ record.count }}</template></a-table-column>
        <a-table-column v-if="usedPorts" title="已被规则使用" :width="120"><template #cell="{ record }">{{ usedIn(record) }}</template></a-table-column>
        <a-table-column title="操作" :width="80"><template #cell="{ rowIndex, record }">
          <a-popconfirm v-if="usedPorts && usedIn(record)" content="其中端口已被规则使用，保存后对应规则会一并删除，确定移除？" type="warning" @ok="removeAt(rowIndex)">
            <a-button type="text" size="mini" status="danger">移除</a-button></a-popconfirm>
          <a-button v-else type="text" size="mini" status="danger" @click="removeAt(rowIndex)">移除</a-button></template></a-table-column>
      </template>
      <template #empty><a-empty description="尚未分配端口" /></template>
    </a-table>
  </div>
</template>

<script setup>
import { reactive, ref, watch } from 'vue'
import { api, allocKindName, expandAlloc } from '../api'

const props = defineProps({ modelValue: Array, nodes: Array, userId: Number, usedPorts: Object, compact: Boolean })
const emit = defineEmits(['update:modelValue'])
const f = reactive({ nodeId: undefined, kind: 'range', single: undefined, multi: [], from: undefined, to: undefined })
const err = ref('')
const checking = ref(false)
watch(() => props.nodes, n => { if (!f.nodeId && n?.length) f.nodeId = n[0].id }, { immediate: true })

const nodeName = id => props.nodes.find(n => n.id === id)?.name || `节点 ${id}`
const usedIn = a => { const u = new Set(props.usedPorts?.[a.nodeId] || []); return expandAlloc(a).filter(p => u.has(p)).length }

async function add() {
  err.value = ''
  if (!f.nodeId) { err.value = '请选择节点'; return }
  const spec = f.kind === 'single' ? String(f.single ?? '') : f.kind === 'multi' ? f.multi.join(',') : `${f.from ?? ''}-${f.to ?? ''}`
  checking.value = true
  try {
    const r = await api.post('/api/users/check-alloc', { userId: props.userId || 0, alloc: { nodeId: f.nodeId, kind: f.kind, spec }, pending: props.modelValue }, { silent: true })
    emit('update:modelValue', [...props.modelValue, { _k: Math.random(), nodeId: f.nodeId, kind: f.kind, spec: r.spec, count: r.count }])
    Object.assign(f, { single: undefined, multi: [], from: undefined, to: undefined })
  } catch (e) { err.value = e.message } finally { checking.value = false }
}
function removeAt(i) { const a = [...props.modelValue]; a.splice(i, 1); emit('update:modelValue', a) }
</script>
