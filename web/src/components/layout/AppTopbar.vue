<!--
  顶栏：移动端汉堡（拉抽屉）、当前页标题、右侧用户区。
  标题取 route.meta.title，由路由集中提供，页面自己不写 document.title。
-->
<template>
  <header class="topbar">
    <button
      type="button"
      class="topbar__menu"
      aria-label="打开导航"
      @click="ui.toggleSidebar()"
    >
      <AppIcon name="menu" :size="20" />
    </button>

    <h1 class="topbar__title">{{ title }}</h1>

    <div class="topbar__right">
      <span class="topbar__user">
        <span class="topbar__avatar">{{ initial }}</span>
        <span class="topbar__name">{{ auth.username || '未登录' }}</span>
      </span>
      <button
        v-if="auth.signedIn"
        type="button"
        class="topbar__logout"
        @click="onLogout"
      >
        登出
      </button>
    </div>
  </header>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import { useAuthStore } from '@/stores/auth'
import { useUiStore } from '@/stores/ui'

const route = useRoute()
const router = useRouter()
const ui = useUiStore()
const auth = useAuthStore()

const title = computed(() => (route.meta.title as string | undefined) ?? 'DockDeck')
const initial = computed(() => (auth.username ? auth.username.slice(0, 1).toUpperCase() : '?'))

async function onLogout() {
  await auth.logout()
  // 当前页若受保护，守卫会自行把我们送到登录页；这里只处理公开页的兜底跳转。
  await router.push({ name: 'login' })
}
</script>

<style scoped>
.topbar {
  grid-area: topbar;
  display: flex;
  align-items: center;
  gap: var(--space-3);
  height: 56px;
  padding: 0 var(--space-4);
  background: var(--bg-base);
  border-bottom: 1px solid var(--border-default);
}

.topbar__menu {
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  padding: 0;
  background: transparent;
  border: 1px solid transparent;
  border-radius: var(--radius-md);
  color: var(--fg-muted);
}

.topbar__menu:hover {
  background: var(--bg-elev-2);
  color: var(--fg-default);
}

.topbar__title {
  font-size: var(--text-lg);
  font-weight: var(--weight-semibold);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.topbar__right {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-left: auto;
}

.topbar__user {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.topbar__avatar {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  background: var(--accent-subtle);
  border: 1px solid var(--accent-border);
  border-radius: var(--radius-full);
  color: var(--accent-hover);
  font-size: var(--text-xs);
  font-weight: var(--weight-semibold);
}

.topbar__logout {
  padding: var(--space-1) var(--space-3);
  background: transparent;
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.topbar__logout:hover {
  border-color: var(--danger-border);
  color: var(--danger);
}

@media (max-width: 639px) {
  .topbar__name {
    display: none;
  }
}

/* 桌面常驻侧栏时，汉堡没有意义（侧栏就在旁边）。 */
@media (min-width: 1024px) {
  .topbar__menu {
    display: none;
  }
}
</style>
