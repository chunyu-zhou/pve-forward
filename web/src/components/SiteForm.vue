<template>
  <a-form :model="f" :label-col-props="{ span: 5 }" :wrapper-col-props="{ span: 19 }">
    <a-form-item label="所属用户" required :extra="admin ? (isPublic ? '公共站点只有管理员可以管理，可指向节点上的任意虚拟机' : undefined) : '站点只能创建在自己名下'">
      <a-select v-model="f.userId" :disabled="!admin" placeholder="请选择用户" @change="onUserChange">
        <a-option v-if="admin" :value="session.user.id">公共（管理员）</a-option>
        <a-option v-for="u in userList" :key="u.id" :value="u.id">{{ u.username }}</a-option>
      </a-select>
    </a-form-item>
    <a-form-item label="节点" required :extra="opts.nodes.length ? (node && !node.deployed ? '该节点的 Web 入口尚未部署，站点会在部署后生效' : undefined) : '没有启用 Web 转发的节点，请联系管理员'">
      <a-select v-model="f.nodeId" placeholder="请选择节点" @change="onNodeChange">
        <a-option v-for="n in opts.nodes" :key="n.id" :value="n.id">{{ n.name }}<span class="muted">（{{ n.wanIp4 }}{{ n.status !== 'online' ? '，离线' : '' }}）</span></a-option>
      </a-select>
    </a-form-item>

    <a-form-item label="域名" required :extra="opts.domains.length ? '填写子域名前缀并选择主域名；保存后自动添加 DNS 解析，指向节点公网 IP' : '没有可用的主域名，请联系管理员添加'">
      <div style="width:100%">
        <template v-for="(d, i) in f.hosts" :key="i">
          <div class="domain-row">
            <a-input-group>
              <a-input v-model="d.sub" placeholder="如 dev-zcy" style="flex:1" allow-clear :error="['bad', 'taken', 'dup'].includes(d.st)" />
              <a-select v-model="d.domainId" style="width:200px" placeholder="选择主域名" :error="!!d.sub && !d.domainId"
                        :options="opts.domains.map(x => ({ label: '.' + x.name, value: x.id }))" />
            </a-input-group>
            <a-button v-if="f.hosts.length > 1" shape="circle" size="small" @click="f.hosts.splice(i, 1)"><template #icon><icon-minus /></template></a-button>
          </div>
          <div v-if="d.st" class="domain-st" :class="{ ok: d.st === 'ok', bad: ['bad', 'taken', 'dup'].includes(d.st), wait: d.st === 'checking' }">
            <template v-if="d.st === 'checking'"><icon-loading /> 正在查询 {{ host(d) }} 的解析…</template>
            <template v-else-if="d.st === 'ok'"><icon-check-circle-fill /> {{ d.msg }}</template>
            <template v-else><icon-close-circle-fill /> {{ d.msg }}</template>
          </div>
        </template>
        <a-link v-if="f.hosts.length < 10" style="font-size:12px" @click="f.hosts.push({ sub: '', domainId: opts.domains[0]?.id })"><icon-plus /> 添加域名</a-link>
      </div>
    </a-form-item>

    <a-form-item label="入口端口" required :extra="`访问地址：${f.hosts[0]?.sub && f.hosts[0]?.domainId ? url(f.hosts[0]) : 'http(s)://子域名.主域名'}`">
      <a-radio-group v-model="f.port" class="entry-port" @change="onPortChange">
        <a-radio v-for="p in entryPorts" :key="p" :value="p">
          <div class="pt">{{ portLabel(p) }}</div><span v-if="p === node?.default" class="def">默认</span>
          <div class="muted">{{ isHttps(p) ? '需要证书' : p === 80 ? '直接访问' : '需带端口访问' }}</div>
        </a-radio>
      </a-radio-group>
    </a-form-item>
    <a-form-item v-if="isHttps(f.port)" label="证书" required :extra="certChoices.length ? '只列出覆盖所填域名的证书' : undefined">
      <a-select v-model="f.certId" placeholder="选择证书">
        <a-option v-for="c in certChoices" :key="c.id" :value="c.id" :label="`${c.name}（${c.domains.join('、')}）`">
          <div class="ip-opt"><span>{{ c.name }} <span class="muted">{{ c.domains.join('、') }}</span></span><span :style="{ color: expColor(c.days), fontSize: '12px' }">{{ c.days }} 天后到期</span></div>
        </a-option>
        <template #empty><div style="padding:12px;text-align:center" class="muted">没有覆盖所填域名的证书，请联系管理员添加，或改用 HTTP 入口</div></template>
      </a-select>
    </a-form-item>

    <a-form-item label="转发到" required :extra="isPublic ? '节点上的全部虚拟机' : '只能选择自己的虚拟机或公共虚拟机'">
      <a-input-group style="width:100%">
        <a-select v-model="f.scheme" style="width:100px" :options="['http', 'https']" />
        <a-select v-model="f.vmId" placeholder="选择虚拟机" style="flex:1" allow-search :disabled="!f.nodeId">
          <a-option v-for="v in opts.vms" :key="v.id" :value="v.id" :label="`${v.vmid} ${v.name}（${v.ipv4 || '未分配 IP'}）`" :disabled="!v.ipv4">
            <div class="ip-opt"><span><span class="mono">{{ v.vmid }}</span> {{ v.name }}<a-tag v-if="v.shared" size="small" style="margin-left:4px">公共</a-tag></span>
              <span class="mono muted">{{ v.ipv4 || '未分配 IP' }}</span></div>
          </a-option>
        </a-select>
        <a-input-number v-model="f.targetPort" :min="1" :max="65535" style="width:130px" placeholder="端口"><template #prefix>:</template></a-input-number>
      </a-input-group>
    </a-form-item>
    <a-form-item label="备注"><a-input v-model="f.remark" :max-length="40" show-word-limit placeholder="选填" /></a-form-item>

    <div class="adv-toggle" @click="adv = !adv">高级参数 <icon-down :style="{ transform: adv ? 'rotate(180deg)' : 'none', transition: 'transform .2s' }" /></div>
    <template v-if="adv">
      <a-form-item label="选项">
        <a-space direction="vertical" :size="6">
          <a-checkbox v-model="f.opts.ws">支持 WebSocket</a-checkbox>
          <a-checkbox v-model="f.opts.realIp">传递真实客户端 IP（X-Forwarded-For / X-Real-IP）</a-checkbox>
          <a-checkbox v-model="f.opts.block">拦截常见攻击（SQL 注入、路径穿越等）</a-checkbox>
          <a-checkbox v-model="f.opts.cache">缓存静态资源</a-checkbox>
          <a-checkbox v-if="isHttps(f.port)" v-model="f.opts.forceHttps">HTTP 自动跳转 HTTPS</a-checkbox>
        </a-space>
      </a-form-item>
      <a-form-item label="访问密码" :extra="auth && editingAuth ? '密码留空则沿用原密码' : '浏览器弹出登录框（HTTP Basic Auth）'">
        <a-space wrap>
          <a-switch v-model="auth" />
          <template v-if="auth">
            <a-input v-model="f.opts.authUser" placeholder="用户名" style="width:140px" />
            <a-input-password v-model="f.opts.authPassword" :placeholder="editingAuth ? '留空不修改' : '密码'" style="width:160px" />
          </template>
        </a-space>
      </a-form-item>
      <a-form-item label="上传大小上限"><a-input-number v-model="f.opts.bodyMax" :min="1" :max="10240" style="width:160px"><template #append>MB</template></a-input-number></a-form-item>
      <a-form-item label="超时"><a-input-number v-model="f.opts.timeout" :min="5" :max="3600" style="width:160px"><template #append>秒</template></a-input-number></a-form-item>
      <a-form-item label="自定义请求头" extra="每行一个，格式为“名称: 值”">
        <a-textarea v-model="f.opts.headers" class="mono" spellcheck="false" :auto-size="{ minRows: 2, maxRows: 4 }" placeholder="X-From: pve-forward" />
      </a-form-item>
      <a-form-item v-if="admin" label="自定义配置" extra="仅管理员可见，原样写入站点的 server 块；下发前执行 openresty -t 校验，错误时保留原配置">
        <a-textarea v-model="f.opts.custom" class="mono" spellcheck="false" :auto-size="{ minRows: 3, maxRows: 8 }" placeholder="location /api/ { ... }" />
      </a-form-item>
    </template>
  </a-form>
</template>

<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api, isAdmin, session, certCovers, expColor } from '../api'

const props = defineProps({ site: Object, userId: Number, active: Boolean })
const admin = computed(isAdmin)
const users = ref([])
const userList = computed(() => (admin.value ? users.value : [session.user]))
const isPublic = computed(() => admin.value && f.userId === session.user?.id)
const opts = reactive({ nodes: [], domains: [], certs: [], vms: [] })
const adv = ref(false)
const auth = ref(false)
const f = reactive({ hosts: [], opts: {} })

const node = computed(() => opts.nodes.find(n => n.id === f.nodeId))
const entryPorts = computed(() => (node.value ? [...node.value.http, ...node.value.https].sort((a, b) => a - b) : []))
const isHttps = p => !!node.value?.https.includes(p)
const portLabel = p => (isHttps(p) ? 'HTTPS ' : 'HTTP ') + p
const domainName = id => opts.domains.find(d => d.id === id)?.name || ''
const norm = d => ({ sub: (d.sub || '').trim().toLowerCase(), domainId: d.domainId })
const host = d => `${norm(d).sub}.${domainName(d.domainId)}`
const url = d => `${isHttps(f.port) ? 'https' : 'http'}://${host(d)}${[80, 443].includes(f.port) ? '' : ':' + f.port}`
const editingAuth = computed(() => !!props.site?.auth && props.site.opts?.authUser === f.opts.authUser)

// 证书：覆盖全部所填域名
const certChoices = computed(() => {
  const hosts = f.hosts.filter(d => d.domainId).map(d => `${norm(d).sub || 'x'}.${domainName(d.domainId)}`)
  return hosts.length ? opts.certs.filter(c => hosts.every(h => certCovers(c.domains, h))) : []
})
watch(certChoices, cs => { if (isHttps(f.port) && !cs.some(c => c.id === f.certId)) f.certId = cs[0]?.id })
function onPortChange(p) {
  if (isHttps(p)) { f.certId = certChoices.value[0]?.id; if (!props.site) f.opts.forceHttps = true } else f.certId = undefined
}

async function loadOpts(withNode = true) {
  if (!f.userId) return
  const d = await api.get(`/api/site-options?user=${f.userId}` + (withNode && f.nodeId ? `&node=${f.nodeId}` : ''))
  opts.nodes = d.nodes || []
  opts.domains = d.domains || []
  opts.certs = d.certs || []
  opts.vms = d.vms || []
}

// 打开时初始化
watch(() => props.active, async v => {
  if (!v) return
  adv.value = false
  if (admin.value && !users.value.length) users.value = await api.get('/api/users')
  const s = props.site
  const o = s?.opts || {}
  Object.assign(f, {
    userId: s ? s.userId : admin.value ? props.userId || users.value[0]?.id : session.user.id,
    nodeId: s?.nodeId, port: s?.port, scheme: s?.scheme || 'http', vmId: s?.vmId, targetPort: s?.targetPort ?? 80,
    certId: s?.certId ?? undefined, remark: s?.remark || '',
    hosts: s ? s.hosts.map(h => ({ sub: h.sub, domainId: h.domainId })) : [{ sub: '', domainId: undefined }],
    opts: { ws: o.ws ?? true, realIp: o.realIp ?? true, block: o.block ?? true, cache: o.cache ?? false, forceHttps: o.forceHttps ?? true,
      authUser: o.authUser || '', authPassword: '', bodyMax: o.bodyMax || 100, timeout: o.timeout || 60, headers: o.headers || '', custom: o.custom || '' },
  })
  auth.value = !!o.authUser
  await loadOpts(false)
  if (!f.nodeId) f.nodeId = (opts.nodes.find(n => n.status === 'online') || opts.nodes[0])?.id
  if (f.nodeId) await loadOpts()
  if (!s) {
    f.port = node.value?.default
    f.hosts[0].domainId = opts.domains[0]?.id
    onPortChange(f.port)
  }
  recheck(true)
}, { immediate: true })

async function onUserChange() {
  f.nodeId = undefined; f.vmId = undefined
  await loadOpts(false)
  f.nodeId = (opts.nodes.find(n => n.status === 'online') || opts.nodes[0])?.id
  if (f.nodeId) await loadOpts()
  f.port = node.value?.default
  for (const h of f.hosts) if (!opts.domains.some(d => d.id === h.domainId)) h.domainId = opts.domains[0]?.id
  recheck(true)
}
async function onNodeChange() {
  f.vmId = undefined
  await loadOpts()
  if (!entryPorts.value.includes(f.port)) { f.port = node.value?.default; onPortChange(f.port) }
}

// ---------------- 域名检查：输入后查询解析，已被使用则不能保存 ----------------
const subRe = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/
let seq = 0
function checkHost(d) {
  clearTimeout(d._t)
  const { sub, domainId } = norm(d)
  if (!sub || !domainId) { d.st = ''; d.msg = ''; return }
  if (!subRe.test(sub)) { d.st = 'bad'; d.msg = '子域名前缀只能包含字母、数字、短横线和点，如 dev-zcy'; return }
  const h = host(d)
  const i = f.hosts.indexOf(d)
  if (f.hosts.some((x, j) => j < i && norm(x).sub && x.domainId && host(x) === h)) { d.st = 'dup'; d.msg = `${h} 重复填写`; return }
  d.st = 'checking'
  const my = ++seq
  d._seq = my
  d._t = setTimeout(async () => {
    try {
      const r = await api.post('/api/sites/check-host', { sub, domainId, siteId: props.site?.id || 0, userId: f.userId }, { silent: true })
      if (d._seq !== my) return
      d.st = r.ok ? 'ok' : 'taken'
      d.msg = r.msg
    } catch (e) {
      if (d._seq === my) { d.st = 'taken'; d.msg = e.message }
    }
  }, 600)
}
function recheck(all) {
  for (const d of f.hosts) {
    const k = `${norm(d).sub}|${d.domainId}`
    if (all || k !== d._k || d.st === 'dup') { d._k = k; checkHost(d) }
  }
}
watch(() => f.hosts.map(d => `${d.sub}|${d.domainId}`).join(','), () => recheck())

const canSave = computed(() => !!f.nodeId && !!f.port && f.hosts.length > 0 && f.hosts.every(d => norm(d).sub && d.domainId && d.st === 'ok') &&
  (!isHttps(f.port) || !!f.certId))

async function save() {
  if (!f.vmId || !f.targetPort) { Message.warning('请选择目标虚拟机并填写端口'); return false }
  if (auth.value && !f.opts.authUser) { Message.warning('请填写访问密码的用户名'); return false }
  const body = {
    userId: f.userId, nodeId: f.nodeId, port: f.port, scheme: f.scheme, vmId: f.vmId, targetPort: f.targetPort,
    certId: isHttps(f.port) ? f.certId : null, remark: f.remark, hosts: f.hosts.map(norm),
    opts: { ...f.opts, authUser: auth.value ? f.opts.authUser.trim() : '', authPassword: auth.value ? f.opts.authPassword : '' },
  }
  const r = props.site?.id ? await api.put(`/api/sites/${props.site.id}`, body) : await api.post('/api/sites', body)
  const failed = (r.dns || []).filter(x => x.error)
  if (failed.length) Message.warning({ content: `已保存，但自动解析失败：${failed.map(x => `${x.host}（${x.error}）`).join('；')}`, duration: 8000 })
  else Message.success(r.dns?.length ? `已保存，已添加解析 ${r.dns.map(x => x.host).join('、')} → ${node.value?.wanIp4}` : '已保存')
  return true
}

defineExpose({ canSave, save })
</script>

<style scoped>
.domain-row { display: flex; align-items: center; gap: 8px; width: 100%; margin-bottom: 8px; }
.domain-row .arco-input-group { flex: 1; }
.domain-st { font-size: 12px; line-height: 20px; margin: -6px 0 6px; }
.domain-st.ok { color: rgb(var(--green-6)); }
.domain-st.bad { color: rgb(var(--red-6)); }
.domain-st.wait { color: var(--color-text-3); }
.entry-port { display: grid; grid-template-columns: repeat(4, 1fr); gap: 8px; width: 100%; }
.entry-port :deep(.arco-radio) { position: relative; overflow: hidden; margin: 0; padding: 8px 10px; min-width: 0; border: 1px solid var(--color-border-2); border-radius: 4px; }
.entry-port :deep(.arco-radio-checked) { border-color: rgb(var(--primary-6)); background: rgb(var(--primary-1)); }
.entry-port .pt { font-weight: 500; white-space: nowrap; }
.entry-port .muted { white-space: nowrap; }
.entry-port .def { position: absolute; top: 0; right: 0; padding: 0 6px; font-size: 12px; line-height: 18px; color: rgb(var(--primary-6)); background: rgb(var(--primary-2)); border-bottom-left-radius: 4px; }
.adv-toggle { display: flex; align-items: center; gap: 12px; margin: 4px 0 16px; cursor: pointer; user-select: none; color: var(--color-text-2); }
.adv-toggle::before, .adv-toggle::after { content: ''; flex: 1; border-top: 1px solid var(--color-border-2); }
.adv-toggle:hover { color: rgb(var(--primary-6)); }
</style>
