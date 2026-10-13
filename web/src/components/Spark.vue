<template>
  <div ref="box" class="spark-box" :style="{ width: width || w + 'px' }" @mousemove="onMove" @mouseleave="hover = -1">
    <svg :viewBox="`0 0 ${w} ${h}`" preserveAspectRatio="none" :style="{ width: '100%', height: h + 'px', display: 'block' }">
      <template v-for="(s, i) in series" :key="i">
        <path v-if="fill && i === 0" :d="path(s.values) + ` L${w} ${h} L0 ${h} Z`" :fill="s.color" fill-opacity=".1" />
        <path :d="path(s.values)" fill="none" :stroke="s.color" stroke-width="1.5" vector-effect="non-scaling-stroke" />
      </template>
      <line v-if="hover >= 0" :x1="x(hover)" :x2="x(hover)" y1="0" :y2="h" stroke="#c9cdd4" vector-effect="non-scaling-stroke" />
    </svg>
    <!-- 悬停数值：format 为空时不显示 -->
    <div v-if="hover >= 0 && format" class="spark-tip" :style="tipStyle">
      <div v-if="labels?.[hover]" class="spark-tip-t">{{ labels[hover] }}</div>
      <div v-for="(s, i) in series" :key="i"><i :style="{ background: s.color }"></i>{{ s.name ? s.name + ' ' : '' }}<b>{{ format(s.values?.[hover] ?? 0) }}</b></div>
    </div>
  </div>
</template>

<script setup>
import { computed, ref } from 'vue'

// labels：每个点的时间说明（可选）；format：悬停时的数值格式（不传则不显示提示）
const props = defineProps({ series: Array, w: { type: Number, default: 60 }, h: { type: Number, default: 20 }, width: String, fill: Boolean, max: Number,
  format: Function, labels: Array })
const box = ref(null)
const hover = ref(-1)
const len = computed(() => Math.max(0, ...props.series.map(s => s.values?.length || 0)))
const x = i => (len.value > 1 ? (i * props.w) / (len.value - 1) : 0)

function path(vals) {
  if (!vals || vals.length < 2) return `M0 ${props.h - 1} L${props.w} ${props.h - 1}`
  const all = props.series.flatMap(s => s.values || [])
  const max = props.max || Math.max(1e-9, ...all)
  const step = props.w / (vals.length - 1)
  return vals.map((v, i) => `${i ? 'L' : 'M'}${(i * step).toFixed(1)} ${(props.h - 1 - (Math.max(0, v || 0) / max) * (props.h - 3)).toFixed(1)}`).join(' ')
}
function onMove(e) {
  if (!props.format || len.value < 2) return
  const r = box.value.getBoundingClientRect()
  hover.value = Math.min(len.value - 1, Math.max(0, Math.round(((e.clientX - r.left) / r.width) * (len.value - 1))))
}
const tipStyle = computed(() => {
  const pct = len.value > 1 ? hover.value / (len.value - 1) : 0
  return pct > 0.6 ? { right: `${(1 - pct) * 100}%`, marginRight: '8px' } : { left: `${pct * 100}%`, marginLeft: '8px' }
})
</script>

<style scoped>
.spark-box { position: relative; }
.spark-tip { position: absolute; top: -4px; z-index: 10; pointer-events: none; background: var(--color-bg-popup); border-radius: 4px;
  box-shadow: 0 4px 10px rgb(0 0 0 / 12%); padding: 4px 8px; font-size: 12px; line-height: 1.7; white-space: nowrap; color: var(--color-text-2); }
.spark-tip-t { color: var(--color-text-3); }
.spark-tip i { display: inline-block; width: 8px; height: 8px; border-radius: 2px; margin-right: 6px; }
.spark-tip b { font-weight: 500; color: var(--color-text-1); }
</style>
