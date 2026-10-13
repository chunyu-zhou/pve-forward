<template>
  <div class="login">
    <div class="logo"><span class="logo-mark"><icon-swap /></span>{{ session.site }}</div>
    <div class="banner"><div class="banner-inner">
      <h2>多节点 PVE 端口转发管理</h2>
      <p>控制端统一管理用户、端口与规则，各 PVE 节点通过 Agent 或 SSH 接入，以 nftables DNAT 执行转发。</p>
      <div class="flow">客户端 → 节点公网 IP:端口<br>&nbsp;&nbsp;→ nftables DNAT<br>&nbsp;&nbsp;→ 节点内网虚拟机:端口</div>
    </div></div>
    <div class="content">
      <div class="login-form-wrapper">
        <div class="login-form-title">登录 {{ session.site }}</div>
        <div class="login-form-sub-title">使用管理员分配的账号登录</div>
        <div class="login-form-error-msg">{{ error }}</div>
        <a-form :model="f" layout="vertical" @submit="submit">
          <template v-if="!need2fa">
            <a-form-item field="username" hide-label>
              <a-input v-model="f.username" placeholder="用户名" autocomplete="username"><template #prefix><icon-user /></template></a-input></a-form-item>
            <a-form-item field="password" hide-label>
              <a-input-password v-model="f.password" placeholder="密码" allow-clear autocomplete="current-password"><template #prefix><icon-lock /></template></a-input-password></a-form-item>
          </template>
          <template v-else>
            <div class="muted" style="margin-bottom:8px">已启用两步验证，请输入认证应用（如 Google Authenticator）中 {{ f.username }} 的 6 位验证码</div>
            <a-form-item field="code" hide-label>
              <a-input ref="codeInput" v-model="f.code" placeholder="6 位验证码" :max-length="6" autocomplete="one-time-code" inputmode="numeric"
                       @input="v => { f.code = v; if (v.replace(/\D/g, '').length === 6) submit() }"><template #prefix><icon-safe /></template></a-input></a-form-item>
            <a-link style="margin-bottom:12px" @click="back">返回重新输入账号</a-link>
          </template>
          <a-space :size="16" direction="vertical" fill>
            <span class="muted">忘记密码请联系管理员</span>
            <a-button type="primary" html-type="submit" long :loading="busy">{{ need2fa ? '验证' : '登录' }}</a-button>
          </a-space>
        </a-form>
      </div>
    </div>
  </div>
</template>

<script setup>
import { nextTick, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, session } from '../api'

const router = useRouter()
const route = useRoute()
const f = reactive({ username: '', password: '', code: '' })
const error = ref('')
const busy = ref(false)
const need2fa = ref(false)
const codeInput = ref(null)

function back() { need2fa.value = false; f.code = ''; error.value = '' }

async function submit() {
  if (busy.value) return
  error.value = ''
  if (!f.username || !f.password) { error.value = '请输入用户名和密码'; return }
  if (need2fa.value && f.code.replace(/\D/g, '').length !== 6) { error.value = '请输入 6 位验证码'; return }
  busy.value = true
  try {
    const r = await api.post('/api/login', { username: f.username, password: f.password, code: f.code.replace(/\D/g, '') }, { silent: true, allow401: true })
    if (r.need2fa) {
      need2fa.value = true
      await nextTick()
      codeInput.value?.focus()
      return
    }
    session.user = r.user
    f.password = ''
    f.code = ''
    router.push(route.query.redirect || '/dashboard')
  } catch (e) {
    error.value = e.message
    if (need2fa.value) f.code = ''
  } finally { busy.value = false }
}
</script>
