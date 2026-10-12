<template>
  <div class="container" style="padding-top:16px">
    <a-space direction="vertical" :size="16" fill>
      <a-card class="general-card" title="基本信息">
        <a-descriptions v-if="info" :column="{ xs: 1, md: 3 }" :data="[
          { label: '用户名', value: u.username }, { label: '角色', value: u.role === 'admin' ? '管理员' : '普通用户' },
          { label: '状态', value: !u.enabled ? '已禁用' : u.quotaBytes && u.usedBytes >= u.quotaBytes ? '流量超限' : '正常' },
          { label: '流量', value: `${fmtBytes(u.usedBytes)} / ${u.quotaBytes ? fmtBytes(u.quotaBytes) : '不限'}` },
          { label: '规则', value: `${info.rules} / ${u.role === 'admin' || !u.maxRules ? '不限' : u.maxRules}` },
          { label: '创建时间', value: fmtTime(u.createdAt) } ]" />
      </a-card>
      <a-card class="general-card" title="修改密码">
        <a-form :model="f" style="width:440px;max-width:100%" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }">
          <a-form-item label="当前密码"><a-input-password v-model="f.old" /></a-form-item>
          <a-form-item label="新密码"><a-input-password v-model="f.new" placeholder="至少 8 位" /></a-form-item>
          <a-form-item label="确认新密码"><a-input-password v-model="f.new2" /></a-form-item>
          <a-form-item><a-button type="primary" :loading="busy" @click="change">保存</a-button></a-form-item>
        </a-form>
      </a-card>
    </a-space>
  </div>
</template>

<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api, fmtBytes, fmtTime } from '../api'

const info = ref(null)
const u = computed(() => info.value?.user || {})
const f = reactive({ old: '', new: '', new2: '' })
const busy = ref(false)
onMounted(async () => { info.value = await api.get('/api/me') })

async function change() {
  if (f.new.length < 8) { Message.warning('新密码至少 8 位'); return }
  if (f.new !== f.new2) { Message.warning('两次输入的新密码不一致'); return }
  busy.value = true
  try {
    await api.put('/api/me/password', { old: f.old, new: f.new })
    Message.success('密码已修改，其他设备上的登录已失效')
    Object.assign(f, { old: '', new: '', new2: '' })
  } finally { busy.value = false }
}
</script>
