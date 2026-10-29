<script setup lang="ts">
import CertManager from '../../components/cert/CertManager.vue'

// 路由仍是单条 /certs，AppShell 的 KeepAlive / 页签按此名缓存。
defineOptions({ name: 'CertList' })
</script>

<template>
  <div class="certs-view page-flex-column">
    <CertManager />
  </div>
</template>

<style scoped lang="scss">
.certs-view {
  overflow: hidden;
}
</style>
