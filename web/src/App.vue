<!--
  应用根：路由出口 + 全局加载指示 + 全局错误提示。
  S6.3 会在此套上侧栏 + 顶栏布局外壳。
-->
<template>
  <div class="app">
    <div v-if="ui.isLoading" class="app__progress" aria-hidden="true" />
    <RouterView />
    <AppToast />
  </div>
</template>

<script setup lang="ts">
import AppToast from '@/components/AppToast.vue'
import { useUiStore } from '@/stores/ui'

const ui = useUiStore()
</script>

<style scoped>
.app {
  min-height: 100vh;
}

.app__progress {
  position: fixed;
  inset: 0 0 auto;
  z-index: var(--z-toast);
  height: 2px;
  background: var(--accent);
  animation: pulse 1.2s var(--ease-standard) infinite;
}

@keyframes pulse {
  0%,
  100% {
    opacity: 0.35;
  }

  50% {
    opacity: 1;
  }
}
</style>
