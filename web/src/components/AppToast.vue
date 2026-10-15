<!--
  全局错误提示。数据源是 ui store 的 error 字段：
  任何地方 catch 到异常后调 ui.notifyError(e)，这里即呈现 code + message。
  不承载重试逻辑——S7 各功能页按语义自行决定要不要重试。
-->
<template>
  <Transition name="toast">
    <div v-if="ui.error" class="toast" role="alert" aria-live="assertive">
      <span class="toast__msg">{{ ui.error.message }}</span>
      <code class="toast__code">{{ ui.error.code }}</code>
      <button type="button" class="toast__close" aria-label="关闭" @click="ui.clearError()">×</button>
    </div>
  </Transition>
</template>

<script setup lang="ts">
import { useUiStore } from '@/stores/ui'

const ui = useUiStore()
</script>

<style scoped>
.toast {
  position: fixed;
  right: var(--space-5);
  bottom: var(--space-5);
  z-index: var(--z-toast);
  display: flex;
  align-items: center;
  gap: var(--space-3);
  max-width: min(420px, calc(100vw - var(--space-6)));
  padding: var(--space-3) var(--space-4);
  background: var(--bg-elev-2);
  border: 1px solid var(--danger-border);
  border-left: 3px solid var(--danger);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-lg);
  color: var(--fg-default);
  font-size: var(--text-sm);
}

.toast__msg {
  flex: 1;
}

.toast__code {
  flex: none;
  padding: 0 var(--space-1);
  background: var(--danger-subtle);
  border-radius: var(--radius-sm);
  color: var(--danger);
  font-size: var(--text-xs);
}

.toast__close {
  flex: none;
  width: 22px;
  height: 22px;
  display: grid;
  place-items: center;
  padding: 0;
  background: transparent;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  color: var(--fg-muted);
  font-size: var(--text-lg);
  line-height: 1;
  transition: background var(--duration-fast) var(--ease-standard);
}

.toast__close:hover {
  background: var(--bg-elev-3);
  color: var(--fg-default);
}

.toast-enter-active,
.toast-leave-active {
  transition:
    opacity var(--duration-base) var(--ease-standard),
    transform var(--duration-base) var(--ease-standard);
}

.toast-enter-from,
.toast-leave-to {
  opacity: 0;
  transform: translateY(8px);
}
</style>
