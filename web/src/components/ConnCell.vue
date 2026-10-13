<template>
  <a-tooltip content="经转发规则的当前连接数：TCP（已建立） / UDP">
    <div class="conn-cell mono" :class="{ muted: !tcp && !udp }"><div>TCP {{ tcp || 0 }}</div><div>UDP {{ udp || 0 }}</div></div>
  </a-tooltip>
</template>

<script setup>
defineProps({ tcp: Number, udp: Number })
</script>

<style scoped>
.conn-cell { line-height: 1.6; font-size: 12px; white-space: nowrap; cursor: default; }
</style>
