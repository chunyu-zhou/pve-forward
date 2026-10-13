<template>
  <!-- 公开页：未开放 / 需要访问密码 -->
  <div v-if="pub && gate" class="pub-gate">
    <a-card class="general-card" style="width:420px;max-width:92vw">
      <div style="text-align:center;padding:24px 0 8px">
        <div class="logo-mark" style="margin:0 auto 12px;width:40px;height:40px;font-size:22px"><icon-swap /></div>
        <a-typography-title :heading="5" style="margin:0 0 4px">{{ gateTitle }}</a-typography-title>
        <a-result v-if="gate === 'closed'" status="404" title="页面未开放" subtitle="管理员未开启公开监控页，或地址已变更" style="padding:16px 0 0" />
        <template v-else>
          <div class="muted" style="margin-bottom:20px">请输入访问密码</div>
          <a-input-password v-model="pwd" placeholder="访问密码" @press-enter="unlock" />
          <div v-if="pwdErr" style="color:rgb(var(--red-6));font-size:12px;margin-top:6px;text-align:left">{{ pwdErr }}</div>
          <a-button type="primary" long style="margin-top:16px" @click="unlock">进入</a-button>
        </template>
      </div>
    </a-card>
  </div>

  <div v-else class="container" :class="{ 'pub-page': pub }" :style="pub ? {} : { paddingTop: '16px' }">
    <div v-if="pub" class="pub-head">
      <a-space :size="12"><div class="logo-mark"><icon-swap /></div>
        <div><div class="pub-title">{{ page.title }}</div><div class="muted">{{ page.desc }}</div></div></a-space>
      <a-space :size="16"><a-badge :status="allOnline ? 'success' : 'warning'" :text="allOnline ? '全部节点正常' : '部分节点异常'" />
        <span class="muted">更新于 {{ updatedText }} · 每 {{ page.refresh }} 秒刷新</span></a-space>
    </div>

    <!-- 汇总 -->
    <a-card class="general-card" :body-style="{ padding: '20px' }" style="margin-bottom:16px">
      <a-grid :cols="{ xs: 2, md: summary.length }" :col-gap="16" :row-gap="16">
        <a-grid-item v-for="s in summary" :key="s.title" class="mon-sum">
          <a-statistic :title="s.title" :value="num(s.value)" :precision="decimals(num(s.value))" :value-style="{ color: s.color, fontWeight: 500 }">
            <template #suffix><span class="unit">{{ s.unit }}</span></template></a-statistic>
          <div class="muted">{{ s.sub }}</div>
        </a-grid-item>
      </a-grid>
    </a-card>

    <!-- 节点：列表 / 方块 -->
    <a-card v-if="data.nodes.length" class="general-card" title="节点" style="margin-bottom:16px">
      <template #extra>
        <a-radio-group v-model="nodeView" type="button" size="small">
          <a-radio value="list"><icon-list /> 列表</a-radio><a-radio value="card"><icon-apps /> 宫格</a-radio></a-radio-group>
      </template>
    <a-grid v-if="nodeView === 'card'" class="node-grid" :cols="{ xs: 1, sm: 1, md: 2, lg: 2, xl: 3, xxl: 3 }" :col-gap="16" :row-gap="16">
      <a-grid-item v-for="n in data.nodes" :key="n.id">
        <a-card class="node-card" :class="{ offline: n.status !== 'online', active: nodeFilter === n.id }" hoverable @click="nodeFilter = nodeFilter === n.id ? 0 : n.id">
          <template #title><a-space><span>{{ n.name }}</span><a-badge :status="nodeBadge[n.status]?.badge" :text="nodeBadge[n.status]?.text" /></a-space></template>
          <template #extra><span class="muted">{{ n.status === 'online' && n.metrics ? '运行 ' + fmtUptime(n.metrics.uptime) : '最后心跳 ' + fmtAgo(n.lastSeen) }}</span></template>
          <template v-if="n.status === 'online' && n.metrics">
            <div class="mon-metric two">
              <span class="lbl">CPU</span>
              <div class="bar"><a-progress :percent="n.metrics.cpu / 100" :color="lvl(n.metrics.cpu)" :show-text="false" />
                <div class="val"><span>{{ num(n.metrics.cpu) }}%</span><span class="muted">{{ n.metrics.cores }} 核</span></div></div>
              <Spark class="spark" :series="[{ values: hist('n' + n.id, 'cpu'), color: lvl(n.metrics.cpu) }]" :max="100" :format="v => num(v) + '%'" />
            </div>
            <div class="mon-metric two">
              <span class="lbl">内存</span>
              <div class="bar"><a-progress :percent="pct(n.metrics.memUsed, n.metrics.memTotal) / 100" :color="lvl(pct(n.metrics.memUsed, n.metrics.memTotal))" :show-text="false" />
                <div class="val"><span>{{ pct(n.metrics.memUsed, n.metrics.memTotal) }}%</span><span class="muted">{{ fmtBytes(n.metrics.memUsed, 0) }} / {{ fmtBytes(n.metrics.memTotal, 0) }}</span></div></div>
              <Spark class="spark" :series="[{ values: hist('n' + n.id, 'mem'), color: lvl(pct(n.metrics.memUsed, n.metrics.memTotal)) }]" :max="100" :format="v => num(v) + '%'" />
            </div>
            <div class="mon-metric two">
              <span class="lbl">存储</span>
              <div class="bar"><a-progress :percent="pct(n.metrics.diskUsed, n.metrics.diskTotal) / 100" :color="lvl(pct(n.metrics.diskUsed, n.metrics.diskTotal))" :show-text="false" />
                <div class="val"><span>{{ pct(n.metrics.diskUsed, n.metrics.diskTotal) }}%</span><span class="muted">{{ fmtBytes(n.metrics.diskUsed) }} / {{ fmtBytes(n.metrics.diskTotal) }}</span></div></div>
              <span class="spark"></span>
            </div>
            <div class="mon-metric">
              <a-tooltip content="CPU 等待磁盘 IO 的时间占比"><span class="lbl" style="cursor:help">IO 等待</span></a-tooltip>
              <a-progress :percent="Math.min(n.metrics.iowait * 4, 100) / 100" :color="iowaitColor(n.metrics.iowait)" :show-text="false" />
              <span class="val">{{ num(n.metrics.iowait) }}%</span>
              <Spark class="spark" :series="[{ values: hist('n' + n.id, 'iow'), color: iowaitColor(n.metrics.iowait) }]" :max="25" :format="v => num(v) + '%'" />
            </div>
            <div v-if="n.disks.length" class="disk-list">
              <div v-for="d in n.disks" :key="d.dev" class="disk-item">
                <div class="disk-l1">
                  <a-tooltip :content="`${d.model || d.dev} · 读 ${fmtRate(d.rBps)} 写 ${fmtRate(d.wBps)}`">
                    <span class="dev"><span class="mono">{{ d.dev }}</span> <a-tag size="small">{{ d.kind }}</a-tag></span></a-tooltip>
                  <a-tag size="small" :color="busy(d.util, d.await).tag">{{ busy(d.util, d.await).text }} {{ num(d.util) }}%</a-tag>
                </div>
                <div class="disk-l2">
                  <a-progress :percent="Math.min(d.util, 100) / 100" :color="busy(d.util, d.await).color" :show-text="false" size="small" />
                  <span class="mono muted">{{ fmtK(d.rIops + d.wIops) }} IOPS</span>
                  <span class="mono" :style="{ color: d.await >= th.busyAwait ? busy(0, d.await).color : 'var(--color-text-3)' }">{{ num(d.await) }} ms</span>
                </div>
              </div>
            </div>
            <div class="mon-foot">
              <span>负载 <b class="mono">{{ num(n.metrics.load[0]) }}</b></span>
              <a-tooltip content="经转发规则的当前连接：TCP（已建立） / UDP"><span>连接 <b class="mono">{{ n.connsTcp }}</b> / <b class="mono">{{ n.connsUdp }}</b></span></a-tooltip>
              <span><icon-arrow-down style="color:rgb(var(--green-6))" /> {{ fmtRate(n.metrics.netIn) }} <icon-arrow-up style="color:rgb(var(--orange-6))" /> {{ fmtRate(n.metrics.netOut) }}</span>
              <span>虚拟机 <b>{{ data.vms.filter(v => v.nodeId === n.id && v.status === 'running').length }}</b>/{{ data.vms.filter(v => v.nodeId === n.id).length }}</span>
            </div>
          </template>
          <a-empty v-else :description="n.status === 'online' ? '等待首次采集…' : '节点离线，暂无实时数据'" style="padding:18px 0" />
        </a-card>
      </a-grid-item>
    </a-grid>
      <a-table v-else row-key="id" :data="data.nodes" :columns="nodeColumns" :pagination="false" :bordered="false" size="small"
               :scroll="pub ? undefined : { x: nodeColumns.reduce((t, c) => t + c.width, 0) }" :class="{ 'pub-table': pub }"
               :row-class="n => 'mon-row' + (nodeFilter === n.id ? ' node-active' : '')" @row-click="n => (nodeFilter = nodeFilter === n.id ? 0 : n.id)">
        <template #nname="{ record: n }">
          <div class="lines"><a-space :size="6"><b style="font-weight:500">{{ n.name }}</b><a-badge :status="nodeBadge[n.status]?.badge" :text="nodeBadge[n.status]?.text" /></a-space>
            <div class="muted" style="font-size:12px">{{ n.status === 'online' && n.metrics ? '运行 ' + fmtUptime(n.metrics.uptime) : '最后心跳 ' + fmtAgo(n.lastSeen) }}</div></div>
        </template>
        <template #ncpu="{ record: n }"><MonCell :on="!!(n.status === 'online' && n.metrics)" mode="bars" :pct="n.metrics?.cpu || 0" :text="num(n.metrics?.cpu) + '%'" :sub="(n.metrics?.cores || 0) + ' 核'" /></template>
        <template #nmem="{ record: n }"><MonCell :on="!!(n.status === 'online' && n.metrics)" mode="bars" :pct="pct(n.metrics?.memUsed, n.metrics?.memTotal)" :text="pct(n.metrics?.memUsed, n.metrics?.memTotal) + '%'" :sub="fmtBytes(n.metrics?.memUsed, 0) + ' / ' + fmtBytes(n.metrics?.memTotal, 0)" /></template>
        <template #nstore="{ record: n }"><MonCell :on="!!(n.status === 'online' && n.metrics)" mode="bars" :pct="pct(n.metrics?.diskUsed, n.metrics?.diskTotal)" :text="pct(n.metrics?.diskUsed, n.metrics?.diskTotal) + '%'" :sub="fmtBytes(n.metrics?.diskUsed) + ' / ' + fmtBytes(n.metrics?.diskTotal)" /></template>
        <template #ndisks="{ record: n }">
          <div v-if="n.status === 'online' && n.metrics && n.disks.length" class="lines" style="font-size:12px">
            <div v-for="d in n.disks" :key="d.dev"><span class="mono">{{ d.dev }}</span>
              <a-tag size="small" :color="busy(d.util, d.await).tag" style="margin-left:4px">{{ busy(d.util, d.await).text }} {{ num(d.util) }}%</a-tag>
              <span class="muted mono" style="margin-left:4px">{{ fmtK(d.rIops + d.wIops) }} IOPS · {{ num(d.await) }} ms</span></div>
            <div class="muted">IO 等待 {{ num(n.metrics.iowait) }}%</div>
          </div><span v-else class="muted">-</span>
        </template>
        <template #nnet="{ record: n }"><div v-if="n.status === 'online' && n.metrics" class="updown"><div class="d"><icon-arrow-down /> {{ fmtRate(n.metrics.netIn) }}</div><div class="u"><icon-arrow-up /> {{ fmtRate(n.metrics.netOut) }}</div></div><span v-else class="muted">-</span></template>
        <template #nconns="{ record: n }"><ConnCell v-if="n.status === 'online' && n.metrics" :tcp="n.connsTcp" :udp="n.connsUdp" /><span v-else class="muted">-</span></template>
        <template #nload="{ record: n }"><span v-if="n.status === 'online' && n.metrics" class="mono">{{ num(n.metrics.load[0]) }}</span><span v-else class="muted">-</span></template>
        <template #nvms="{ record: n }"><span><b>{{ data.vms.filter(v => v.nodeId === n.id && v.status === 'running').length }}</b> / {{ data.vms.filter(v => v.nodeId === n.id).length }}</span></template>
      </a-table>
    </a-card>

    <!-- 虚拟机与容器 -->
    <a-card v-if="!pub || page.showVms" class="general-card" :title="admin || pub ? '虚拟机与容器' : '我的虚拟机'">
      <template #extra>
        <a-space v-if="!pub">
          <span class="muted">更新于 {{ updatedText }}</span>
          <a-badge v-if="refresh === -1" :status="wsState === 'open' ? 'processing' : 'warning'" :text="wsState === 'open' ? '实时' : '正在连接…'" />
          <a-select v-model="refresh" size="small" style="width:170px" :options="[{ label: '实时推送', value: -1 }, { label: '自动刷新 10s', value: 10 }, { label: '自动刷新 30s', value: 30 }, { label: '自动刷新 60s', value: 60 }, { label: '暂停刷新', value: 0 }]" />
        </a-space>
      </template>
      <a-row justify="space-between" align="center" style="margin-bottom:16px" :gutter="8">
        <a-space wrap>
          <a-input-search v-model="kw" placeholder="名称 / VMID / IP" style="width:200px" allow-clear />
          <a-select v-if="nodeOptions.length > 1" v-model="nodeFilter" style="width:140px" :options="[{ label: '全部节点', value: 0 }, ...nodeOptions]" />
          <a-radio-group v-model="typeFilter" type="button"><a-radio value="">全部</a-radio><a-radio value="qemu">VM</a-radio><a-radio value="lxc">CT</a-radio></a-radio-group>
          <a-radio-group v-model="statusFilter" type="button"><a-radio value="">全部状态</a-radio><a-radio value="running">运行中</a-radio><a-radio value="stopped">已停止</a-radio></a-radio-group>
        </a-space>
        <a-space>
          <span class="muted">显示</span>
          <a-radio-group v-model="mode" type="button" size="small"><a-radio value="bars">进度条</a-radio><a-radio value="trend">趋势</a-radio></a-radio-group>
          <template v-if="nodeOptions.length > 1"><span class="muted" style="margin-left:8px">按节点分组</span><a-switch v-model="grouped" size="small" /></template>
        </a-space>
      </a-row>
      <a-table row-key="key" :data="rows" :columns="columns" :pagination="false" :bordered="false" :loading="loading && !data.updated"
               v-model:expanded-keys="expanded" :indent-size="0" :span-method="span" :row-class="r => (r.group ? 'grp-row' : 'mon-row')"
               :scroll="pub ? undefined : { x: columns.reduce((t, c) => t + c.width, 0) }" :class="{ 'pub-table': pub }" @row-click="r => !r.group && (!pub || page.showDetail) && openVM(r)">
        <template #name="{ record }">
          <div v-if="record.group" class="grp">
            <icon-cloud /><span class="nm">{{ record.node.name }}</span>
            <a-badge :status="nodeBadge[record.node.status]?.badge" :text="nodeBadge[record.node.status]?.text" />
            <div class="info"><span>运行 <b>{{ record.children.filter(v => v.status === 'running').length }}</b> / {{ record.children.length }}</span></div>
          </div>
          <template v-else><span class="circle" :class="{ pass: record.status === 'running' }"></span><b style="font-weight:500">{{ record.name }}</b> <span class="muted mono">{{ record.vmid }}</span></template>
        </template>
        <template #type="{ record }"><a-tag size="small" :color="record.type === 'qemu' ? 'arcoblue' : 'cyan'">{{ record.type === 'qemu' ? 'VM' : 'CT' }}</a-tag></template>
        <template #owner="{ record }"><a-tag v-if="record.shared" size="small" color="arcoblue">公共</a-tag><span v-else-if="record.owner">{{ record.owner }}</span><span v-else class="muted">-</span></template>
        <template #uptime="{ record }"><span :class="{ muted: record.status !== 'running' }">{{ record.status === 'running' ? fmtUptime(record.uptime) : '-' }}</span></template>
        <template #cpu="{ record }"><MonCell :on="record.status === 'running'" :mode="mode" :pct="record.cpu" :text="num(record.cpu) + '%'" :sub="record.cores + ' 核'" :history="hist('v' + record.id, 'cpu')" /></template>
        <template #mem="{ record }"><MonCell :on="record.status === 'running'" :mode="mode" :pct="pct(record.memUsed, record.memTotal)" :text="pct(record.memUsed, record.memTotal) + '%'" :sub="fmtBytes(record.memUsed) + ' / ' + fmtBytes(record.memTotal, 0)" :history="hist('v' + record.id, 'mem')" /></template>
        <template #disk="{ record }"><div class="disk-cell">
          <div class="dc-size">
            <template v-if="record.diskTotal && record.type === 'lxc' && record.status !== 'offline'">
              <div>{{ pct(record.diskUsed, record.diskTotal) }}%</div><div class="muted">{{ fmtBytes(record.diskUsed) }} / {{ fmtBytes(record.diskTotal, 0) }}</div></template>
            <template v-else-if="record.diskTotal"><div>{{ fmtBytes(record.diskTotal, 0) }}</div><div class="muted">容量</div></template>
            <span v-else class="muted">-</span>
          </div>
          <a-tooltip v-if="record.status === 'running'" :content="`IO 压力 ${num(record.ioPressure)}%（等待磁盘 IO 的时间占比）`">
            <div class="dc-io">
              <div class="dc-rw"><div>读 {{ fmtRate(record.diskR) }}</div><div>写 {{ fmtRate(record.diskW) }}</div></div>
              <div class="dc-st"><a-tag size="small" :color="vmBusy(record).tag">{{ vmBusy(record).text }}</a-tag><span class="mono muted">{{ fmtK(record.iops) }} IOPS</span></div>
            </div>
          </a-tooltip>
        </div></template>
        <template #net="{ record }"><div v-if="record.status === 'running'" class="updown"><div class="d"><icon-arrow-down /> {{ fmtRate(record.netIn) }}</div><div class="u"><icon-arrow-up /> {{ fmtRate(record.netOut) }}</div></div><span v-else class="muted">-</span></template>
        <template #fwd="{ record }"><a-tooltip content="近 24 小时经转发规则的流量：上传为客户端 → 虚拟机，下载为虚拟机 → 客户端">
          <div class="updown"><div class="u"><icon-upload /> {{ fmtBytes(record.up24) }}</div><div class="d"><icon-download /> {{ fmtBytes(record.down24) }}</div></div></a-tooltip></template>
        <template #conns="{ record }"><ConnCell v-if="record.status === 'running'" :tcp="record.connsTcp" :udp="record.connsUdp" /><span v-else class="muted">-</span></template>
        <template #ip="{ record }"><div class="lines"><div class="mono">{{ record.ipv4 || '-' }}</div><div v-if="record.ipv6" class="mono muted">{{ record.ipv6 }}</div></div></template>
        <template #empty><a-empty :description="data.updated ? '没有虚拟机' : '加载中…'" /></template>
      </a-table>
    </a-card>

    <a-modal v-model:visible="drawer" width="90vw" :footer="false" unmount-on-close class="vm-detail-modal" :body-style="{ height: 'calc(90vh - 96px)', overflow: 'auto' }">
      <template #title><a-space><span class="circle" :class="{ pass: vm.status === 'running' }"></span>{{ vm.name }} <span class="muted mono">{{ vm.nodeName }}/{{ vm.vmid }}</span></a-space></template>
      <a-descriptions :column="{ xs: 1, md: 3, xl: 4 }" size="small" :data="vmRows" />
      <template v-if="showTraffic">
        <a-divider />
        <a-row justify="space-between" align="center" style="margin-bottom:8px">
          <b>转发流量趋势</b>
          <a-space :size="16">
            <span class="muted">合计 <icon-upload /> 上传 {{ fmtBytes(vmTraffic.up) }} · <icon-download /> 下载 {{ fmtBytes(vmTraffic.down) }}</span>
            <a-radio-group v-model="trafficRange" type="button" size="small" @change="loadTraffic"><a-radio value="24h">近 24 小时</a-radio><a-radio value="7d">近 7 天</a-radio></a-radio-group>
          </a-space>
        </a-row>
        <div class="chart-legend"><span><i style="background:#165dff"></i>上传（客户端 → 虚拟机）</span><span><i style="background:#ff7d00"></i>下载（虚拟机 → 客户端）</span></div>
        <a-spin :loading="trafficLoading" style="width:100%">
          <TrafficChart :series="vmTraffic.series" :step="vmTraffic.step" :height="260" />
        </a-spin>
      </template>
      <a-divider />
      <a-row justify="space-between" style="margin-bottom:12px"><b>资源历史</b>
        <a-radio-group v-model="tf" type="button" size="small" @change="loadRRD"><a-radio value="hour">1 小时</a-radio><a-radio value="day">24 小时</a-radio><a-radio value="week">7 天</a-radio></a-radio-group></a-row>
      <a-spin :loading="rrdLoading" style="width:100%">
        <a-alert v-if="rrdErr" type="warning" style="margin-bottom:12px">{{ rrdErr }}</a-alert>
        <a-grid :cols="{ xs: 1, md: 2, xl: 4 }" :col-gap="12" :row-gap="12">
          <a-grid-item v-for="c in charts" :key="c.title">
            <div class="mini-chart">
              <div class="mc-head"><span>{{ c.title }}</span><b style="font-weight:500">{{ c.now }}</b></div>
              <Spark :series="c.series" :w="300" :h="110" width="100%" fill :max="c.max" :format="c.format" :labels="rrdLabels" />
              <div class="muted" style="display:flex;justify-content:space-between"><span>{{ { hour: '1 小时前', day: '24 小时前', week: '7 天前' }[tf] }}</span><span>{{ c.legend }}</span><span>现在</span></div>
            </div>
          </a-grid-item>
        </a-grid>
      </a-spin>
    </a-modal>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { api, isAdmin, nodeBadge, fmtBytes, fmtRate, fmtK, fmtUptime, fmtAgo, pct, lvl, num, bytesParts } from '../api'
import Spark from '../components/Spark.vue'
import MonCell from '../components/MonCell.vue'
import TrafficChart from '../components/TrafficChart.vue'
import ConnCell from '../components/ConnCell.vue'

const props = defineProps({ slug: String })
const pub = computed(() => !!props.slug)
const admin = computed(() => !pub.value && isAdmin())
const data = ref({ nodes: [], vms: [], updated: 0, thresholds: { busyUtil: 70, satUtil: 90, busyAwait: 20, satAwait: 40 } })
const page = ref({ title: '', desc: '', refresh: 30, showVms: true, showDetail: true, showIp: false, showOwner: false })
const th = computed(() => data.value.thresholds)
const loading = ref(false)
const gate = ref('')
const gateTitle = ref('状态页')
const pwd = ref('')
const pwdErr = ref('')
// 刷新方式：-1 为实时推送（WebSocket，每次采集完成即推送），正数为定时刷新的秒数，0 为暂停
const stored = Number(localStorage.getItem('pf-monitor-refresh') ?? 30)
const refresh = ref(stored === 3 ? -1 : stored)
const wsState = ref('')
// 节点显示方式：列表（默认）或方块
const nodeView = ref(localStorage.getItem('pf-monitor-nodeview') || 'list')
watch(nodeView, v => localStorage.setItem('pf-monitor-nodeview', v))
const nodeColumns = computed(() => {
  const cols = [
    { title: '节点', slotName: 'nname', width: 190 },
    { title: 'CPU', slotName: 'ncpu', width: 190 },
    { title: '内存', slotName: 'nmem', width: 200 },
    { title: '存储', slotName: 'nstore', width: 210 },
    { title: '磁盘', slotName: 'ndisks', width: 320 },
    { title: '网络', slotName: 'nnet', width: 120 },
    { title: '连接数', slotName: 'nconns', width: 96 },
    { title: '负载', slotName: 'nload', width: 70 },
    { title: '虚拟机', slotName: 'nvms', width: 80 },
  ]
  return pub.value ? cols.map(({ width, ...c }) => c) : cols
})
const kw = ref('')
const nodeFilter = ref(0)
const typeFilter = ref('')
const statusFilter = ref('')
const mode = ref('bars')
const grouped = ref(true)
const expanded = ref([])

// 客户端保留最近 30 次采样，用于卡片与表格中的趋势线
const history = new Map()
const histTick = ref(0)
function push(key, metric, v) {
  const k = key + ':' + metric
  const a = history.get(k) || []
  a.push(v)
  if (a.length > 30) a.shift()
  history.set(k, a)
}
const hist = (key, metric) => (histTick.value, history.get(key + ':' + metric) || [])

// 应用一份监控数据（定时刷新与实时推送共用）
function apply(d) {
  data.value = d
  for (const n of d.nodes) if (n.metrics) {
    push('n' + n.id, 'cpu', n.metrics.cpu); push('n' + n.id, 'mem', pct(n.metrics.memUsed, n.metrics.memTotal)); push('n' + n.id, 'iow', n.metrics.iowait)
  }
  for (const v of d.vms) if (v.status === 'running') { push('v' + v.id, 'cpu', v.cpu); push('v' + v.id, 'mem', pct(v.memUsed, v.memTotal)) }
  histTick.value++
  if (!expanded.value.length) expanded.value = [...new Set(d.vms.map(v => 'n' + v.nodeId))]
}

async function load() {
  loading.value = true
  try {
    let d
    if (pub.value) {
      const headers = {}
      const saved = sessionStorage.getItem('pf-pub-' + props.slug)
      if (saved) headers['X-Access-Password'] = saved
      try {
        d = await api.get(`/api/public/${props.slug}`, { headers, silent: true, allow401: true })
      } catch (e) {
        if (e.status === 401) { gate.value = 'password'; if (saved) { sessionStorage.removeItem('pf-pub-' + props.slug); pwdErr.value = e.message } }
        else if (e.status === 404) gate.value = 'closed'
        return
      }
      gate.value = ''
      page.value = d.page
      document.title = d.page.title
      refresh.value = d.page.refresh
    } else {
      // 刷新间隔告诉控制端：页面打开期间节点按该间隔采集，数据才会随之更新
      d = await api.get(`/api/monitor?refresh=${Math.max(refresh.value, 0)}`, { silent: true })
    }
    apply(d)
  } catch { /* 静默，下次重试 */ } finally { loading.value = false }
}
async function unlock() {
  if (!pwd.value) return
  sessionStorage.setItem('pf-pub-' + props.slug, pwd.value)
  pwdErr.value = ''
  await load()
}

let timer, ws, wsRetry
// 实时推送：断开后 3 秒重连；连接期间节点按 2 秒采集，每次采集完成推送
function openWS() {
  closeWS()
  wsState.value = 'connecting'
  ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/monitor/ws?every=2`)
  ws.onopen = () => { wsState.value = 'open' }
  ws.onmessage = e => { try { apply(JSON.parse(e.data)) } catch { /* 忽略 */ } }
  ws.onclose = () => {
    wsState.value = 'closed'
    if (refresh.value === -1) wsRetry = setTimeout(openWS, 3000)
  }
}
function closeWS() {
  clearTimeout(wsRetry)
  if (ws) { ws.onclose = null; ws.close(); ws = null }
}
function schedule() {
  clearInterval(timer)
  closeWS()
  if (refresh.value === -1 && !pub.value) openWS()
  else if (refresh.value > 0) timer = setInterval(load, refresh.value * 1000)
}
watch(refresh, v => { if (!pub.value) localStorage.setItem('pf-monitor-refresh', v); schedule(); if (v > 0) load() })
onMounted(async () => { await load(); schedule() })
onUnmounted(() => { clearInterval(timer); closeWS() })

const updatedText = computed(() => (data.value.updated ? new Date(data.value.updated * 1000).toLocaleTimeString() : '-'))
const allOnline = computed(() => (data.value.nodes.length ? data.value.nodes : data.value.nodeNames || []).every(n => n.status === 'online'))
const nodeOptions = computed(() => {
  const m = new Map(data.value.vms.map(v => [v.nodeId, v.nodeName]))
  for (const n of data.value.nodes) m.set(n.id, n.name)
  return [...m].map(([value, label]) => ({ value, label }))
})

// 磁盘繁忙度分级
function busy(util, aw = 0) {
  const t = th.value
  if (util >= t.satUtil || aw >= t.satAwait) return { text: '饱和', tag: 'red', color: '#f53f3f' }
  if (util >= t.busyUtil || aw >= t.busyAwait) return { text: '繁忙', tag: 'orange', color: '#ff7d00' }
  if (util >= 30) return { text: '正常', tag: 'arcoblue', color: '#165dff' }
  return { text: '空闲', tag: 'green', color: '#00b42a' }
}
// 虚拟机没有 %util，按 cgroup IO 压力（等待 IO 的时间占比）判断
const vmBusy = v => (v.ioPressure >= 50 ? busy(100) : v.ioPressure >= 20 ? busy(th.value.busyUtil) : v.iops >= 50 || v.ioPressure >= 2 ? busy(30) : busy(0))
const iowaitColor = w => (w >= 20 ? '#f53f3f' : w >= 10 ? '#ff7d00' : '#00b42a')

const summary = computed(() => {
  const vms = data.value.vms
  const run = vms.filter(v => v.status === 'running')
  const on = data.value.nodes.filter(n => n.status === 'online' && n.metrics)
  if (data.value.nodes.length) {
    const avg = on.length ? num(on.reduce((s, n) => s + n.metrics.cpu, 0) / on.length) : 0
    const mu = on.reduce((s, n) => s + n.metrics.memUsed, 0), mt = on.reduce((s, n) => s + n.metrics.memTotal, 0)
    const [muV, muU] = bytesParts(mu)
    const disks = on.flatMap(n => n.disks)
    const busyDisks = disks.filter(d => ['繁忙', '饱和'].includes(busy(d.util, d.await).text))
    return [
      { title: '在线节点', value: on.length, unit: '/ ' + data.value.nodes.length, sub: '离线 ' + data.value.nodes.filter(n => n.status !== 'online').length, color: 'rgb(var(--green-6))' },
      { title: '运行中', value: run.length, unit: '/ ' + vms.length + ' 台', sub: `VM ${run.filter(v => v.type === 'qemu').length} · CT ${run.filter(v => v.type === 'lxc').length}` },
      { title: '平均 CPU', value: avg, unit: '%', sub: '在线节点平均', color: lvl(avg) },
      { title: '内存使用', value: muV, unit: muU + ' / ' + fmtBytes(mt), sub: pct(mu, mt) + '%', color: lvl(pct(mu, mt)) },
      { title: '当前连接', value: on.reduce((t, n) => t + (n.conns || 0), 0), unit: '个', sub: `TCP ${on.reduce((t, n) => t + (n.connsTcp || 0), 0)} · UDP ${on.reduce((t, n) => t + (n.connsUdp || 0), 0)}`, color: 'rgb(var(--arcoblue-6))' },
      { title: '磁盘繁忙', value: busyDisks.length, unit: '/ ' + disks.length + ' 块', sub: '最高 IO 等待 ' + num(Math.max(0, ...on.map(n => n.metrics.iowait))) + '%', color: busyDisks.length ? '#ff7d00' : undefined },
    ]
  }
  return [
    { title: '虚拟机', value: vms.length, unit: '台', sub: '运行中 ' + run.length },
    { title: 'CPU 合计', value: run.reduce((s, v) => s + (v.cpu * v.cores) / 100, 0), unit: '核', sub: '共 ' + vms.reduce((s, v) => s + v.cores, 0) + ' 核' },
    { title: '内存', value: bytesParts(run.reduce((s, v) => s + v.memUsed, 0))[0], unit: bytesParts(run.reduce((s, v) => s + v.memUsed, 0))[1], sub: '共 ' + fmtBytes(vms.reduce((s, v) => s + v.memTotal, 0)) },
    { title: '网络出向', value: bytesParts(run.reduce((s, v) => s + v.netOut, 0))[0], unit: bytesParts(run.reduce((s, v) => s + v.netOut, 0))[1] + '/s', sub: '入向 ' + fmtRate(run.reduce((s, v) => s + v.netIn, 0)) },
    { title: '当前连接', value: run.reduce((t, v) => t + (v.conns || 0), 0), unit: '个', sub: `TCP ${run.reduce((t, v) => t + (v.connsTcp || 0), 0)} · UDP ${run.reduce((t, v) => t + (v.connsUdp || 0), 0)}`, color: 'rgb(var(--arcoblue-6))' },
    { title: '磁盘繁忙', value: run.filter(v => ['繁忙', '饱和'].includes(vmBusy(v).text)).length, unit: '台', sub: '按 IO 压力判断', color: '#ff7d00' },
  ]
})

const filteredVMs = computed(() => data.value.vms.filter(v => (!nodeFilter.value || v.nodeId === nodeFilter.value) && (!typeFilter.value || v.type === typeFilter.value) &&
  (!statusFilter.value || v.status === statusFilter.value) && (!kw.value || `${v.vmid} ${v.name} ${v.ipv4 || ''}`.toLowerCase().includes(kw.value.toLowerCase()))))
const rows = computed(() => {
  const list = filteredVMs.value.map(v => ({ ...v, key: 'v' + v.id }))
  if (!grouped.value || nodeOptions.value.length < 2) return list
  return nodeOptions.value.map(o => {
    const n = data.value.nodes.find(x => x.id === o.value) || data.value.nodeNames?.find(x => x.id === o.value) || { id: o.value, name: o.label, status: 'online' }
    return { key: 'n' + o.value, group: true, node: n, children: list.filter(v => v.nodeId === o.value) }
  }).filter(g => g.children.length)
})
const showOwner = computed(() => !pub.value ? admin.value : page.value.showOwner)
const showIp = computed(() => !pub.value || page.value.showIp)
const columns = computed(() => (pub.value ? allColumns.value.map(({ width, ...c }) => c) : allColumns.value))
const allColumns = computed(() => [
  { title: '名称', slotName: 'name', width: 200, dataIndex: 'name', sortable: { sortDirections: ['ascend', 'descend'] } },
  { title: '类型', slotName: 'type', width: 64 },
  ...(showOwner.value ? [{ title: '归属', slotName: 'owner', width: 90 }] : []),
  { title: '运行时长', slotName: 'uptime', width: 110 },
  { title: 'CPU', slotName: 'cpu', width: 180, dataIndex: 'cpu', sortable: { sortDirections: ['descend', 'ascend'] } },
  { title: '内存', slotName: 'mem', width: 200, dataIndex: 'memUsed', sortable: { sortDirections: ['descend', 'ascend'] } },
  { title: '磁盘 / IO', slotName: 'disk', width: 340, dataIndex: 'iops', sortable: { sortDirections: ['descend', 'ascend'] } },
  { title: '网络', slotName: 'net', width: 120 },
  ...(showTraffic.value ? [{ title: '转发流量（24h）', slotName: 'fwd', width: 140, dataIndex: 'down24', sortable: { sortDirections: ['descend', 'ascend'] } }] : []),
  { title: '连接数', slotName: 'conns', width: 110, dataIndex: 'conns', sortable: { sortDirections: ['descend', 'ascend'] } },
  ...(showIp.value ? [{ title: 'IP', slotName: 'ip', width: 150 }] : []),
])
// a-statistic 需要指定小数位数：按数值实际的小数位（最多两位）显示
const decimals = v => Math.min((String(v).split('.')[1] || '').length, 2)
const span = ({ record, columnIndex }) => (record.group ? (columnIndex === 0 ? { rowspan: 1, colspan: columns.value.length } : { rowspan: 0, colspan: 0 }) : undefined)

// ---- 详情抽屉 ----
const drawer = ref(false)
const vm = ref({})
const tf = ref('hour')
const rrd = ref([])
const rrdLoading = ref(false)
const rrdErr = ref('')
function openVM(v) { vm.value = v; drawer.value = true; loadRRD(); if (showTraffic.value) loadTraffic() }
// 转发流量（登录用户可见，公开页不显示）
const showTraffic = computed(() => !pub.value)
const trafficRange = ref('24h')
const trafficLoading = ref(false)
const vmTraffic = ref({ series: [], step: 3600, up: 0, down: 0 })
async function loadTraffic() {
  trafficLoading.value = true
  try {
    vmTraffic.value = await api.get(`/api/monitor/traffic?vm=${vm.value.id}&range=${trafficRange.value}&tz=${-new Date().getTimezoneOffset()}`, { silent: true })
  } catch { vmTraffic.value = { series: [], step: 3600, up: 0, down: 0 } } finally { trafficLoading.value = false }
}
async function loadRRD() {
  rrdLoading.value = true
  rrdErr.value = ''
  try {
    const url = pub.value ? `/api/public/${props.slug}/rrd?vm=${vm.value.id}&tf=${tf.value}` : `/api/monitor/rrd?vm=${vm.value.id}&tf=${tf.value}`
    const headers = pub.value ? { 'X-Access-Password': sessionStorage.getItem('pf-pub-' + props.slug) || '' } : {}
    rrd.value = await api.get(url, { headers, silent: true })
  } catch (e) { rrd.value = []; rrdErr.value = e.message } finally { rrdLoading.value = false }
}
const vmRows = computed(() => {
  const v = vm.value
  const r = [
    { label: '类型', value: v.type === 'qemu' ? '虚拟机 (VM)' : '容器 (CT)' }, { label: '状态', value: v.status === 'running' ? '运行中 · ' + fmtUptime(v.uptime) : v.status === 'offline' ? '节点离线，状态未知' : '已停止' },
    { label: '配置', value: `${v.cores} 核 / ${fmtBytes(v.memTotal, 0)} / ${fmtBytes(v.diskTotal, 0)}` },
  ]
  if (showOwner.value) r.push({ label: '归属', value: v.shared ? '公共' : v.owner || '-' })
  if (showIp.value) r.push({ label: 'IP', value: [v.ipv4, v.ipv6].filter(Boolean).join(' / ') || '-' })
  if (v.status === 'running') r.push({ label: '磁盘 IO', value: `${vmBusy(v).text} · ${fmtK(v.iops)} IOPS · 压力 ${num(v.ioPressure)}%` })
  if (v.status === 'running') r.push({ label: '当前连接', value: `TCP ${v.connsTcp || 0} · UDP ${v.connsUdp || 0}` })
  if (showTraffic.value) r.push({ label: '近 24 小时转发', value: `上传 ${fmtBytes(v.up24)} · 下载 ${fmtBytes(v.down24)}` })
  return r
})
const col = k => rrd.value.map(p => p[k] || 0)
const rrdLabels = computed(() => rrd.value.map(p => (p.time ? new Date(p.time * 1000).toLocaleString([], { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '')))
const charts = computed(() => {
  const v = vm.value
  const last = rrd.value.length ? rrd.value[rrd.value.length - 1] : {}
  return [
    { title: 'CPU', now: ((last.cpu || 0) * 100).toFixed(1) + '%', series: [{ values: col('cpu').map(x => x * 100), color: '#165dff' }], legend: v.cores + ' 核', max: 100, format: x => num(x) + '%' },
    { title: '内存', now: fmtBytes(last.mem) + ' / ' + fmtBytes(last.maxmem || v.memTotal, 0), series: [{ values: col('mem'), color: '#722ed1' }], legend: '已用', max: last.maxmem || v.memTotal, format: x => fmtBytes(x) },
    { title: '网络', now: `↓ ${fmtRate(last.netin)}  ↑ ${fmtRate(last.netout)}`, series: [{ name: '入', values: col('netin'), color: '#00b42a' }, { name: '出', values: col('netout'), color: '#ff7d00' }], legend: '入 / 出', format: fmtRate },
    { title: '磁盘 IO', now: `读 ${fmtRate(last.diskread)}  写 ${fmtRate(last.diskwrite)}`, series: [{ name: '读', values: col('diskread'), color: '#14c9c9' }, { name: '写', values: col('diskwrite'), color: '#f53f3f' }], legend: '读 / 写', format: fmtRate },
  ]
})
</script>
