<template>
  <a-modal :visible="visible" :title="rule?.id ? '编辑规则' : '添加规则'" :width="600" :ok-loading="saving" @ok="save" @cancel="$emit('update:visible', false)" unmount-on-close>
    <a-form :model="f" :label-col-props="{ span: 5 }" :wrapper-col-props="{ span: 19 }">
      <a-form-item label="所属用户" required :extra="admin ? (isPublic ? '公共规则只有管理员可以管理，可使用保留端口，可指向节点上的任意虚拟机' : undefined) : '规则只能创建在自己名下'">
        <a-select v-model="f.userId" :disabled="!admin" placeholder="请选择用户" @change="onUserChange">
          <a-option v-if="admin" :value="session.user.id">公共（管理员）</a-option>
          <a-option v-for="u in userList" :key="u.id" :value="u.id">{{ u.username }}</a-option></a-select>
      </a-form-item>
      <a-form-item label="节点" required :extra="isPublic ? undefined : opts.nodes.length ? '仅列出该用户分配了端口的节点' : '该用户还没有被分配任何节点的端口'">
        <a-select v-model="f.nodeId" placeholder="请选择节点" @change="onNodeChange">
          <a-option v-for="n in opts.nodes" :key="n.id" :value="n.id">
            {{ n.name }}<span class="muted">（{{ n.wanIp4 }}{{ n.status !== 'online' ? '，离线' : '' }}）</span></a-option>
        </a-select>
      </a-form-item>
      <a-form-item v-if="isPublic" label="监听端口" required extra="公共规则可使用任意未被规则占用、未分配给用户的端口（包括保留端口）">
        <a-input-number v-model="f.port" :min="1" :max="65535" placeholder="1 - 65535" :disabled="!f.nodeId" />
      </a-form-item>
      <a-form-item v-else label="监听端口" required
        :extra="f.nodeId ? portExtra : '请先选择节点'">
        <a-select v-model="f.port" placeholder="请选择或输入端口搜索" :disabled="!f.nodeId" allow-search :options="portOptions"
                  :virtual-list-props="portOptions.length > 200 ? { height: 240 } : undefined">
          <template #empty><div style="padding:12px;text-align:center" class="muted">{{ emptyReason }}</div></template>
        </a-select>
      </a-form-item>
      <a-form-item label="协议" required>
        <a-radio-group v-model="f.proto"><a-radio value="tcp">TCP</a-radio><a-radio value="udp">UDP</a-radio><a-radio value="both">TCP + UDP</a-radio></a-radio-group>
      </a-form-item>
      <a-form-item label="目标虚拟机" required :extra="isPublic ? '节点上的全部虚拟机' : admin ? '只能选择该用户在此节点上的虚拟机或公共虚拟机' : '只能选择自己在此节点上的虚拟机或公共虚拟机'">
        <a-select v-model="f.vmId" placeholder="请选择虚拟机" :disabled="!f.nodeId" allow-search @change="onVMChange">
          <a-optgroup :label="isPublic ? '虚拟机' : admin ? '该用户的虚拟机' : '我的虚拟机'">
            <a-option v-for="v in opts.vms.filter(v => !v.shared)" :key="v.id" :value="v.id" :label="vmLabel(v)" :disabled="!v.ipv4 && !v.ipv6">
              <div class="ip-opt"><span><span class="mono">{{ v.vmid }}</span> {{ v.name }}<span v-if="v.status !== 'running'" class="muted">（{{ v.status === 'stopped' ? '已停止' : v.status }}）</span>
                <a-tag v-if="v.ipv4 && !v.ipv6" size="small" style="margin-left:4px">仅 IPv4</a-tag></span><span class="mono muted">{{ ipLabel(v) }}</span></div></a-option>
          </a-optgroup>
          <a-optgroup label="公共虚拟机">
            <a-option v-for="v in opts.vms.filter(v => v.shared)" :key="v.id" :value="v.id" :label="vmLabel(v)">
              <div class="ip-opt"><span><span class="mono">{{ v.vmid }}</span> {{ v.name }}
                <a-tag v-if="v.ipv4 && !v.ipv6" size="small" style="margin-left:4px">仅 IPv4</a-tag></span><span class="mono muted">{{ ipLabel(v) }}</span></div></a-option>
          </a-optgroup>
        </a-select>
      </a-form-item>
      <a-form-item label="IP 版本" required :extra="ipverHint">
        <a-radio-group v-model="f.ipVer">
          <a-radio value="v4" :disabled="!!vm && !vm.ipv4">IPv4</a-radio>
          <a-radio value="v6" :disabled="!v6Allowed">IPv6</a-radio>
          <a-radio value="both" :disabled="!v6Allowed || (!!vm && !vm.ipv4)">IPv4 + IPv6</a-radio></a-radio-group>
      </a-form-item>
      <a-form-item label="目标端口" required><a-input-number v-model="f.targetPort" :min="1" :max="65535" placeholder="1 - 65535" /></a-form-item>
      <a-form-item label="备注"><a-input v-model="f.remark" :max-length="40" show-word-limit placeholder="选填" /></a-form-item>
    </a-form>
  </a-modal>
</template>

<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api, isAdmin, session } from '../api'

const props = defineProps({ visible: Boolean, rule: Object, userId: Number })
const emit = defineEmits(['update:visible', 'saved'])
const admin = computed(isAdmin)
const users = ref([])
const userList = computed(() => (admin.value ? users.value : [session.user]))
// 公共规则：所属用户为管理员自己
const isPublic = computed(() => admin.value && f.userId === session.user?.id)
const f = reactive({})
const opts = reactive({ nodes: [], ports: [], vms: [], total: 0, usedCount: 0, busyCount: 0, reservedCount: 0 })
// 端口较多时必须通过 options 传入，虚拟列表才能正常渲染
const portOptions = computed(() => opts.ports.map(p => ({ label: String(p), value: p })))
const portExtra = computed(() => {
  const ex = [opts.usedCount && `规则已使用 ${opts.usedCount} 个`, opts.busyCount && `宿主机占用 ${opts.busyCount} 个`, opts.reservedCount && `保留 ${opts.reservedCount} 个`].filter(Boolean)
  return `只能从分配的 ${opts.total} 个端口中选择，当前可选 ${opts.ports.length} 个` + (ex.length ? `（已排除${ex.join('、')}）` : '') +
    (node.value && node.value.status !== 'online' ? '。节点当前离线，规则会在节点恢复后下发' : '')
})
const emptyReason = computed(() => (!opts.total ? '该节点上还没有分配端口，请联系管理员分配' : '已分配的端口都已被使用或占用，请联系管理员分配更多端口'))
const saving = ref(false)

const node = computed(() => opts.nodes.find(n => n.id === f.nodeId))
const vm = computed(() => opts.vms.find(v => v.id === f.vmId))
// 含 v6 的选项需要：节点启用了 IPv6，且所选虚拟机有 IPv6 地址
const v6Allowed = computed(() => !!node.value?.v6 && (!vm.value || !!vm.value.ipv6))
const ipverHint = computed(() => {
  if (!node.value) return undefined
  const p = f.port || '端口'
  const e = []
  if (f.ipVer !== 'v6') e.push(`${node.value.wanIp4}:${p}`)
  if (f.ipVer !== 'v4' && node.value.wanIp6) e.push(`[${node.value.wanIp6}]:${p}`)
  let s = '入口：' + e.join('  /  ')
  if (!node.value.v6) s += `（节点 ${node.value.name} 未启用 IPv6）`
  else if (vm.value && !vm.value.ipv6) s += '（所选虚拟机没有 IPv6 地址）'
  return s
})
const ipLabel = v => [v.ipv4, v.ipv6].filter(Boolean).join('  ·  ') || '未分配 IP'
const vmLabel = v => `${v.vmid} ${v.name}  ${ipLabel(v)}`

async function loadOpts(withNode = true) {
  if (!f.userId) return
  const q = `/api/rule-options?user=${f.userId}` + (withNode && f.nodeId ? `&node=${f.nodeId}&keep=${props.rule?.nodeId === f.nodeId ? props.rule.port : 0}` : '')
  const d = await api.get(q)
  opts.nodes = d.nodes || []
  opts.ports = d.ports || []
  opts.vms = d.vms || []
  opts.total = d.total || 0
  opts.usedCount = d.usedCount || 0
  opts.busyCount = d.busyCount || 0
  opts.reservedCount = d.reservedCount || 0
}

watch(() => props.visible, async v => {
  if (!v) return
  if (admin.value && !users.value.length) users.value = await api.get('/api/users')
  const r = props.rule
  Object.assign(f, r
    ? { userId: r.userId, nodeId: r.nodeId, port: r.port, proto: r.proto, ipVer: r.ipVer, vmId: r.vmId, targetPort: r.targetPort, remark: r.remark }
    : { userId: admin.value ? props.userId || users.value[0]?.id : session.user.id, nodeId: undefined, port: undefined, proto: 'tcp', ipVer: 'v4', vmId: undefined, targetPort: undefined, remark: '' })
  await loadOpts(false)
  if (!f.nodeId) f.nodeId = (opts.nodes.find(n => n.status === 'online') || opts.nodes[0])?.id
  if (f.nodeId) await loadOpts()
}, { immediate: true })

async function onUserChange() { f.nodeId = undefined; f.port = undefined; f.vmId = undefined; await loadOpts(false); f.nodeId = (opts.nodes.find(n => n.status === 'online') || opts.nodes[0])?.id; if (f.nodeId) await loadOpts() }
async function onNodeChange() { f.port = undefined; f.vmId = undefined; await loadOpts(); if (!node.value?.v6) f.ipVer = 'v4' }
function onVMChange() {
  if (!v6Allowed.value) f.ipVer = 'v4'
  else if (vm.value && !vm.value.ipv4) f.ipVer = 'v6'
}

async function save() {
  if (!f.userId || !f.nodeId || !f.port || !f.vmId || !f.targetPort) { Message.warning('请填写完整'); return false }
  saving.value = true
  try {
    if (props.rule?.id) await api.put(`/api/rules/${props.rule.id}`, f)
    else await api.post('/api/rules', f)
    Message.success('已保存，正在下发到节点')
    emit('update:visible', false)
    emit('saved')
  } catch { /* 已提示 */ } finally { saving.value = false }
}
</script>
