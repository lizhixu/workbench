<script setup lang="ts">
import { useRoute } from 'vue-router'
import FileManagerPane from '../../components/host/FileManagerPane.vue'

// KeepAlive 按组件名缓存页签视图，名字必须与 AppShell 里登记的一致
defineOptions({ name: 'FileManager' })

const route = useRoute()
</script>

<template>
  <FileManagerPane :host-id="route.params.id as string" />
</template>