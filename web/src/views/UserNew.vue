<template>
  <div class="container" style="padding-top:16px">
    <a-card class="general-card">
      <template #title>添加用户</template>
      <div class="step-wrapper">
        <a-steps :current="step" style="width: 760px; max-width: 100%" line-less class="steps">
          <a-step description="用户名与配额">账号信息</a-step>
          <a-step description="按节点分配端口">端口分配</a-step>
          <a-step description="选择 PVE 虚拟机">虚拟机</a-step>
          <a-step description="创建成功">完成</a-step>
        </a-steps>

        <a-form v-if="step === 1" :model="f" class="step-form" style="width:520px;max-width:100%" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }">
          <a-form-item label="用户名" required :validate-status="err.name ? 'error' : undefined" :help="err.name">
            <a-input v-model="f.username" placeholder="字母、数字、下划线，3-20 位" /></a-form-item>
          <a-form-item label="密码" required :validate-status="err.pwd ? 'error' : undefined" :help="err.pwd">
            <a-input-password v-model="f.password" placeholder="至少 8 位" /></a-form-item>
          <a-form-item label="流量上限" extra="所有节点合计，留空或 0 为不限"><a-input-number v-model="f.quotaGb" placeholder="不限" :min="0"><template #append>GB</template></a-input-number></a-form-item>
          <a-form-item label="状态"><a-radio-group v-model="f.enabled"><a-radio :value="true">启用</a-radio><a-radio :value="false">禁用</a-radio></a-radio-group></a-form-item>
          <a-form-item label="备注"><a-textarea v-model="f.remark" placeholder="选填" :max-length="100" show-word-limit /></a-form-item>
          <a-form-item><a-button type="primary" @click="next">下一步</a-button></a-form-item>
        </a-form>

        <div v-else-if="step === 2" class="step-form" style="width:900px;max-width:100%">
          <a-alert v-if="!nodes.length" type="warning" style="margin-bottom:16px">还没有已初始化的节点，请先在「PVE 节点 → 添加节点」中接入并初始化节点</a-alert>
          <a-form :model="{}" layout="vertical"><AllocEditor ref="allocRef" v-model="f.allocations" :nodes="nodes" /></a-form>
          <div v-if="err.ports" style="color:rgb(var(--red-6));margin-top:8px">{{ err.ports }}</div>
          <div style="text-align:center;margin-top:32px"><a-space><a-button type="secondary" @click="step--">上一步</a-button><a-button type="primary" @click="next">下一步</a-button></a-space></div>
        </div>

        <div v-else-if="step === 3" class="step-form" style="width:1000px;max-width:100%">
          <VMPicker v-model="f.vmIds" :vms="vms" :nodes="nodes" @reload="loadVMs" />
          <div style="text-align:center;margin-top:32px"><a-space>
            <a-button type="secondary" @click="step--">上一步</a-button>
            <a-button type="primary" :loading="saving" @click="submit">{{ f.vmIds.length ? '提交' : '暂不分配，提交' }}</a-button></a-space></div>
        </div>

        <div v-else class="success-wrap">
          <a-result status="success" title="用户创建成功" :subtitle="`账号 ${f.username} 已可登录，端口与虚拟机已下发到对应节点`" />
          <a-space :size="16"><a-button type="primary" @click="$router.push('/users')">查看用户列表</a-button><a-button type="secondary" @click="restart">继续添加</a-button></a-space>
          <div class="details-wrapper">
            <a-typography-title :heading="6" style="margin-top: 0">创建信息</a-typography-title>
            <a-descriptions :column="{ xs: 1, md: 2 }" :data="summary" />
          </div>
        </div>
      </div>
    </a-card>
  </div>
</template>

<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import AllocEditor from '../components/AllocEditor.vue'
import VMPicker from '../components/VMPicker.vue'

const step = ref(1)
const err = reactive({})
const saving = ref(false)
const nodes = ref([])
const vms = ref([])
const allocRef = ref(null)
const fresh = () => ({ username: '', password: '', quotaGb: undefined, maxRules: 0, enabled: true, remark: '', allocations: [], vmIds: [] })
const f = reactive(fresh())

async function loadVMs() { vms.value = await api.get('/api/vms') }
onMounted(async () => {
  const d = await api.get('/api/nodes')
  nodes.value = d.nodes.filter(n => n.initialized)
  loadVMs()
})

const clear = () => Object.keys(err).forEach(k => delete err[k])
async function next() {
  clear()
  if (step.value === 1) {
    if (!/^[A-Za-z0-9_]{3,20}$/.test(f.username)) err.name = '用户名需为 3-20 位字母、数字或下划线'
    else {
      const users = await api.get('/api/users')
      if (users.some(u => u.username === f.username)) err.name = '用户名已存在'
    }
    if (f.password.length < 8) err.pwd = '密码至少 8 位'
  }
  if (step.value === 2) {
    if (!(await allocRef.value.flush())) return
    if (!f.allocations.length) err.ports = '至少分配 1 个端口'
  }
  if (!Object.keys(err).length) step.value++
}
async function submit() {
  saving.value = true
  try {
    await api.post('/api/users', { ...f, quotaGb: f.quotaGb || 0 })
    await loadVMs()
    step.value = 4
  } finally { saving.value = false }
}
function restart() { Object.assign(f, fresh()); step.value = 1 }

const nodeName = id => nodes.value.find(n => n.id === id)?.name
const summary = computed(() => [
  { label: '用户名', value: f.username }, { label: '状态', value: f.enabled ? '启用' : '禁用' },
  { label: '流量上限', value: f.quotaGb ? f.quotaGb + ' GB' : '不限' }, { label: '端口数量', value: f.allocations.reduce((s, a) => s + a.count, 0) + ' 个' },
  { label: '端口', value: f.allocations.map(a => `${nodeName(a.nodeId)}：${a.spec}`).join('；') },
  { label: '虚拟机', value: vms.value.filter(v => f.vmIds.includes(v.id)).map(v => `${v.nodeName}/${v.vmid} ${v.name}（${v.ipv4 || '-'}）`).join('，') || '未分配' },
])
</script>
