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
          <a-card class="general-card"><template #title>连接方式</template>
            <a-form-item label="方式"><a-radio-group v-model="f.mode" type="button"><a-radio value="agent">Agent（WSS）</a-radio><a-radio value="ssh">SSH</a-radio></a-radio-group>
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
            <a-row :gutter="80"><a-col :xs="24" :md="8"><a-form-item label="保留端口" extra="不可分配给用户"><a-input-tag v-model="f.reserved" /></a-form-item></a-col></a-row>
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
import { inject, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import { api, nodeBadge, fmtAgo } from '../api'
import SSHFields from '../components/SSHFields.vue'

const props = defineProps({ id: String })
const router = useRouter()
const collapsed = inject('collapsed')
const n = ref(null)
const creds = ref([])
const saving = ref(false)
const f = reactive({})

onMounted(async () => {
  const [d, c] = await Promise.all([api.get(`/api/nodes/${props.id}`), api.get('/api/credentials')])
  n.value = d.node
  creds.value = c
  const x = d.node
  Object.assign(f, { name: x.name, remark: x.remark, mode: x.mode, sshHost: x.sshHost, sshPort: x.sshPort || 22, sshAuth: x.sshAuth || 'password', sshUser: x.sshUser || 'root',
    sshPassword: '', sshKey: '', sshPassphrase: '', credId: x.credId || undefined, wanIp4: x.net.wanIp4, wanIp6: x.net.wanIp6,
    reserved: (x.net.reserved || []).map(String), dns: x.net.dns || [] })
})

async function save() {
  saving.value = true
  try {
    const modeChanged = f.mode !== n.value.mode
    await api.put(`/api/nodes/${props.id}`, { ...f, reserved: f.reserved.map(Number).filter(p => p > 0 && p < 65536) })
    Message.success('已保存')
    router.push(modeChanged ? `/nodes/${props.id}/setup` : '/nodes')
  } finally { saving.value = false }
}
</script>
