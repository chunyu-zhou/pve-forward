<template>
  <div class="wp-container">
    <div class="wp-left">
      <div class="panel">
        <div class="wp-banner">
          <a-typography-title :heading="5" style="margin-top: 0">欢迎回来！{{ session.user?.username }}</a-typography-title>
          <a-divider class="panel-border" />
        </div>
        <a-grid :cols="24" :row-gap="16" class="data-panel">
          <a-grid-item v-for="(s, i) in stats" :key="s.title" class="panel-col" :class="{ last: i === stats.length - 1 }" :span="{ xs: 12, xl: 6 }">
            <a-space>
              <a-avatar :size="54" class="col-avatar"><component :is="s.icon" :style="{ color: s.color, fontSize: '24px' }" /></a-avatar>
              <a-statistic :title="s.title" :value="s.value" :precision="s.precision || 0" :value-from="0" animation show-group-separator>
                <template #suffix><span class="unit">{{ s.unit }}</span></template>
              </a-statistic>
            </a-space>
            <div class="muted" style="margin:6px 0 0 66px">{{ s.sub }}</div>
          </a-grid-item>
          <a-grid-item :span="24"><a-divider class="panel-border" style="margin: 4px 0 0 0" /></a-grid-item>
        </a-grid>
        <a-card class="general-card" title="流量趋势" :header-style="{ paddingBottom: 0 }" :body-style="{ paddingTop: '20px' }">
          <template #extra>
            <a-space>
              <a-select v-if="nodes.length > 1" v-model="chartNode" size="small" style="width:140px" :options="[{ label: '全部节点', value: 0 }, ...nodes.map(n => ({ label: n.name, value: n.id }))]" @change="loadDash" />
              <a-radio-group v-model="range" type="button" size="small" @change="loadDash"><a-radio value="24h">近 24 小时</a-radio><a-radio value="7d">近 7 天</a-radio></a-radio-group>
            </a-space>
          </template>
          <div class="chart-legend"><span><i style="background:#165dff"></i>上行（客户端 → 虚拟机）</span><span><i style="background:#ff7d00"></i>下行（虚拟机 → 客户端）</span></div>
          <TrafficChart :series="dash.series || []" :step="dash.step || 3600" />
        </a-card>
      </div>
      <a-grid :cols="24" :col-gap="16" :row-gap="16" style="margin-top: 16px">
        <a-grid-item :span="{ xs: 24, lg: 12 }">
          <a-card class="general-card" title="流量排行（24 小时）" :header-style="{ paddingBottom: 0 }" :body-style="{ padding: '17px 20px 21px 20px' }" style="min-height:395px">
            <template #extra><a-link @click="$router.push('/rules')">查看更多</a-link></template>
            <a-space direction="vertical" :size="10" fill>
              <a-radio-group v-model="rankProto" type="button"><a-radio value="">全部</a-radio><a-radio value="tcp">TCP</a-radio><a-radio value="udp">UDP</a-radio></a-radio-group>
              <a-table :data="ranking" :pagination="false" :bordered="false" :scroll="{ x: '100%', y: '264px' }" row-key="ruleId">
                <template #columns>
                  <a-table-column title="排名" :width="56"><template #cell="{ rowIndex }">{{ rowIndex + 1 }}</template></a-table-column>
                  <a-table-column title="规则"><template #cell="{ record }"><a-typography-paragraph :ellipsis="{ rows: 1 }" style="margin:0">{{ record.rule.remark || record.rule.vmName }} <span class="muted mono">{{ record.rule.nodeName }}:{{ record.rule.port }}</span></a-typography-paragraph></template></a-table-column>
                  <a-table-column v-if="admin" title="用户" :width="80"><template #cell="{ record }">{{ record.rule.owner }}</template></a-table-column>
                  <a-table-column title="流量" :width="96"><template #cell="{ record }">{{ fmtBytes(record.bytes) }}</template></a-table-column>
                  <a-table-column title="日环比" :width="90"><template #cell="{ record }">
                    <span>{{ Math.abs(record.change).toFixed(1) }}%</span>
                    <icon-caret-up v-if="record.change > 0" style="color:#f53f3f;font-size:8px;margin-left:4px" />
                    <icon-caret-down v-else-if="record.change < 0" style="color:#00b42a;font-size:8px;margin-left:4px" /></template></a-table-column>
                </template>
                <template #empty><a-empty description="近 24 小时没有流量" /></template>
              </a-table>
            </a-space>
          </a-card>
        </a-grid-item>
        <a-grid-item :span="{ xs: 24, lg: 12 }">
          <a-card class="general-card" title="规则构成" :header-style="{ paddingBottom: 0 }" :body-style="{ padding: '20px' }" style="min-height:395px">
            <template #extra><a-radio-group v-model="pieBy" type="button" size="small"><a-radio value="node">按节点</a-radio><a-radio value="proto">按协议</a-radio><a-radio value="ipVer">按 IP 版本</a-radio></a-radio-group></template>
            <div class="donut-wrap">
              <svg viewBox="0 0 220 220" width="240" height="240">
                <circle cx="110" cy="110" r="78" fill="none" stroke="#f2f3f5" stroke-width="26" />
                <circle v-for="seg in pie.segs" :key="seg.name" cx="110" cy="110" r="78" fill="none" :stroke="seg.color" stroke-width="26"
                        :stroke-dasharray="seg.len + ' ' + (pie.C - seg.len)" :stroke-dashoffset="-seg.off" transform="rotate(-90 110 110)" />
                <text x="110" y="104" text-anchor="middle" font-size="12" fill="#86909c">规则总数</text>
                <text x="110" y="130" text-anchor="middle" font-size="24" font-weight="500" fill="#1d2129">{{ pie.total }}</text>
              </svg>
              <div class="donut-legend">
                <div v-for="seg in pie.segs" :key="seg.name"><i :style="{ background: seg.color }"></i><span>{{ seg.name }}</span><b>{{ seg.value }}</b><span class="muted">{{ pie.total ? Math.round((seg.value / pie.total) * 100) : 0 }}%</span></div>
              </div>
            </div>
          </a-card>
        </a-grid-item>
      </a-grid>
    </div>

    <div class="wp-right">
      <a-grid :cols="24" :row-gap="16">
        <a-grid-item :span="24">
          <div class="panel moduler-wrap">
            <a-card class="general-card" title="快捷操作" :header-style="{ paddingBottom: 0 }" :body-style="{ padding: '24px 20px 0 20px' }">
              <a-row :gutter="8">
                <a-col v-for="q in quickOps" :key="q.text" :span="8" class="wrapper" @click="q.run()">
                  <div class="icon"><component :is="q.icon" /></div>
                  <a-typography-paragraph class="text">{{ q.text }}</a-typography-paragraph>
                </a-col>
              </a-row>
            </a-card>
          </div>
        </a-grid-item>
        <a-grid-item class="panel" :span="24">
          <a-card v-if="admin" class="general-card" title="节点状态" :header-style="{ paddingBottom: 0 }" :body-style="{ padding: '12px 20px' }">
            <template #extra><a-link @click="$router.push('/nodes')">管理</a-link></template>
            <div v-for="n in nodes" :key="n.id" class="node-item">
              <div class="status-item"><span><b style="font-weight:500">{{ n.name }}</b> <span class="muted">{{ n.remark }}</span></span>
                <a-badge :status="nodeBadge[n.status].badge" :text="nodeBadge[n.status].text" /></div>
              <div class="muted">{{ n.initialized ? `${n.net.wanIp4} · ${n.net.cidr4}` : '等待接入 / 网络初始化' }}<template v-if="n.status === 'online' && n.metrics"> · CPU {{ n.metrics.cpu }}% · 内存 {{ pct(n.metrics.memUsed, n.metrics.memTotal) }}%</template>
                <template v-if="n.status === 'offline'"> · 心跳 {{ fmtAgo(n.lastSeen) }}</template></div>
            </div>
            <a-empty v-if="!nodes.length" description="还没有节点"><a-button size="small" type="primary" @click="$router.push('/nodes/new')">添加节点</a-button></a-empty>
          </a-card>
          <a-card v-else class="general-card" title="我的配额" :header-style="{ paddingBottom: 0 }" :body-style="{ padding: '12px 20px' }">
            <div v-for="g in quota" :key="g.node" class="node-item">
              <div class="status-item"><b style="font-weight:500">{{ g.name }}</b><span>端口 {{ g.used }} / {{ g.total }}</span></div>
              <a-progress :percent="g.total ? g.used / g.total : 0" :show-text="false" size="small" />
            </div>
            <template v-if="me">
              <div class="status-item"><span class="muted" style="font-size:13px">规则</span><span>{{ me.rules }} 条</span></div>
              <div class="status-item"><span class="muted" style="font-size:13px">流量</span><span>{{ fmtBytes(me.user.usedBytes) }} / {{ me.user.quotaBytes ? fmtBytes(me.user.quotaBytes) : '不限' }}</span></div>
              <div class="status-item"><span class="muted" style="font-size:13px">虚拟机</span><span>{{ me.vms }} 台</span></div>
            </template>
          </a-card>
        </a-grid-item>
        <a-grid-item class="panel" :span="24">
          <a-card class="general-card" title="事件通知" :header-style="{ paddingBottom: 0 }" :body-style="{ padding: '15px 20px 13px 20px' }">
            <template #extra><a-link @click="$router.push({ path: '/rules', query: { status: 'err' } })">查看异常</a-link></template>
            <div v-for="e in dash.events" :key="e.id" class="ann-item">
              <a-tag :color="{ error: 'red', warn: 'orangered', info: 'cyan', todo: 'gold' }[e.level]" size="small">{{ { error: '异常', warn: '告警', info: '通知', todo: '待办' }[e.level] }}</a-tag>
              <a-tooltip :content="`${fmtTime(e.ts)} ${e.message}`"><span class="item-content">{{ e.message }}</span></a-tooltip>
            </div>
            <a-empty v-if="!dash.events?.length" description="暂无事件" />
          </a-card>
        </a-grid-item>
      </a-grid>
    </div>
    <RuleModal v-model:visible="ruleModal" @saved="load" />
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, session, isAdmin, nodeBadge, fmtBytes, fmtAgo, fmtTime, pct, protoName, ipverName } from '../api'
import RuleModal from '../components/RuleModal.vue'
import TrafficChart from '../components/TrafficChart.vue'

const router = useRouter()
const admin = computed(isAdmin)
const dash = ref({ series: [], events: [], ranking: [], todayUp: 0, todayDown: 0, totalUp: 0, totalDown: 0 })
const rules = ref([])
const nodes = ref([])
const users = ref([])
const me = ref(null)
const range = ref('24h')
const chartNode = ref(0)
const rankProto = ref('')
const pieBy = ref('node')
const ruleModal = ref(false)

async function loadDash() { dash.value = await api.get(`/api/dashboard?range=${range.value}&node=${chartNode.value}&tz=${-new Date().getTimezoneOffset()}`) }
async function load() {
  const tasks = [loadDash(), api.get('/api/rules').then(r => (rules.value = r))]
  if (admin.value) tasks.push(api.get('/api/nodes').then(d => (nodes.value = d.nodes)), api.get('/api/users').then(u => (users.value = u)))
  else tasks.push(api.get('/api/me').then(m => (me.value = m)))
  await Promise.all(tasks)
}
let timer
onMounted(() => { load(); timer = setInterval(load, 60000) })
onUnmounted(() => clearInterval(timer))

const stats = computed(() => {
  const rs = rules.value
  const err = rs.filter(r => r.enabled && r.error).length
  const ruleStat = { title: '转发规则', value: rs.length, unit: '条', sub: `正常 ${rs.filter(r => r.enabled && !r.error).length} · 异常 ${err} · 停用 ${rs.filter(r => !r.enabled).length}`, icon: 'icon-swap', color: 'rgb(var(--green-6))' }
  const today = dash.value.todayUp + dash.value.todayDown
  const traffic = { title: '今日流量', value: Number((today / 2 ** 30).toFixed(2)), precision: 2, unit: 'GB', sub: `上行 ${fmtBytes(dash.value.todayUp)} · 下行 ${fmtBytes(dash.value.todayDown)}`, icon: 'icon-cloud-download', color: 'rgb(var(--orange-6))' }
  const total = { title: '累计流量', value: Number(((dash.value.totalUp + dash.value.totalDown) / 2 ** 30).toFixed(2)), precision: 2, unit: 'GB', sub: '全部规则合计', icon: 'icon-bar-chart', color: 'rgb(var(--cyan-6))' }
  if (admin.value) {
    const ready = nodes.value.filter(n => n.initialized)
    return [
      { title: '在线节点', value: nodes.value.filter(n => n.status === 'online').length, unit: '/ ' + nodes.value.length, sub: `离线 ${ready.filter(n => n.status !== 'online').length} · 待接入 ${nodes.value.length - ready.length}`, icon: 'icon-cloud', color: 'rgb(var(--arcoblue-6))' },
      { title: '用户', value: users.value.length, unit: '人', sub: `已禁用 ${users.value.filter(u => !u.enabled).length} · 流量超限 ${users.value.filter(u => u.quotaBytes && u.usedBytes >= u.quotaBytes).length}`, icon: 'icon-user-group', color: 'rgb(var(--purple-6))' },
      ruleStat, traffic,
    ]
  }
  const ports = me.value ? me.value.allocations.reduce((s, a) => s + a.count, 0) : 0
  return [
    { title: '我的端口', value: ports, unit: '个', sub: `已使用 ${rs.length}`, icon: 'icon-tag', color: 'rgb(var(--arcoblue-6))' },
    ruleStat, traffic, total,
  ]
})

const ranking = computed(() => (dash.value.ranking || []).map(r => ({ ...r, rule: rules.value.find(x => x.id === r.ruleId) }))
  .filter(r => r.rule && (!rankProto.value || r.rule.proto === rankProto.value || r.rule.proto === 'both')))

const palette = ['#165dff', '#14c9c9', '#ff7d00', '#722ed1', '#00b42a', '#f53f3f', '#eb0aa4']
const pie = computed(() => {
  const rs = rules.value
  let groups
  if (pieBy.value === 'node') groups = [...new Map(rs.map(r => [r.nodeId, r.nodeName]))].map(([id, name]) => [name, r => r.nodeId === id])
  else if (pieBy.value === 'proto') groups = Object.entries(protoName).map(([k, v]) => [v, r => r.proto === k])
  else groups = Object.entries(ipverName).map(([k, v]) => [v, r => r.ipVer === k])
  const C2 = 2 * Math.PI * 78
  let off = 0
  const segs = groups.map(([name, f], i) => {
    const value = rs.filter(f).length
    const len = rs.length ? (value / rs.length) * C2 : 0
    const seg = { name, color: palette[i % palette.length], value, len, off }
    off += len
    return seg
  })
  return { C: C2, total: rs.length, segs }
})

const quota = computed(() => {
  if (!me.value) return []
  const m = new Map()
  for (const a of me.value.allocations) m.set(a.nodeId, (m.get(a.nodeId) || 0) + a.count)
  return [...m].map(([node, total]) => ({ node, name: me.value.nodeNames[node], total, used: (me.value.usedPorts[node] || []).length }))
})

const quickOps = computed(() => admin.value ? [
  { text: '添加规则', icon: 'icon-plus-circle', run: () => (ruleModal.value = true) },
  { text: '添加用户', icon: 'icon-user-add', run: () => router.push('/users/new') },
  { text: '添加节点', icon: 'icon-cloud', run: () => router.push('/nodes/new') },
  { text: '资源监控', icon: 'icon-desktop', run: () => router.push('/monitor') },
  { text: '虚拟机', icon: 'icon-storage', run: () => router.push('/vms') },
  { text: '系统设置', icon: 'icon-settings', run: () => router.push('/settings') },
] : [
  { text: '添加规则', icon: 'icon-plus-circle', run: () => (ruleModal.value = true) },
  { text: '我的规则', icon: 'icon-swap', run: () => router.push('/rules') },
  { text: '资源监控', icon: 'icon-desktop', run: () => router.push('/monitor') },
])
</script>
