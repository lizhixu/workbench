<script setup lang="ts">
import { useRoute } from 'vue-router'
import FileManagerPane from '../../components/host/FileManagerPane.vue'

const route = useRoute()
</script>

<template>
  <FileManagerPane :host-id="route.params.id as string" />
</template>