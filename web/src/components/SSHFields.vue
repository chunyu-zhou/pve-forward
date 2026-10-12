<template>
  <a-form-item label="SSH 地址" required :validate-status="err.host ? 'error' : undefined" :help="err.host">
    <a-input-group style="width:100%"><a-input v-model="f.sshHost" placeholder="IP 或域名" style="width:72%" /><a-input-number v-model="f.sshPort" :min="1" :max="65535" style="width:28%" /></a-input-group>
  </a-form-item>
  <a-form-item label="认证方式">
    <a-radio-group v-model="f.sshAuth" type="button"><a-radio value="password">密码</a-radio><a-radio value="key">密钥</a-radio><a-radio value="cred">已保存凭证</a-radio></a-radio-group>
  </a-form-item>
  <a-form-item v-if="f.sshAuth === 'cred'" label="凭证" required :validate-status="err.cred ? 'error' : undefined" :help="err.cred">
    <a-select v-model="f.credId" placeholder="选择凭证">
      <a-option v-for="c in creds" :key="c.id" :value="c.id" :label="c.name"><div class="ip-opt"><span>{{ c.name }}</span><span class="muted">{{ c.username }} · {{ c.type === 'key' ? '密钥' : '密码' }}</span></div></a-option>
      <template #empty><div style="padding:12px;text-align:center" class="muted">还没有凭证</div></template>
    </a-select>
    <template #extra><a-link @click="$emit('new-cred')">管理凭证</a-link></template>
  </a-form-item>
  <template v-else>
    <a-form-item label="用户名" extra="需为 root，或具备免密 sudo 权限的用户"><a-input v-model="f.sshUser" /></a-form-item>
    <a-form-item v-if="f.sshAuth === 'password'" label="密码" :required="!edit" :validate-status="err.secret ? 'error' : undefined" :help="err.secret">
      <a-input-password v-model="f.sshPassword" :placeholder="edit ? '留空则不修改' : ''" /></a-form-item>
    <template v-else>
      <a-form-item label="私钥" :required="!edit" :validate-status="err.secret ? 'error' : undefined" :help="err.secret || '支持 OpenSSH / PEM 格式'">
        <a-textarea v-model="f.sshKey" :auto-size="{ minRows: 3, maxRows: 6 }" :placeholder="edit ? '留空则不修改' : '-----BEGIN OPENSSH PRIVATE KEY-----'" class="mono" /></a-form-item>
      <a-form-item label="私钥口令"><a-input-password v-model="f.sshPassphrase" placeholder="无口令可留空" /></a-form-item>
    </template>
    <a-form-item v-if="!edit" label=" ">
      <a-checkbox v-model="f.saveCred">保存为凭证，供其他节点复用</a-checkbox>
      <a-input v-if="f.saveCred" v-model="f.credName" size="small" placeholder="凭证名称" style="width:200px;margin-left:12px" /></a-form-item>
  </template>
</template>

<script setup>
defineProps({ f: Object, err: Object, creds: Array, edit: Boolean })
defineEmits(['new-cred'])
</script>
