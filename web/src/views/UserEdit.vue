<template>
  <div class="container with-actions" style="padding-top:16px">
    <a-spin :loading="loading" style="width:100%">
      <a-form v-if="f" :model="f" layout="vertical">
        <a-space direction="vertical" :size="16" fill>
          <a-card class="general-card"><template #title>账号信息 · {{ f.username }}</template>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="用户名"><a-input v-model="f.username" disabled /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="新密码"><a-input-password v-model="f.password" placeholder="留空则不修改" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="状态"><a-radio-group v-model="f.enabled"><a-radio :value="true">启用</a-radio><a-radio :value="false">禁用</a-radio></a-radio-group></a-form-item></a-col>
            </a-row>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="流量上限" :extra="`所有节点合计，已用 ${fmtBytes(used)}`"><a-input-number v-model="f.quotaGb" placeholder="不限" :min="0"><template #append>GB</template></a-input-number></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="规则上限"><a-input-number v-model="f.maxRules" :min="1"><template #append>条</template></a-input-number></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="备注"><a-input v-model="f.remark" placeholder="选填" /></a-form-item></a-col>
            </a-row>
          </a-card>
          <a-card class="general-card"><template #title>端口分配</template>
            <template #extra><span class="muted">共 {{ f.allocations.reduce((s, a) => s + a.count, 0) }} 个端口</span></template>
            <AllocEditor v-model="f.allocations" :nodes="nodes" :user-id="Number(id)" :used-ports="usedPorts" />
          </a-card>
          <a-card class="general-card"><template #title>虚拟机分配</template>
            <template #extra><span class="muted">该用户的规则只能指向这些虚拟机或同节点的公共虚拟机</span></template>
            <VMPicker v-model="f.vmIds" :vms="vms" :nodes="nodes" :user-id="Number(id)" @reload="loadVMs" />
          </a-card>
        </a-space>
        <div class="actions" :style="{ left: (collapsed ? 48 : 220) + 'px' }">
          <a-space><a-button @click="$router.push('/users')">取消</a-button><a-button @click="load">重置</a-button>
            <a-button type="primary" :loading="saving" @click="save">保存并下发</a-button></a-space>
        </div>
      </a-form>
    </a-spin>
  </div>
</template>

<script setup>
import { inject, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import { api, fmtBytes } from '../api'
import AllocEditor from '../components/AllocEditor.vue'
import VMPicker from '../components/VMPicker.vue'

const props = defineProps({ id: String })
const router = useRouter()
const collapsed = inject('collapsed')
const f = ref(null)
const used = ref(0)
const usedPorts = ref({})
const nodes = ref([])
const vms = ref([])
const loading = ref(false)
const saving = ref(false)

async function loadVMs() { vms.value = await api.get('/api/vms') }
async function load() {
  loading.value = true
  try {
    const [u, n, list] = await Promise.all([api.get(`/api/users/${props.id}`), api.get('/api/nodes'), api.get('/api/users'), loadVMs()])
    nodes.value = n.nodes.filter(x => x.initialized)
    usedPorts.value = list.find(x => x.id === u.id)?.usedPorts || {}
    used.value = u.usedBytes
    f.value = {
      username: u.username, password: '', enabled: u.enabled, quotaGb: u.quotaBytes ? +(u.quotaBytes / 2 ** 30).toFixed(2) : undefined,
      maxRules: u.maxRules, remark: u.remark, allocations: u.allocations.map(a => ({ ...a, _k: a.id })), vmIds: u.vmIds,
    }
  } finally { loading.value = false }
}
onMounted(load)

async function save() {
  saving.value = true
  try {
    await api.put(`/api/users/${props.id}`, { ...f.value, quotaGb: f.value.quotaGb || 0 })
    Message.success('已保存，正在下发到相关节点')
    router.push('/users')
  } finally { saving.value = false }
}
</script>
