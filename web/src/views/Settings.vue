<template>
  <div class="container with-actions" style="padding-top:16px">
    <a-spin :loading="!st" style="width:100%">
      <a-form v-if="st" :model="st" layout="vertical">
        <a-space direction="vertical" :size="16" fill>
          <UpdateCard :st="st" />
          <a-card class="general-card"><template #title>控制端</template>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="站点名称"><a-input v-model="st.site" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="访问地址" extra="浏览器与节点 Agent 访问控制端的地址（不含端口）；未填写时自动记录管理员首次访问的地址"><a-input v-model="st.url" placeholder="https://域名或IP" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item :label="listen ? '访问端口' : '面板端口'" :extra="listen ? `节点 Agent 连接用的外部端口；程序固定监听 ${listen}，修改映射请改 docker compose 中 ports 左侧` : '修改后服务会重新监听'"><a-input-number v-model="st.port" :min="1" :max="65535" /></a-form-item></a-col>
            </a-row>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="HTTPS 证书">
                <a-select v-model="st.tls" :options="[{ label: '自签名', value: 'self' }, { label: `Let's Encrypt`, value: 'acme' }, { label: '由反向代理处理', value: 'proxy' }]" /></a-form-item></a-col>
              <a-col v-if="pin" :xs="24" :md="16"><a-form-item label="证书公钥固定值（Agent 校验用）"><a-input :model-value="pin" readonly class="mono" /></a-form-item></a-col>
            </a-row>
          </a-card>
          <a-card class="general-card"><template #title>节点与统计</template>
            <a-row :gutter="80">
              <a-col :xs="24" :md="6"><a-form-item label="采集间隔" extra="流量统计、虚拟机同步、健康检查"><a-input-number v-model="st.interval" :min="10" :max="3600"><template #append>秒</template></a-input-number></a-form-item></a-col>
              <a-col :xs="24" :md="6"><a-form-item label="离线判定阈值" extra="采集连续失败超过该时长才显示离线，避免瞬时超时误判"><a-input-number v-model="st.offlineAfter" :min="30" :max="3600"><template #append>秒</template></a-input-number></a-form-item></a-col>
              <a-col :xs="24" :md="6"><a-form-item label="新节点默认可分配端口" extra="添加节点时的默认值，可在每个节点中单独修改"><a-input v-model="st.defaultPortRange" placeholder="10000-60000" /></a-form-item></a-col>
              <a-col :xs="24" :md="6"><a-form-item label="新节点默认保留端口" extra="支持单个与范围，如 22,8006,3000-3100"><a-input v-model="st.defaultReservedPorts" placeholder="22,8006" allow-clear /></a-form-item></a-col>
            </a-row>
            <a-row :gutter="80">
              <a-col :xs="12" :md="6"><a-form-item label="磁盘繁忙 %util"><a-input-number v-model="st.diskBusyUtil" :min="1" :max="100"><template #append>%</template></a-input-number></a-form-item></a-col>
              <a-col :xs="12" :md="6"><a-form-item label="磁盘饱和 %util"><a-input-number v-model="st.diskSatUtil" :min="1" :max="100"><template #append>%</template></a-input-number></a-form-item></a-col>
              <a-col :xs="12" :md="6"><a-form-item label="繁忙延迟"><a-input-number v-model="st.diskBusyAwait" :min="1"><template #append>ms</template></a-input-number></a-form-item></a-col>
              <a-col :xs="12" :md="6"><a-form-item label="饱和延迟"><a-input-number v-model="st.diskSatAwait" :min="1"><template #append>ms</template></a-input-number></a-form-item></a-col>
            </a-row>
          </a-card>
          <a-card class="general-card"><template #title>公开监控页</template>
            <template #extra><a-space><a-tag :color="pub.enabled ? 'green' : 'gray'">{{ pub.enabled ? '已开启' : '未开启' }}</a-tag>
              <a-button size="small" :disabled="!pub.enabled || !pub.slug" @click="previewPub"><template #icon><icon-eye /></template>预览</a-button></a-space></template>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="允许外部访问" extra="开启后无需登录即可查看监控页"><a-switch v-model="pub.enabled" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="访问地址">
                <a-input :model-value="pubUrl" readonly><template #append><a-link @click="copyText(pubUrl)"><icon-copy /></a-link></template></a-input>
                <template #extra>路径后缀：<a-input v-model="pub.slug" size="mini" style="width:130px" /> <a-link @click="pub.slug = randomSlug()">随机生成</a-link></template></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="访问控制">
                <a-radio-group v-model="pub.access"><a-radio value="open">完全公开</a-radio><a-radio value="password">访问密码</a-radio></a-radio-group>
                <a-input-password v-if="pub.access === 'password'" v-model="pubPwd" :placeholder="hasPwd ? '已设置，留空则不修改' : '访问密码'" style="margin-top:8px" /></a-form-item></a-col>
            </a-row>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="页面标题"><a-input v-model="pub.title" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="页面说明"><a-input v-model="pub.desc" placeholder="选填" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="刷新间隔"><a-input-number v-model="pub.refresh" :min="5" :max="3600"><template #append>秒</template></a-input-number></a-form-item></a-col>
            </a-row>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="展示节点"><a-select v-model="pub.nodes" multiple :options="nodes.map(n => ({ label: n.name, value: n.id }))" placeholder="选择要公开的节点" /></a-form-item></a-col>
              <a-col :xs="24" :md="16"><a-form-item label="展示内容" extra="外部访问页永不显示 MAC、转发规则、端口等管理信息">
                <a-space wrap :size="20">
                  <a-checkbox v-model="pub.showNodes">节点概况</a-checkbox>
                  <a-checkbox v-model="pub.showVms">虚拟机列表</a-checkbox>
                  <a-checkbox v-model="pub.showDetail" :disabled="!pub.showVms">虚拟机历史曲线</a-checkbox>
                  <a-checkbox v-model="pub.showIp" :disabled="!pub.showVms">内网 IP</a-checkbox>
                  <a-checkbox v-model="pub.showOwner" :disabled="!pub.showVms">归属用户</a-checkbox>
                  <a-checkbox v-model="pub.onlyShared" :disabled="!pub.showVms">只显示公共虚拟机</a-checkbox>
                </a-space></a-form-item></a-col>
            </a-row>
          </a-card>
          <a-card class="general-card"><template #title>安全</template>
            <a-row :gutter="80">
              <a-col :xs="24" :md="8"><a-form-item label="会话有效期"><a-input-number v-model="st.sessionDays" :min="1" :max="365"><template #append>天</template></a-input-number></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="登录失败锁定" extra="15 分钟内失败次数达到后锁定 15 分钟，0 为不锁定"><a-input-number v-model="st.lockAttempts" :min="0" :max="100"><template #append>次</template></a-input-number></a-form-item></a-col>
            </a-row>
          </a-card>
        </a-space>
        <div class="actions" :style="{ left: (collapsed ? 48 : 220) + 'px' }">
          <a-space><a-button @click="load">重置</a-button><a-button type="primary" :loading="saving" @click="save">保存</a-button></a-space>
        </div>
      </a-form>
    </a-spin>
  </div>
</template>

<script setup>
import { computed, inject, onMounted, ref } from 'vue'
import { Message, Modal } from '@arco-design/web-vue'
import { api, copyText, loadSession } from '../api'
import UpdateCard from '../components/UpdateCard.vue'

const collapsed = inject('collapsed')
const st = ref(null)
const pub = ref({})
const pubPwd = ref('')
const hasPwd = ref(false)
const pin = ref('')
const listen = ref('')
const nodes = ref([])
const saving = ref(false)
const randomSlug = () => Math.random().toString(36).slice(2, 10) + Math.random().toString(36).slice(2, 6)
const pubUrl = computed(() => `${location.origin}/status/${pub.value.slug || ''}`)

async function load() {
  const [d, n] = await Promise.all([api.get('/api/settings'), api.get('/api/nodes')])
  st.value = d.settings
  pub.value = { ...d.public, slug: d.public.slug || randomSlug() }
  hasPwd.value = d.publicHasPassword
  pin.value = d.pin
  listen.value = d.listen || ''
  nodes.value = n.nodes.filter(x => x.initialized)
  pubPwd.value = ''
}
onMounted(load)

async function save() {
  saving.value = true
  try {
    const r = await api.put('/api/settings', { settings: st.value, public: pub.value, publicPwd: pubPwd.value })
    await loadSession()
    if (r.restart) Modal.info({ title: '服务正在重新启动', content: `访问地址或证书已变更，请稍后访问 ${st.value.url}:${st.value.port}` })
    else Message.success('已保存')
    load()
  } finally { saving.value = false }
}
function previewPub() { window.open(`#/status/${pub.value.slug}`, '_blank') }
</script>
