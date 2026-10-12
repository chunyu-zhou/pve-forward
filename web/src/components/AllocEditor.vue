<template>
  <div>
    <a-row :gutter="24" align="start">
      <a-col :xs="24" :md="7"><a-form-item label="节点" :label-col-flex="compact ? '60px' : undefined">
        <a-select v-model="f.nodeId" :options="nodes.map(n => ({ label: n.name, value: n.id }))" placeholder="选择节点" /></a-form-item></a-col>
      <a-col :xs="24" :md="17"><a-form-item label="端口" :validate-status="err ? 'error' : undefined" :help="err || policyText">
        <a-space style="width:100%" fill>
          <a-input v-model="f.text" placeholder="直接输入，如 20000-20009,21000,21005" allow-clear style="width:380px;max-width:100%" @press-enter="add" />
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
import { computed, reactive, ref, watch } from 'vue'
import { api, allocKindName, expandAlloc } from '../api'

const props = defineProps({ modelValue: Array, nodes: Array, userId: Number, usedPorts: Object, compact: Boolean })
const emit = defineEmits(['update:modelValue'])
const f = reactive({ nodeId: undefined, text: '' })
const err = ref('')
const checking = ref(false)
watch(() => props.nodes, n => { if (!f.nodeId && n?.length) f.nodeId = n[0].id }, { immediate: true })

const nodeName = id => props.nodes.find(n => n.id === id)?.name || `节点 ${id}`
// 所选节点的端口策略：只能分配可分配范围内、非保留的端口
const policyText = computed(() => {
  const p = props.nodes.find(n => n.id === f.nodeId)?.ports
  if (!p) return '同一节点内端口不可重复，不同节点可使用相同端口号'
  return `可分配范围 ${p.range}` + (p.reserved ? `，保留端口 ${p.reserved}` : '') + '；同一节点内端口不可重复'
})
const usedIn = a => { const u = new Set(props.usedPorts?.[a.nodeId] || []); return expandAlloc(a).filter(p => u.has(p)).length }

// 把输入拆成分配记录：每个范围一条，单个端口合并为一条
function split(text) {
  const tokens = text.split(/[,，、\s]+/).filter(Boolean)
  const singles = [], out = []
  for (const t of tokens) {
    if (/^\d+$/.test(t)) singles.push(t)
    else if (/^\d+-\d+$/.test(t)) out.push({ kind: 'range', spec: t })
    else throw new Error(`“${t}” 不是有效的端口或范围`)
  }
  if (singles.length) out.unshift({ kind: singles.length === 1 ? 'single' : 'multi', spec: singles.join(',') })
  return out
}

// 添加输入框中的端口；返回是否成功
async function add() {
  err.value = ''
  if (!f.nodeId) { err.value = '请选择节点'; return false }
  let parts
  try { parts = split(f.text) } catch (e) { err.value = e.message; return false }
  if (!parts.length) { err.value = '请输入端口，如 20000-20009,21000'; return false }
  checking.value = true
  try {
    const added = []
    for (const p of parts) {
      const r = await api.post('/api/users/check-alloc', { userId: props.userId || 0, alloc: { nodeId: f.nodeId, ...p }, pending: [...props.modelValue, ...added] }, { silent: true })
      added.push({ _k: Math.random(), nodeId: f.nodeId, kind: p.kind, spec: r.spec, count: r.count })
    }
    emit('update:modelValue', [...props.modelValue, ...added])
    f.text = ''
    return true
  } catch (e) { err.value = e.message; return false } finally { checking.value = false }
}

// 下一步 / 保存前调用：输入框中还有未添加的端口时自动添加
defineExpose({ flush: async () => !f.text.trim() || add() })
function removeAt(i) { const a = [...props.modelValue]; a.splice(i, 1); emit('update:modelValue', a) }
</script>
