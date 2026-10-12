<template>
  <div class="container" style="padding-top:16px">
    <a-card class="general-card" title="SSH 凭证">
      <template #extra><span class="muted">凭证在控制端加密保存，可被多个 SSH 节点复用；密码与私钥保存后不可查看</span></template>
      <a-row style="margin-bottom: 16px">
        <a-space><a-button type="primary" @click="open()"><template #icon><icon-plus /></template>新建凭证</a-button>
          <a-input-search v-model="kw" placeholder="名称 / 用户名" style="width:220px" allow-clear /></a-space>
      </a-row>
      <a-table row-key="id" :data="list.filter(c => !kw || (c.name + c.username).includes(kw))" :pagination="false" :bordered="false" :loading="loading">
        <template #columns>
          <a-table-column title="名称" data-index="name" :width="180" />
          <a-table-column title="类型" :width="90"><template #cell="{ record }"><a-tag :color="record.type === 'key' ? 'arcoblue' : 'orange'">{{ record.type === 'key' ? '密钥' : '密码' }}</a-tag></template></a-table-column>
          <a-table-column title="用户名" :width="110"><template #cell="{ record }"><span class="mono">{{ record.username }}</span></template></a-table-column>
          <a-table-column title="指纹 / 说明"><template #cell="{ record }"><span v-if="record.type === 'key'" class="mono muted">{{ record.keyAlgo }} · {{ record.keyFp }}</span><span v-else class="muted">密码已加密保存</span>
            <div v-if="record.remark" class="muted">{{ record.remark }}</div></template></a-table-column>
          <a-table-column title="使用节点" :width="200"><template #cell="{ record }">
            <a-space v-if="record.nodes.length" :size="4" wrap><a-tag v-for="n in record.nodes" :key="n" size="small">{{ n }}</a-tag></a-space><span v-else class="muted">未使用</span></template></a-table-column>
          <a-table-column title="更新时间" :width="150"><template #cell="{ record }">{{ fmtTime(record.updatedAt) }}</template></a-table-column>
          <a-table-column title="操作" :width="140"><template #cell="{ record }"><a-space :size="0">
            <a-button type="text" size="small" @click="open(record)">编辑</a-button>
            <a-popconfirm :content="record.nodes.length ? '该凭证正被节点使用，无法删除' : '确定删除该凭证？'" type="warning" @ok="remove(record)">
              <a-button type="text" size="small" status="danger" :disabled="!!record.nodes.length">删除</a-button></a-popconfirm></a-space></template></a-table-column>
        </template>
        <template #empty><a-empty description="还没有凭证" /></template>
      </a-table>
    </a-card>

    <a-modal v-model:visible="visible" :title="f.id ? '编辑凭证' : '新建凭证'" :width="620" :ok-loading="saving" @before-ok="save">
      <a-form :model="f" :label-col-props="{ span: 5 }" :wrapper-col-props="{ span: 19 }">
        <a-form-item label="名称" required><a-input v-model="f.name" placeholder="例如 机房 A root 密钥" /></a-form-item>
        <a-form-item label="类型"><a-radio-group v-model="f.type" type="button"><a-radio value="password">密码</a-radio><a-radio value="key">密钥</a-radio></a-radio-group></a-form-item>
        <a-form-item label="用户名" extra="需为 root，或具备免密 sudo 权限的用户"><a-input v-model="f.username" /></a-form-item>
        <a-form-item v-if="f.type === 'password'" label="密码"><a-input-password v-model="f.password" :placeholder="f.id ? '留空则不修改' : ''" /></a-form-item>
        <template v-else>
          <a-form-item label="密钥来源"><a-radio-group v-model="f.src" @change="f.src === 'gen' && gen()"><a-radio value="paste">粘贴私钥</a-radio><a-radio value="gen">由控制端生成</a-radio></a-radio-group></a-form-item>
          <template v-if="f.src === 'paste'">
            <a-form-item label="私钥"><a-textarea v-model="f.privateKey" :auto-size="{ minRows: 4, maxRows: 8 }" class="mono" :placeholder="f.id ? '留空则不修改' : '-----BEGIN OPENSSH PRIVATE KEY-----'" /></a-form-item>
            <a-form-item label="私钥口令"><a-input-password v-model="f.passphrase" placeholder="无口令可留空" /></a-form-item>
          </template>
          <a-form-item v-else label="公钥" :extra="`ED25519 · ${generated.fingerprint || ''}。私钥只保存在控制端；请在每台 PVE 上执行一次以下命令`">
            <a-spin :loading="!generated.command" style="width:100%">
              <div class="cmd" style="width:100%">{{ generated.command }}<a-button class="copy" size="mini" @click="copyText(generated.command)"><template #icon><icon-copy /></template>复制</a-button></div></a-spin>
          </a-form-item>
        </template>
        <a-form-item label="备注"><a-input v-model="f.remark" placeholder="选填" /></a-form-item>
      </a-form>
    </a-modal>
  </div>
</template>

<script setup>
import { onMounted, reactive, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api, fmtTime, copyText } from '../api'

const list = ref([])
const loading = ref(false)
const kw = ref('')
const visible = ref(false)
const saving = ref(false)
const f = reactive({})
const generated = reactive({ keyId: '', command: '', fingerprint: '' })

async function load() { loading.value = true; try { list.value = await api.get('/api/credentials') } finally { loading.value = false } }
onMounted(load)

function open(c) {
  Object.assign(generated, { keyId: '', command: '', fingerprint: '' })
  Object.assign(f, c ? { id: c.id, name: c.name, type: c.type, username: c.username, remark: c.remark, password: '', privateKey: '', passphrase: '', src: 'paste' }
    : { id: 0, name: '', type: 'key', username: 'root', remark: '', password: '', privateKey: '', passphrase: '', src: 'paste' })
  visible.value = true
}
async function gen() { Object.assign(generated, await api.post('/api/credentials/generate-key')) }
async function save(done) {
  saving.value = true
  try {
    const body = { name: f.name, type: f.type, username: f.username, remark: f.remark, password: f.password, privateKey: f.src === 'paste' ? f.privateKey : '', passphrase: f.passphrase, keyId: f.type === 'key' && f.src === 'gen' ? generated.keyId : '' }
    if (f.id) await api.put(`/api/credentials/${f.id}`, body)
    else await api.post('/api/credentials', body)
    Message.success('凭证已加密保存')
    load()
    done(true)
  } catch { done(false) } finally { saving.value = false }
}
async function remove(c) { await api.del(`/api/credentials/${c.id}`); Message.success('已删除'); load() }
</script>
