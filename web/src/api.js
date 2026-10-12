import { reactive } from 'vue'
import { Message } from '@arco-design/web-vue'

// 全局会话状态
export const session = reactive({ loaded: false, setup: false, site: 'PVE Forward', version: '', user: null, updateAvailable: '' })

export const isAdmin = () => session.user?.role === 'admin'

export class ApiError extends Error {
  constructor(status, message) {
    super(message)
    this.status = status
  }
}

async function request(method, url, body, opts = {}) {
  const headers = { 'X-Requested-With': 'pveforward', ...(opts.headers || {}) }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const resp = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin' })
  if (opts.raw) return resp
  const text = await resp.text()
  let data = null
  try { data = text ? JSON.parse(text) : null } catch { data = null }
  if (!resp.ok) {
    const msg = data?.error || `请求失败（${resp.status}）`
    if (resp.status === 401 && !opts.allow401 && session.user) {
      session.user = null
      location.hash = '#/login'
    }
    if (!opts.silent) Message.error(msg)
    throw new ApiError(resp.status, msg)
  }
  return data
}

export const api = {
  get: (url, opts) => request('GET', url, undefined, opts),
  post: (url, body, opts) => request('POST', url, body ?? {}, opts),
  put: (url, body, opts) => request('PUT', url, body ?? {}, opts),
  del: (url, opts) => request('DELETE', url, undefined, opts),
  raw: (method, url, body) => request(method, url, body, { raw: true }),
}

export async function loadSession() {
  const b = await api.get('/api/bootstrap')
  Object.assign(session, { loaded: true, setup: b.setup, site: b.site, version: b.version, user: b.user || null, updateAvailable: b.updateAvailable || '' })
  document.title = b.site
}

// 读取 NDJSON 流式响应，逐行回调
export async function streamNDJSON(url, body, onLine) {
  const resp = await api.raw('POST', url, body)
  if (!resp.ok) {
    let msg = `请求失败（${resp.status}）`
    try { msg = (await resp.json()).error || msg } catch { /* ignore */ }
    throw new ApiError(resp.status, msg)
  }
  const reader = resp.body.getReader()
  const dec = new TextDecoder()
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (value) buf += dec.decode(value, { stream: true })
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).trim()
      buf = buf.slice(i + 1)
      if (line) onLine(JSON.parse(line))
    }
    if (done) break
  }
}

// ---------------- 格式化 ----------------

// 数值最多保留 digits 位小数（默认两位），去掉末尾多余的 0
export const num = (n, digits = 2) => Number((Number(n) || 0).toFixed(Math.min(digits, 2)))

// 字节数：最低以 KB 显示，最多两位小数
export function bytesParts(b, digits = 2) {
  b = (Number(b) || 0) / 1024
  const u = ['KB', 'MB', 'GB', 'TB', 'PB']
  let i = 0
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++ }
  return [num(b, digits), u[i]]
}
export const fmtBytes = (b, digits = 2) => bytesParts(b, digits).join(' ')

export const fmtRate = b => fmtBytes(b) + '/s'

export function fmtK(n) {
  n = Number(n) || 0
  return n >= 1000 ? num(n / 1000) + 'k' : String(num(n))
}

export function fmtUptime(s) {
  s = Number(s) || 0
  if (s <= 0) return '-'
  const d = Math.floor(s / 86400)
  if (d > 0) return d + ' 天'
  const h = Math.floor(s / 3600)
  if (h > 0) return h + ' 小时'
  return Math.max(1, Math.floor(s / 60)) + ' 分钟'
}

export function fmtAgo(ts) {
  if (!ts) return '-'
  const s = Math.floor(Date.now() / 1000 - ts)
  if (s < 10) return '刚刚'
  if (s < 60) return s + ' 秒前'
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前'
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前'
  return Math.floor(s / 86400) + ' 天前'
}

export function fmtTime(ts) {
  if (!ts) return '-'
  const d = new Date(ts * 1000)
  const p = n => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

export const pct = (u, t) => (t ? Math.round((u / t) * 100) : 0)

// 使用率颜色
export const lvl = p => (p >= 85 ? '#f53f3f' : p >= 65 ? '#ff7d00' : '#00b42a')

export const protoName = { tcp: 'TCP', udp: 'UDP', both: 'TCP + UDP' }
export const ipverName = { v4: 'IPv4', v6: 'IPv6', both: 'IPv4 + IPv6' }
export const allocKindName = { single: '单个', multi: '多个', range: '范围' }

export const nodeBadge = {
  online: { badge: 'success', text: '在线' },
  offline: { badge: 'danger', text: '离线' },
  pending: { badge: 'warning', text: '待初始化' },
  waiting: { badge: 'normal', text: '等待连接' },
}

// 复制文本（HTTP 下 navigator.clipboard 不可用时回退）
export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text)
  } catch {
    const ta = document.createElement('textarea')
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    ta.remove()
  }
  Message.success('已复制')
}

// 展开端口分配
export function expandAlloc(a) {
  if (a.kind === 'range') {
    const [f, t] = a.spec.split('-').map(Number)
    const r = []
    for (let p = f; p <= t; p++) r.push(p)
    return r
  }
  return a.spec.split(',').map(Number)
}

// 解析端口集合文本，如 "22,8006,3000-3100"；返回合并后的区间，格式错误时抛出异常
export function parsePortSet(spec) {
  const spans = []
  for (const t of String(spec || '').split(/[,，、\s]+/).filter(Boolean)) {
    const m = /^(\d+)(?:-(\d+))?$/.exec(t)
    const a = m && Number(m[1]), b = m && Number(m[2] ?? m[1])
    if (!m || a < 1 || b > 65535 || a > b) throw new Error(`“${t}” 不是有效的端口或范围`)
    spans.push([a, b])
  }
  spans.sort((x, y) => x[0] - y[0])
  const merged = []
  for (const s of spans) {
    const last = merged[merged.length - 1]
    if (last && s[0] <= last[1] + 1) last[1] = Math.max(last[1], s[1])
    else merged.push([...s])
  }
  return merged
}

// 节点端口策略说明：可分配范围中去掉保留端口后的数量；格式错误时返回错误信息
export function portPolicyHint(range, reserved) {
  try {
    const r = parsePortSet(range), x = parsePortSet(reserved)
    if (!r.length) return { error: '请填写可分配端口范围' }
    let n = 0
    for (const [a, b] of r) for (let p = a; p <= b; p++) if (!x.some(([c, d]) => p >= c && p <= d)) n++
    return n ? { count: n } : { error: '可分配范围内的端口都被保留了' }
  } catch (e) { return { error: e.message } }
}
