<template>
  <div ref="box" class="traffic-chart" @mouseleave="hover = -1">
    <svg :width="W" :height="H" :viewBox="`0 0 ${W} ${H}`" style="display:block" @mousemove="onMove">
      <defs>
        <linearGradient id="tcUp" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#165dff" stop-opacity=".18" /><stop offset="100%" stop-color="#165dff" stop-opacity="0" /></linearGradient>
        <linearGradient id="tcDown" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#ff7d00" stop-opacity=".14" /><stop offset="100%" stop-color="#ff7d00" stop-opacity="0" /></linearGradient>
      </defs>
      <g v-for="t in ticks" :key="t.v">
        <line :x1="L" :x2="W - R" :y1="y(t.v)" :y2="y(t.v)" stroke="#e5e6eb" :stroke-dasharray="t.v ? '3 3' : undefined" />
        <text :x="L - 8" :y="y(t.v) + 4" text-anchor="end" font-size="12" fill="#86909c">{{ t.label }}</text>
      </g>
      <text v-for="l in xLabels" :key="l.i" :x="x(l.i)" :y="H - 8" :text-anchor="l.anchor" font-size="12" fill="#86909c">{{ l.label }}</text>
      <path :d="area(up)" fill="url(#tcUp)" /><path :d="area(down)" fill="url(#tcDown)" />
      <path :d="line(up)" fill="none" stroke="#165dff" stroke-width="2" stroke-linejoin="round" />
      <path :d="line(down)" fill="none" stroke="#ff7d00" stroke-width="2" stroke-linejoin="round" />
      <template v-if="hover >= 0">
        <line :x1="x(hover)" :x2="x(hover)" :y1="T" :y2="H - B" stroke="#c9cdd4" />
        <circle :cx="x(hover)" :cy="y(up[hover])" r="4" fill="#fff" stroke="#165dff" stroke-width="2" />
        <circle :cx="x(hover)" :cy="y(down[hover])" r="4" fill="#fff" stroke="#ff7d00" stroke-width="2" />
      </template>
    </svg>
    <div v-if="hover >= 0" class="tc-tip" :style="tipStyle">
      <div class="tc-tip-title">{{ timeLabel(hover, true) }}</div>
      <div><i style="background:#165dff"></i>上行<b>{{ fmtBytes(up[hover]) }}</b></div>
      <div><i style="background:#ff7d00"></i>下行<b>{{ fmtBytes(down[hover]) }}</b></div>
    </div>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { fmtBytes, num } from '../api'

// series：[{ t: 起始时间（秒）, up, down }]；step：每个点的时长（3600 或 86400 秒）
const props = defineProps({ series: { type: Array, default: () => [] }, step: { type: Number, default: 3600 }, height: { type: Number, default: 280 } })

const box = ref(null)
const W = ref(800) // 按容器实际宽度绘制，文字不会被拉伸
const H = computed(() => props.height)
const L = 72, R = 24, T = 12, B = 30
const hover = ref(-1)

let ro
onMounted(() => {
  ro = new ResizeObserver(([e]) => { W.value = Math.max(320, Math.floor(e.contentRect.width)) })
  ro.observe(box.value)
})
onUnmounted(() => ro?.disconnect())

const up = computed(() => props.series.map(p => p.up || 0))
const down = computed(() => props.series.map(p => p.down || 0))
const n = computed(() => Math.max(props.series.length, 2))

// 纵轴：按数据选择单位，刻度取 1 / 2 / 2.5 / 5 × 10ⁿ 的整齐数值
const scale = computed(() => {
  const peak = Math.max(0, ...up.value, ...down.value)
  const units = [['KB', 1024], ['MB', 1024 ** 2], ['GB', 1024 ** 3], ['TB', 1024 ** 4]]
  let [unit, size] = units[0]
  for (const u of units) if (peak >= u[1]) [unit, size] = u
  const raw = Math.max(peak / size, 1) / 4
  const mag = 10 ** Math.floor(Math.log10(raw))
  const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(s => s >= raw)
  const count = Math.max(1, Math.ceil(peak / size / step))
  return { max: step * count * size, ticks: Array.from({ length: count + 1 }, (_, k) => ({ v: k * step * size, label: `${num(k * step)} ${unit}` })) }
})
const ticks = computed(() => scale.value.ticks)

const x = i => L + (i * (W.value - L - R)) / (n.value - 1)
const y = v => H.value - B - ((v || 0) / scale.value.max) * (H.value - T - B)
const line = vs => vs.map((v, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join(' ')
const area = vs => (vs.length ? `${line(vs)} L${x(vs.length - 1).toFixed(1)} ${H.value - B} L${x(0).toFixed(1)} ${H.value - B} Z` : '')

function timeLabel(i, full) {
  const p = props.series[i]
  if (!p) return ''
  const d = new Date(p.t * 1000)
  const pad = v => String(v).padStart(2, '0')
  const day = `${d.getMonth() + 1}-${pad(d.getDate())}`
  if (props.step >= 86400) return day
  const h = `${pad(d.getHours())}:00`
  return full ? `${day} ${h} – ${pad((d.getHours() + 1) % 24)}:00` : h
}

// 横轴：按宽度决定标签间隔；首尾标签贴边对齐，避免被裁掉
const xLabels = computed(() => {
  const len = props.series.length
  if (!len) return []
  const every = Math.max(1, Math.ceil(len / Math.max(2, Math.floor((W.value - L - R) / 64))))
  const out = []
  for (let i = 0; i < len; i += every) out.push(i)
  // 最后一个点与前一个标签距离太近时，用最后一个点替换它，避免文字重叠
  if (out[out.length - 1] !== len - 1) {
    if (x(len - 1) - x(out[out.length - 1]) >= 64) out.push(len - 1)
    else out[out.length - 1] = len - 1
  }
  return out.map(i => ({ i, label: timeLabel(i), anchor: i === 0 ? 'start' : i === len - 1 ? 'end' : 'middle' }))
})

function onMove(e) {
  const rect = e.currentTarget.getBoundingClientRect()
  const px = e.clientX - rect.left
  if (px < L - 10 || px > W.value - R + 10 || !props.series.length) { hover.value = -1; return }
  hover.value = Math.min(props.series.length - 1, Math.max(0, Math.round(((px - L) / (W.value - L - R)) * (n.value - 1))))
}
const tipStyle = computed(() => {
  const px = x(hover.value)
  return px > W.value - 200 ? { right: `${W.value - px + 12}px`, top: '16px' } : { left: `${px + 12}px`, top: '16px' }
})
</script>

<style scoped>
.traffic-chart { position: relative; width: 100%; }
.tc-tip { position: absolute; pointer-events: none; background: var(--color-bg-popup); border-radius: 4px; box-shadow: 0 4px 10px rgb(0 0 0 / 12%); padding: 8px 12px; font-size: 12px; line-height: 1.9; min-width: 150px; white-space: nowrap; }
.tc-tip-title { color: var(--color-text-1); font-weight: 500; }
.tc-tip i { display: inline-block; width: 8px; height: 8px; border-radius: 2px; margin-right: 6px; }
.tc-tip b { float: right; margin-left: 16px; font-weight: 500; color: var(--color-text-1); }
</style>
