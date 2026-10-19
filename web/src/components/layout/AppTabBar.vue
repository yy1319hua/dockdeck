<!--
  底部标签栏（PLAN-MOBILE M2，方案 A）：手机态（<768px）的主导航。
  桌面态由 CSS display:none 整体隐藏——AppShell 无条件挂载本组件，
  断点切换零 JS，与侧栏「同一 DOM 两种形态」的既有口径一致。

  设计口径：
  - 五项固定（总览/容器/镜像/文件/设置），从 navItems 数据源按白名单挑，
    不复制文案；卷网络/Compose/审计低频，收进设置或后续分组（M7 复核）。
  - 触区：整条高 52px + 安全区内边距，单项竖排图标+文字，命中面积全宽。
  - 高亮：active 用 --chart-1；aria-current 标记当前路由（无障碍）。
-->
<template>
  <nav class="tabbar" aria-label="底部导航">
    <RouterLink
      v-for="item in items"
      :key="item.to"
      class="tabbar__item"
      active-class="tabbar__item--active"
      :to="{ name: item.to }"
    >
      <AppIcon :name="item.icon" />
      <span class="tabbar__label">{{ item.label }}</span>
    </RouterLink>
  </nav>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import { navItems } from '@/config/nav'

// 白名单：TabBar 只放高频五项；label/icon 直接复用 navItems 单一数据源。
const TAB_ROUTES = ['home', 'containers', 'images', 'files', 'settings']
const items = computed(() => navItems.filter((n) => n.to && TAB_ROUTES.includes(n.to)))
</script>

<style scoped>
.tabbar {
  /* 桌面态隐藏：导航职责归侧栏。 */
  display: none;
}

@media (max-width: 767px) {
  .tabbar {
    grid-area: tabbar;
    display: flex;
    background: var(--bg-elev-1);
    border-top: 1px solid var(--border-default);
    /* 全面屏手势区：内容抬高，背景延伸到安全区之下。 */
    padding-bottom: env(safe-area-inset-bottom);
  }

  .tabbar__item {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
    padding: 8px 0 6px;
    color: var(--fg-muted);
    text-decoration: none;
    font-size: 11px;
    min-height: 52px;
    box-sizing: border-box;
  }

  .tabbar__item--active {
    color: var(--chart-1);
  }
}
</style>
