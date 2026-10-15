<!--
  全局提示。两个通道，数据源都是 ui store：
   * error  —— 任何地方 catch 到异常后调 ui.notifyError(e)，呈现 code + message。
     不承载重试逻辑——S7 各功能页按语义自行决定要不要重试。
   * notice —— 操作成功的轻回报（S7.3 起，如「已重启 nginx」），数秒自熄。
     与 error 分开是因为语义不同：失败是 alert（要用户处理），成功是 status（告知即可）。

  两条 toast 摞在同一个右下角容器里（column-reverse：error 在下、notice 在上），
  不会互相覆盖。
-->
<template>
  <div class="toasts">
    <Transition name="toast">
      <div v-if="ui.error" class="toast" role="alert" aria-live="assertive">
        <span class="toast__msg">{{ ui.error.message }}</span>
        <code class="toast__code">{{ ui.error.code }}</code>
        <button type="button" class="toast__close" aria-label="关闭" @click="ui.clearError()">×</button>
      </div>
    </Transition>
    <Transition name="toast">
      <div v-if="ui.notice" class="toast toast--ok" role="status" aria-live="polite">
        <span class="toast__msg">{{ ui.notice }}</span>
        <button type="button" class="toast__close" aria-label="关闭" @click="ui.clearNotice()">×</button>
      </div>
    </Transition>
  </div>
</template>

<script setup lang="ts">
import { useUiStore } from '@/stores/ui'

const ui = useUiStore()
</script>

<style scoped>
.toasts {
  position: fixed;
  right: var(--space-5);
  bottom: var(--space-5);
  z-index: var(--z-toast);
  display: flex;
  flex-direction: column-reverse;
  gap: var(--space-2);
}

.toast {
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

.toast--ok {
  border-color: var(--success-border);
  border-left-color: var(--success);
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
