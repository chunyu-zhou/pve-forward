<template>
  <span v-if="!on" class="muted">-</span>
  <div v-else class="mon-cell">
    <a-progress v-if="mode === 'bars' || !history || history.length < 2" :percent="Math.min(pct, 100) / 100" :color="lvl(pct)" :show-text="false" />
    <Spark v-else :series="[{ values: history, color: lvl(pct) }]" :w="90" :h="24" width="100%" :max="100" style="flex:1" />
    <div class="t"><div>{{ text }}</div><div class="muted">{{ sub }}</div></div>
  </div>
</template>

<script setup>
import { lvl } from '../api'
import Spark from './Spark.vue'
defineProps({ on: Boolean, mode: String, pct: Number, text: String, sub: String, history: Array })
</script>
