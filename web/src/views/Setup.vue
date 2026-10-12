<template>
  <div class="setup">
    <div class="setup-navbar">
      <a-space><div class="logo-mark"><icon-swap /></div>
        <a-typography-title :style="{ margin: 0, fontSize: '18px' }" :heading="5">PVE Forward</a-typography-title>
        <a-tag color="arcoblue" style="margin-left:8px">控制端初始化</a-tag></a-space>
      <span class="muted">控制端 v{{ session.version }}</span>
    </div>
    <div class="setup-body">
      <a-card class="general-card">
        <template #title>初始化控制端</template>
        <template #extra><span class="muted">PVE 节点在初始化完成后，于「PVE 节点 → 添加节点」中接入</span></template>
        <div class="step-wrapper" style="padding:40px 0 48px">
          <a-steps :current="step" style="width: 640px; max-width: 100%" class="steps">
            <a-step description="令牌与管理员账号">验证</a-step>
            <a-step description="访问地址与端口">控制端设置</a-step>
            <a-step description="初始化完成">完成</a-step>
          </a-steps>

          <a-form v-if="step === 1" :model="f" class="step-form" style="width:560px;max-width:100%" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }">
            <a-form-item label="初始化令牌" required :validate-status="err.token ? 'error' : undefined"
              :help="err.token || '安装控制端时终端已输出；也可执行 pve-forward setup-token 重新生成'">
              <a-input v-model="f.token" placeholder="例如 7KQ2-M9XD-4TPA" allow-clear><template #prefix><icon-safe /></template></a-input></a-form-item>
            <a-form-item label="管理员账号" required :validate-status="err.username ? 'error' : undefined" :help="err.username">
              <a-input v-model="f.username" /></a-form-item>
            <a-form-item label="密码" required :validate-status="err.password ? 'error' : undefined" :help="err.password">
              <a-input-password v-model="f.password" placeholder="至少 8 位" /></a-form-item>
            <a-form-item label="确认密码" required :validate-status="err.password2 ? 'error' : undefined" :help="err.password2">
              <a-input-password v-model="f.password2" placeholder="再次输入密码" @press-enter="next" /></a-form-item>
            <a-form-item><a-button type="primary" :loading="busy" @click="next">下一步</a-button></a-form-item>
          </a-form>

          <a-form v-else-if="step === 2" :model="f" class="step-form" style="width:600px;max-width:100%" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }">
            <a-form-item label="站点名称"><a-input v-model="f.site" /></a-form-item>
            <a-form-item label="控制端访问地址" required :validate-status="err.url ? 'error' : undefined"
              :help="err.url || 'Agent 通过此地址连接控制端，需能从各 PVE 节点访问；已按当前浏览器地址自动填写'">
              <a-input v-model="f.url" placeholder="https://panel.example.com" /></a-form-item>
            <a-form-item label="面板端口" extra="Web 与 Agent 连接共用此端口"><a-input-number v-model="f.port" :min="1" :max="65535" /></a-form-item>
            <a-form-item label="HTTPS 证书">
              <a-radio-group v-model="f.tls"><a-radio value="proxy">由反向代理处理（默认）</a-radio><a-radio value="self">自签名</a-radio><a-radio value="acme">Let's Encrypt</a-radio></a-radio-group>
              <template #extra>{{ tlsHint }}</template>
            </a-form-item>
            <a-form-item><a-space><a-button type="secondary" @click="step--">上一步</a-button><a-button type="primary" :loading="busy" @click="submit">完成初始化</a-button></a-space></a-form-item>
          </a-form>

          <div v-else class="success-wrap">
            <a-result status="success" title="控制端初始化完成" subtitle="下一步：在「PVE 节点 → 添加节点」中接入第一台 PVE" />
            <a-space :size="16">
              <a-button v-if="!result.restart" type="primary" @click="enter">进入控制台</a-button>
              <a-button v-else type="primary" :href="newAddress">前往新地址</a-button>
            </a-space>
            <a-alert v-if="result.restart" type="warning" style="max-width:640px;margin:20px auto 0;text-align:left">
              端口或证书设置已变更，服务正在重新启动，请访问 {{ newAddress }} 并重新登录。
            </a-alert>
            <div class="details-wrapper" style="max-width:640px">
              <a-descriptions :column="1" title="控制端信息" :data="[
                { label: '站点名称', value: f.site }, { label: '访问地址', value: newAddress },
                { label: 'Agent 连接地址', value: newAddress + '/agent/ws' }, { label: '管理员', value: f.username } ]" />
            </div>
          </div>
        </div>
      </a-card>
    </div>
  </div>
</template>

<script setup>
import { computed, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, loadSession, session } from '../api'

const router = useRouter()
const step = ref(1)
const busy = ref(false)
const err = reactive({})
const result = reactive({ restart: false })
const f = reactive({
  token: '', username: 'admin', password: '', password2: '', site: 'PVE Forward',
  url: `${location.protocol}//${location.hostname}`, port: Number(location.port) || (location.protocol === 'https:' ? 443 : 80), tls: 'proxy',
})
const tlsHint = computed(() => ({
  self: '自动生成证书；Agent 安装命令会附带证书公钥固定值以防中间人',
  acme: '需要使用域名，且本机 80 端口可从公网访问',
  proxy: '控制端以 HTTP 提供服务，请在前端反向代理（Nginx / Caddy）上配置 HTTPS 与 WebSocket',
}[f.tls]))
const newAddress = computed(() => `${f.tls === 'proxy' ? f.url.replace(/^https?:/, location.protocol) : f.url.replace(/^http:/, 'https:')}:${f.port}`)

const clear = () => Object.keys(err).forEach(k => delete err[k])

async function next() {
  clear()
  if (!/^[A-Za-z0-9_]{3,20}$/.test(f.username)) err.username = '需为 3-20 位字母、数字或下划线'
  if (f.password.length < 8) err.password = '密码至少 8 位'
  else if (f.password !== f.password2) err.password2 = '两次输入的密码不一致'
  if (!/^[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}$/i.test(f.token.trim())) err.token = '令牌格式应为 XXXX-XXXX-XXXX'
  if (Object.keys(err).length) return
  busy.value = true
  try {
    await api.post('/api/setup', { token: f.token, username: f.username, password: f.password, check: true }, { silent: true })
    step.value = 2
  } catch (e) { err.token = e.message } finally { busy.value = false }
}

async function submit() {
  clear()
  if (!/^https?:\/\/[^/:]+$/.test(f.url.trim())) { err.url = '格式应为 https://域名或IP，不含端口和路径'; return }
  busy.value = true
  try {
    const r = await api.post('/api/setup', { ...f, url: f.url.trim() }, { silent: true })
    result.restart = r.restart
    step.value = 3
  } catch (e) { err.url = e.message } finally { busy.value = false }
}

async function enter() {
  await loadSession()
  router.push('/dashboard')
}
</script>
