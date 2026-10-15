<!-- S6.1 占位首页：仅证明根路由可达。真实总览页在 S7.1。 -->
<template>
  <div class="boot">
    <h1 class="boot__title">DockDeck</h1>
    <p class="boot__hint">前端骨架已就位（S6.1）· 路由与状态已接线</p>
    <p class="boot__link"><a href="/dev/tokens">设计令牌色卡 →</a></p>
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
  min-height: 100vh;
  padding: var(--space-5);
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
