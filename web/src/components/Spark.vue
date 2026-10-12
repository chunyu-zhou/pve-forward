<template>
  <svg :viewBox="`0 0 ${w} ${h}`" preserveAspectRatio="none" :style="{ width: width || w + 'px', height: h + 'px', display: 'block' }">
    <template v-for="(s, i) in series" :key="i">
      <path v-if="fill && i === 0" :d="path(s.values) + ` L${w} ${h} L0 ${h} Z`" :fill="s.color" fill-opacity=".1" />
      <path :d="path(s.values)" fill="none" :stroke="s.color" stroke-width="1.5" vector-effect="non-scaling-stroke" />
    </template>
  </svg>
</template>

<script setup>
const props = defineProps({ series: Array, w: { type: Number, default: 60 }, h: { type: Number, default: 20 }, width: String, fill: Boolean, max: Number })
function path(vals) {
  if (!vals || vals.length < 2) return `M0 ${props.h - 1} L${props.w} ${props.h - 1}`
  const all = props.series.flatMap(s => s.values || [])
  const max = props.max || Math.max(1e-9, ...all)
  const step = props.w / (vals.length - 1)
  return vals.map((v, i) => `${i ? 'L' : 'M'}${(i * step).toFixed(1)} ${(props.h - 1 - (Math.max(0, v || 0) / max) * (props.h - 3)).toFixed(1)}`).join(' ')
}
</script>
