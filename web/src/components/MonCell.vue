<template>
  <span v-if="!on" class="muted">-</span>
  <!-- 数值显示在进度条 / 趋势线下方：主值在左，补充说明在右 -->
  <div v-else class="mon-cell">
    <a-progress v-if="mode === 'bars' || !history || history.length < 2" :percent="Math.min(pct, 100) / 100" :color="lvl(pct)" :show-text="false" />
    <Spark v-else :series="[{ values: history, color: lvl(pct) }]" :w="90" :h="24" width="100%" :max="100" :format="v => num(v) + '%'" />
    <div class="t"><span>{{ text }}</span><span class="muted">{{ sub }}</span></div>
  </div>
</template>

<script setup>
import { lvl, num } from '../api'
import Spark from './Spark.vue'
defineProps({ on: Boolean, mode: String, pct: Number, text: String, sub: String, history: Array })
</script>
