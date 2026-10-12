<template>
  <div class="container with-actions" style="padding-top:16px">
    <a-spin :loading="!n" style="width:100%">
      <a-form v-if="n" :model="f" layout="vertical">
        <a-space direction="vertical" :size="16" fill>
          <a-card class="general-card"><template #title>节点信息</template>
            <template #extra><a-badge :status="nodeBadge[n.status].badge" :text="`${nodeBadge[n.status].text} · 心跳 ${fmtAgo(n.lastSeen)}`" /></template>
            <a-alert v-if="n.error && n.status !== 'online'" type="error" style="margin-bottom:16px">{{ n.error }}</a-alert>
            <a-alert v-if="n.applyError" type="error" style="margin-bottom:16px">规则下发失败：{{ n.applyError }}</a-alert>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="节点名称"><a-input v-model="f.name" :max-length="40" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="备注"><a-input v-model="f.remark" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="主机"><a-input :model-value="`${n.hostname || '-'} · PVE ${n.pveVersion || '-'}`" disabled /></a-form-item></a-col>
            </a-row>
          </a-card>
          <a-card class="general-card"><template #title>端口</template>
            <template #extra><span class="muted">已分配给用户的端口必须仍在可分配范围内且不是保留端口，否则无法保存</span></template>
            <a-row :gutter="80">
              <a-col :xs="24" :md="12"><a-form-item label="可分配端口" required :validate-status="portHint.error ? 'error' : undefined"
                :help="portHint.error || `只有此范围内的端口能分配给用户，当前可分配 ${portHint.count} 个；支持多段，如 10000-20000,30000-40000`">
                <a-input v-model="f.portRange" placeholder="10000-60000" /></a-form-item></a-col>
              <a-col :xs="24" :md="12"><a-form-item label="保留端口" extra="不会分配给用户，支持单个与范围，如 22,8006,3000-3100">
                <a-input v-model="f.reservedPorts" placeholder="22,8006" allow-clear /></a-form-item></a-col>
            </a-row>
          </a-card>
          <a-card class="general-card"><template #title>连接方式</template>
            <a-alert v-if="agentWhy" type="warning" title="无法改用 Agent 方式" style="margin-bottom:16px">{{ agentWhy }}</a-alert>
            <a-form-item label="方式"><a-radio-group v-model="f.mode" type="button"><a-radio value="agent" :disabled="!!agentWhy">Agent（WSS）</a-radio><a-radio value="ssh">SSH</a-radio></a-radio-group>
              <template #extra>{{ f.mode !== n.mode ? (f.mode === 'agent' ? '保存后请在节点上执行安装命令' : '保存后需测试连接并确认主机指纹') : '' }}</template></a-form-item>
            <a-form v-if="f.mode === 'ssh'" :model="f" :label-col-props="{ span: 4 }" :wrapper-col-props="{ span: 14 }" style="max-width:860px">
              <SSHFields :f="f" :err="{}" :creds="creds" edit @new-cred="$router.push('/credentials')" />
            </a-form>
            <a-input v-else :model-value="`Agent v${n.agentVersion || '-'}`" disabled style="max-width:300px" />
          </a-card>
          <a-card class="general-card"><template #title>公网</template>
            <template #extra><a-button size="small" @click="$router.push(`/nodes/${id}/setup`)">重新配置网络</a-button></template>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="公网网口"><a-input :model-value="n.net.wanIface" disabled /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="公网 IPv4" extra="仅用于展示规则入口"><a-input v-model="f.wanIp4" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="公网 IPv6"><a-input v-model="f.wanIp6" :disabled="!n.net.cidr6" :placeholder="n.net.cidr6 ? '' : '未启用 IPv6'" /></a-form-item></a-col>
            </a-row>
          </a-card>
          <a-card class="general-card"><template #title>内网与 DHCP</template>
            <a-descriptions :column="{ xs: 1, md: 3 }" :data="[
              { label: '网桥', value: n.net.bridge }, { label: 'IPv4 网段', value: n.net.cidr4 }, { label: '网关', value: n.net.gw4 },
              { label: 'IPv6 网段', value: n.net.cidr6 || '未启用' }, { label: 'DHCP', value: { managed: 'dnsmasq（本系统管理）', external: '外部 DHCP', none: '无' }[n.net.dhcp] } ]" />
            <a-form-item v-if="n.net.dhcp === 'managed'" label="DNS" style="margin-top:16px;max-width:400px"><a-input-tag v-model="f.dns" /></a-form-item>
          </a-card>
        </a-space>
        <div class="actions" :style="{ left: (collapsed ? 48 : 220) + 'px' }">
          <a-space><a-button @click="$router.push('/nodes')">取消</a-button><a-button type="primary" :loading="saving" @click="save">保存</a-button></a-space>
        </div>
      </a-form>
    </a-spin>
  </div>
</template>

<script setup>
import { computed, inject, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import { api, nodeBadge, fmtAgo, portPolicyHint } from '../api'
import SSHFields from '../components/SSHFields.vue'

const props = defineProps({ id: String })
const router = useRouter()
const collapsed = inject('collapsed')
const n = ref(null)
const creds = ref([])
const saving = ref(false)
const f = reactive({})
const portHint = computed(() => portPolicyHint(f.portRange, f.reservedPorts))
// 已是 Agent 方式的节点不受影响；SSH 节点在访问地址不是 HTTPS 时不能改为 Agent
const agentWhy = ref('')

onMounted(async () => {
  const [d, c] = await Promise.all([api.get(`/api/nodes/${props.id}`), api.get('/api/credentials')])
  n.value = d.node
  creds.value = c
  agentWhy.value = d.node.mode === 'agent' ? '' : d.agentUnavailable || ''
  const x = d.node
  Object.assign(f, { name: x.name, remark: x.remark, mode: x.mode, sshHost: x.sshHost, sshPort: x.sshPort || 22, sshAuth: x.sshAuth || 'password', sshUser: x.sshUser || 'root',
    sshPassword: '', sshKey: '', sshPassphrase: '', credId: x.credId || undefined, wanIp4: x.net.wanIp4, wanIp6: x.net.wanIp6,
    portRange: x.ports.range, reservedPorts: x.ports.reserved, dns: x.net.dns || [] })
})

async function save() {
  if (portHint.value.error) { Message.warning(portHint.value.error); return }
  saving.value = true
  try {
    const modeChanged = f.mode !== n.value.mode
    await api.put(`/api/nodes/${props.id}`, { ...f })
    Message.success('已保存')
    router.push(modeChanged ? `/nodes/${props.id}/setup` : '/nodes')
  } finally { saving.value = false }
}
</script>
