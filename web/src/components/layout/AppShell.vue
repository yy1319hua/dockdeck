<!--
  应用外壳：需要登录的主界面共用这一层（侧栏 + 顶栏 + 内容）。
  谁套壳由路由结构决定（AppShell 作为父路由），而不是在 App.vue 里 if 判断——
  /login、/setup 走顶层路由，天然不套壳，保持居中单卡形态。

  响应式顺序是「先定手机」：<1024px 侧栏是抽屉 + 遮罩；≥1024px 才变常驻列，
  折叠状态（shell--collapsed）只影响列宽，抽屉态下该类名不产生任何作用。
-->
<template>
  <div class="shell" :class="{ 'shell--collapsed': ui.sidebarCollapsed }">
    <AppTopbar />
    <AppSidebar />
    <div v-if="ui.sidebarOpen" class="shell__scrim" @click="ui.closeSidebar()" />
    <main class="shell__main">
      <RouterView />
    </main>
    <!-- 手机态主导航（方案 A，PLAN-MOBILE M1/M2）：<768px 显示，桌面 CSS 隐藏。 -->
    <AppTabBar />
  </div>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import AppSidebar from './AppSidebar.vue'
import AppTabBar from './AppTabBar.vue'
import AppTopbar from './AppTopbar.vue'
import { useUiStore } from '@/stores/ui'

const ui = useUiStore()
const route = useRoute()

// 路由切换即收抽屉：移动端点完导航要立刻看见内容，而不是被遮罩压着。
watch(
  () => route.fullPath,
  () => ui.closeSidebar(),
)

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape' && ui.sidebarOpen) {
    ui.closeSidebar()
  }
}

onMounted(() => window.addEventListener('keydown', onKeydown))
onUnmounted(() => window.removeEventListener('keydown', onKeydown))
</script>

<style scoped>
.shell {
  display: grid;
  grid-template-areas:
    'sidebar topbar'
    'sidebar main';
  grid-template-columns: 240px minmax(0, 1fr);
  grid-template-rows: 56px minmax(0, 1fr);
  min-height: 100vh;
}

.shell--collapsed {
  grid-template-columns: 64px minmax(0, 1fr);
}

.shell__main {
  grid-area: main;
  min-width: 0;
  padding: var(--space-5);
}

.shell__scrim {
  position: fixed;
  inset: 0;
  z-index: calc(var(--z-modal) - 1);
  background: rgb(1 4 9 / 62%);
}

@media (max-width: 1023px) {
  .shell {
    grid-template-areas:
      'topbar'
      'main';
    grid-template-columns: minmax(0, 1fr);
  }

  .shell__main {
    padding: var(--space-4) var(--space-3);
  }
}

/* 手机态（<768px，方案 A）：侧栏整体退役（抽屉也不再出现），主导航沉底。 */
@media (max-width: 767px) {
  .shell {
    grid-template-areas:
      'topbar'
      'main'
      'tabbar';
    grid-template-columns: minmax(0, 1fr);
    grid-template-rows: 56px minmax(0, 1fr) auto;
  }

  .sidebar {
    display: none;
  }

  .shell__main {
    /* TabBar 吸底不占内容流之外：多留一层底部安全区，长列表最后一行不被盖。 */
    padding-bottom: calc(var(--space-5) + env(safe-area-inset-bottom));
  }
}
</style>
