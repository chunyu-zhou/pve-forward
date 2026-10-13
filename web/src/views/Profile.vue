<template>
  <div class="container" style="padding-top:16px">
    <a-space direction="vertical" :size="16" fill>
      <a-card class="general-card" title="基本信息">
        <a-descriptions v-if="info" :column="{ xs: 1, md: 3 }" :data="[
          { label: '用户名', value: u.username }, { label: '角色', value: u.role === 'admin' ? '管理员' : '普通用户' },
          { label: '状态', value: !u.enabled ? '已禁用' : u.quotaBytes && u.usedBytes >= u.quotaBytes ? '流量超限' : '正常' },
          { label: '流量', value: `${fmtBytes(u.usedBytes)} / ${u.quotaBytes ? fmtBytes(u.quotaBytes) : '不限'}` },
          { label: '规则', value: `${info.rules} 条` },
          { label: '创建时间', value: fmtTime(u.createdAt) } ]" />
      </a-card>

      <a-card class="general-card" title="修改密码">
        <!-- autocomplete：避免浏览器把保存的登录密码自动填入「当前密码」以外的框 -->
        <a-form ref="pwForm" :model="f" style="width:440px;max-width:100%" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }" autocomplete="off">
          <input type="text" name="username" :value="u.username" autocomplete="username" hidden />
          <a-form-item label="当前密码"><a-input-password v-model="f.old" v-model:visibility="show.old" @visibility-change="syncPasswordFields" autocomplete="current-password" /></a-form-item>
          <a-form-item label="新密码"><a-input-password v-model="f.new" v-model:visibility="show.new" @visibility-change="syncPasswordFields" placeholder="至少 8 位" autocomplete="new-password" /></a-form-item>
          <a-form-item label="确认新密码"><a-input-password v-model="f.new2" v-model:visibility="show.new2" @visibility-change="syncPasswordFields" autocomplete="new-password" /></a-form-item>
          <a-form-item><a-button type="primary" :loading="busy" @click="change">保存</a-button></a-form-item>
        </a-form>
      </a-card>

      <a-card class="general-card" title="两步验证">
        <template #extra><a-tag v-if="u.twoFa" color="green"><icon-safe /> 已启用</a-tag><a-tag v-else>未启用</a-tag></template>
        <div class="muted" style="margin-bottom:16px">启用后，登录时除密码外还需输入认证应用（Google Authenticator、Microsoft Authenticator、1Password 等）中的 6 位动态验证码。每个账号单独绑定。</div>
        <!-- 已启用：解除 -->
        <a-form v-if="u.twoFa" :model="d" style="width:440px;max-width:100%" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }">
          <a-form-item label="当前密码"><a-input-password v-model="d.password" autocomplete="current-password" /></a-form-item>
          <a-form-item label="验证码"><a-input v-model="d.code" :max-length="6" placeholder="认证应用中的 6 位验证码" autocomplete="one-time-code" inputmode="numeric" /></a-form-item>
          <a-form-item><a-popconfirm content="解除后登录只需密码，确定解除？" type="warning" @ok="disable"><a-button status="danger" :loading="busy2">解除两步验证</a-button></a-popconfirm></a-form-item>
        </a-form>
        <!-- 未启用：第 1 步验证密码生成二维码，第 2 步扫码并输入验证码 -->
        <a-form v-else-if="!setup" :model="d" style="width:440px;max-width:100%" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }">
          <a-form-item label="当前密码"><a-input-password v-model="d.password" autocomplete="current-password" placeholder="验证身份后生成二维码" /></a-form-item>
          <a-form-item><a-button type="primary" :loading="busy2" @click="start">开始绑定</a-button></a-form-item>
        </a-form>
        <div v-else class="tfa-setup">
          <img :src="setup.qr" alt="二维码" class="tfa-qr" />
          <div style="flex:1;min-width:260px">
            <ol class="tfa-steps">
              <li>用认证应用扫描左侧二维码；无法扫码时手动输入密钥：
                <div class="mono tfa-secret">{{ setup.secret.match(/.{1,4}/g).join(' ') }} <a-link @click="copyText(setup.secret)">复制</a-link></div></li>
              <li>输入应用中显示的 6 位验证码完成绑定。</li>
            </ol>
            <a-space>
              <a-input v-model="d.code" :max-length="6" placeholder="6 位验证码" style="width:160px" autocomplete="one-time-code" inputmode="numeric" @press-enter="enable" />
              <a-button type="primary" :loading="busy2" @click="enable">完成绑定</a-button>
              <a-button @click="setup = null; d.code = ''">取消</a-button>
            </a-space>
          </div>
        </div>
      </a-card>
    </a-space>
  </div>
</template>

<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api, copyText, fmtBytes, fmtTime, session } from '../api'

const info = ref(null)
const u = computed(() => info.value?.user || {})
const f = reactive({ old: '', new: '', new2: '' })
const show = reactive({ old: true, new: true, new2: true })
const pwForm = ref(null)
const busy = ref(false)
const d = reactive({ password: '', code: '' })
const setup = ref(null)
const busy2 = ref(false)
async function load() { info.value = await api.get('/api/me'); if (session.user) session.user.twoFa = info.value.user.twoFa }
onMounted(load)

// 浏览器自动填充的密码可能没有触发输入事件：保存时以输入框中的实际内容为准
function fieldValues() {
  const ins = pwForm.value?.$el?.querySelectorAll('.arco-input-wrapper input') || []
  return { old: ins[0]?.value ?? f.old, new: ins[1]?.value ?? f.new, new2: ins[2]?.value ?? f.new2 }
}
function syncPasswordFields() {
  const v = fieldValues()
  Object.assign(f, { old: v.old, new: v.new, new2: v.new2 })
  return v
}
async function change() {
  if (busy.value) return
  const v = syncPasswordFields()
  if (!v.old) { Message.warning('请输入当前密码'); return }
  const bytes = new TextEncoder().encode(v.new).length
  if (bytes < 8 || bytes > 72) { Message.warning('新密码须为 8–72 字节（中文等字符占多个字节）'); return }
  if (v.new !== v.new2) { Message.warning('两次输入的新密码不一致'); return }
  busy.value = true
  try {
    await api.put('/api/me/password', { old: v.old, new: v.new })
    Message.success('密码已修改，其他设备上的登录已失效')
    Object.assign(f, { old: '', new: '', new2: '' })
    Object.assign(show, { old: true, new: true, new2: true })
    setup.value = null
  } finally { busy.value = false }
}

async function start() {
  if (!d.password) { Message.warning('请输入当前密码'); return }
  busy2.value = true
  try {
    setup.value = await api.post('/api/me/2fa/setup', { password: d.password })
    d.password = ''
  } finally { busy2.value = false }
}
async function enable() {
  if (d.code.replace(/\D/g, '').length !== 6) { Message.warning('请输入 6 位验证码'); return }
  busy2.value = true
  try {
    await api.post('/api/me/2fa/enable', { code: d.code.replace(/\D/g, '') })
    Message.success('已启用两步验证，下次登录需要输入验证码')
    setup.value = null
    Object.assign(d, { password: '', code: '' })
    await load()
  } finally { busy2.value = false }
}
async function disable() {
  busy2.value = true
  try {
    await api.post('/api/me/2fa/disable', { password: d.password, code: d.code.replace(/\D/g, '') })
    Message.success('已解除两步验证')
    Object.assign(d, { password: '', code: '' })
    await load()
  } finally { busy2.value = false }
}
</script>

<style scoped>
.tfa-setup { display: flex; gap: 24px; align-items: flex-start; flex-wrap: wrap; }
.tfa-qr { width: 180px; height: 180px; border: 1px solid var(--color-border-2); border-radius: 4px; image-rendering: pixelated; }
.tfa-steps { margin: 0 0 16px; padding-left: 18px; line-height: 2; }
.tfa-secret { margin: 4px 0; padding: 6px 10px; background: var(--color-fill-2); border-radius: 4px; display: inline-block; font-size: 13px; letter-spacing: 1px; }
</style>
