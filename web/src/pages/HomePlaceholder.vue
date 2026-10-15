<!-- S6.1 占位首页（S3 起被 AppShell 包裹）。真实总览页在 S7.1。 -->
<template>
  <div class="boot">
    <h1 class="boot__title">DockDeck</h1>
    <p class="boot__hint">布局已就位（S6.3）· 手机优先，桌面可折叠侧栏</p>
    <p class="boot__link">
      <RouterLink to="/dev/tokens">设计令牌色卡 →</RouterLink>
    </p>
  </div>
</template>

<script setup lang="ts">
// 占位页暂无逻辑。
</script>

<style scoped>
.boot {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--space-2);
  /* 不再自撑 100vh：外壳已是满高，这里只在内容区内居中。 */
  padding: var(--space-6) 0;
  text-align: center;
}

.boot__title {
  font-size: var(--text-2xl);
  letter-spacing: 0.02em;
}

.boot__hint {
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.boot__link {
  margin-top: var(--space-4);
  font-size: var(--text-sm);
}
</style>
