<!--
  侧栏。两种形态共用同一个 DOM，靠 CSS 断点切换：
    <1024px —— 抽屉：fixed 覆盖 + 遮罩，收起时用 translateX 移出屏幕。
    ≥1024px —— 常驻：占网格一列，可折叠成图标条（另有专门的 collapse 状态）。
  未接入的功能页按 S6.3 决议先占位列出，置灰不可点并标「待实现」。
-->
<template>
  <aside
    class="sidebar"
    :class="{ 'sidebar--open': ui.sidebarOpen, 'sidebar--collapsed': ui.sidebarCollapsed }"
  >
    <div class="sidebar__brand">
      <span class="sidebar__logo">◈</span>
      <span class="sidebar__wordmark">DockDeck</span>
    </div>

    <nav class="sidebar__nav" :aria-label="'主导航'">
      <template v-for="item in navItems" :key="item.label">
        <RouterLink
          v-if="item.to"
          class="navitem"
          active-class="navitem--active"
          :to="{ name: item.to }"
          @click="ui.closeSidebar()"
        >
          <AppIcon :name="item.icon" />
          <span class="navitem__label">{{ item.label }}</span>
        </RouterLink>
        <span v-else class="navitem navitem--soon" :title="`${item.label}：功能页将在 S7 接入`">
          <AppIcon :name="item.icon" />
          <span class="navitem__label">{{ item.label }}</span>
          <span class="navitem__tag">待实现</span>
        </span>
      </template>
    </nav>

    <button type="button" class="sidebar__collapse" @click="ui.toggleCollapsed()">
      <AppIcon name="menu" />
      <span class="sidebar__collapse-label">折叠侧栏</span>
    </button>
  </aside>
</template>

<script setup lang="ts">
import AppIcon from '@/components/AppIcon.vue'
import { navItems } from '@/config/nav'
import { useUiStore } from '@/stores/ui'

const ui = useUiStore()
</script>

<style scoped>
.sidebar {
  grid-area: sidebar;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-3) var(--space-2);
  background: var(--bg-elev-1);
  border-right: 1px solid var(--border-default);
  overflow: hidden;
}

.sidebar__brand {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  height: 40px;
  padding: 0 var(--space-2);
  color: var(--fg-default);
  font-weight: var(--weight-semibold);
  white-space: nowrap;
}

.sidebar__logo {
  flex: none;
  width: 22px;
  text-align: center;
  color: var(--accent);
  font-size: var(--text-lg);
}

.sidebar__wordmark {
  font-size: var(--text-base);
}

.sidebar__nav {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  margin-top: var(--space-3);
}

.navitem {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-md);
  color: var(--fg-muted);
  font-size: var(--text-sm);
  white-space: nowrap;
  transition:
    background var(--duration-fast) var(--ease-standard),
    color var(--duration-fast) var(--ease-standard);
}

.navitem:hover {
  background: var(--bg-elev-2);
  color: var(--fg-default);
}

.navitem--active {
  background: var(--accent-subtle);
  border-left: 2px solid var(--accent);
  color: var(--fg-default);
  padding-left: calc(var(--space-3) - 2px);
}

.navitem--soon {
  cursor: not-allowed;
  opacity: 0.45;
}

.navitem--soon:hover {
  background: transparent;
  color: var(--fg-muted);
}

.navitem__label {
  flex: 1;
}

.navitem__tag {
  flex: none;
  padding: 0 var(--space-1);
  background: var(--bg-elev-3);
  border-radius: var(--radius-sm);
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.sidebar__collapse {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-top: auto;
  padding: var(--space-2) var(--space-3);
  background: transparent;
  border: 1px solid transparent;
  border-radius: var(--radius-md);
  color: var(--fg-subtle);
  font-size: var(--text-sm);
  white-space: nowrap;
}

.sidebar__collapse:hover {
  background: var(--bg-elev-2);
  color: var(--fg-default);
}

/* ---------- 手机 / 平板：抽屉（先定的形态） ---------- */
@media (max-width: 1023px) {
  .sidebar {
    position: fixed;
    inset: 0 auto 0 0;
    z-index: var(--z-modal);
    width: 264px;
    transform: translateX(-100%);
    transition: transform var(--duration-base) var(--ease-standard);
    box-shadow: var(--shadow-lg);
  }

  .sidebar--open {
    transform: translateX(0);
  }

  /* 抽屉态没有「折叠」的位置，隐藏即可。 */
  .sidebar__collapse {
    display: none;
  }
}

/* ---------- 桌面：常驻，可折叠成图标条 ---------- */
@media (min-width: 1024px) {
  .sidebar--collapsed {
    padding-left: var(--space-2);
    padding-right: var(--space-2);
  }

  .sidebar--collapsed .sidebar__brand {
    justify-content: center;
    padding: 0;
  }

  .sidebar--collapsed .sidebar__wordmark,
  .sidebar--collapsed .navitem__label,
  .sidebar--collapsed .navitem__tag,
  .sidebar--collapsed .sidebar__collapse-label {
    display: none;
  }

  .sidebar--collapsed .navitem,
  .sidebar--collapsed .sidebar__collapse {
    justify-content: center;
    padding-left: 0;
    padding-right: 0;
  }

  .sidebar--collapsed .navitem--active {
    border-left: 0;
    padding-left: var(--space-2);
  }
}
</style>
