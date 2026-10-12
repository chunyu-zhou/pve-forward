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
          <a-form-item field="username" hide-label>
            <a-input v-model="f.username" placeholder="用户名"><template #prefix><icon-user /></template></a-input></a-form-item>
          <a-form-item field="password" hide-label>
            <a-input-password v-model="f.password" placeholder="密码" allow-clear><template #prefix><icon-lock /></template></a-input-password></a-form-item>
          <a-space :size="16" direction="vertical" fill>
            <span class="muted">忘记密码请联系管理员</span>
            <a-button type="primary" html-type="submit" long :loading="busy">登录</a-button>
          </a-space>
        </a-form>
      </div>
    </div>
  </div>
</template>

<script setup>
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, session } from '../api'

const router = useRouter()
const route = useRoute()
const f = reactive({ username: '', password: '' })
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  if (!f.username || !f.password) { error.value = '请输入用户名和密码'; return }
  busy.value = true
  try {
    const r = await api.post('/api/login', f, { silent: true, allow401: true })
    session.user = r.user
    router.push(route.query.redirect || '/dashboard')
  } catch (e) { error.value = e.message } finally { busy.value = false }
}
</script>
