<!--
  通用确认对话框（S7.3 起引入，此前项目没有弹层）。

  三种形态，靠 props 组合，不搞三个组件：
    * 简单确认——title + message + 确认/取消（stop、kill 等可逆或需斟酌的动作）；
    * 输名确认——requireName 给出目标名，用户须一字不差键入才能确认
      （删容器：后端 guard.Confirm 要求完全一致，前端先拦一遍省一次 400 往返）；
    * 附加选项——默认插槽（force/v 勾选、kill 信号下拉、stop 超时输入）。

  Teleport 到 body 避免被父级 transform/overflow 裁剪；Esc 与点遮罩都是取消。
-->
<template>
  <Teleport to="body">
    <Transition name="fade">
      <div v-if="open" class="mask" @click.self="onCancel">
        <div class="dialog" role="alertdialog" :aria-label="title">
          <h3 class="dialog__title">{{ title }}</h3>
          <p class="dialog__msg">{{ message }}</p>

          <div v-if="requireName" class="field dialog__name">
            <label class="field__label" :for="inputId">输入 {{ requireName }} 以确认</label>
            <input
              :id="inputId"
              v-model="typed"
              class="field__input"
              type="text"
              autocomplete="off"
              spellcheck="false"
              :placeholder="requireName"
              @keyup.enter="tryConfirm"
            />
            <p v-if="typed && !matched" class="field__hint field__hint--warn">名字不一致</p>
          </div>

          <div v-if="$slots.default" class="dialog__opts">
            <slot />
          </div>

          <div class="dialog__acts">
            <button type="button" class="btn" @click="onCancel">取消</button>
            <button
              type="button"
              class="btn"
              :class="danger ? 'btn--danger' : 'btn--primary'"
              :disabled="!matched"
              @click="onConfirm"
            >
              {{ confirmLabel }}
            </button>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'

const props = withDefaults(
  defineProps<{
    open: boolean
    title: string
    message: string
    /** 破坏性动作用 danger 样式（红确认钮）。 */
    danger?: boolean
    /** 给出目标名即开启「输名确认」；空串关闭。 */
    requireName?: string
    confirmLabel?: string
  }>(),
  { danger: false, requireName: '', confirmLabel: '确认' },
)

const emit = defineEmits<{
  (e: 'confirm'): void
  (e: 'cancel'): void
}>()

// 同一时刻只会有一个确认框，但 id 仍要唯一（避免 label for 撞车）。
const inputId = `confirm-name-${Math.random().toString(36).slice(2, 8)}`
const typed = ref('')
const matched = computed(() => !props.requireName || typed.value.trim() === props.requireName)

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') {
    emit('cancel')
  }
}

// 打开时清空已键入内容并挂上 Esc；关闭即摘，不常驻监听。
watch(
  () => props.open,
  (v) => {
    typed.value = ''
    if (v) {
      window.addEventListener('keydown', onKey)
    } else {
      window.removeEventListener('keydown', onKey)
    }
  },
)

function tryConfirm() {
  if (matched.value) {
    emit('confirm')
  }
}

function onConfirm() {
  emit('confirm')
}

function onCancel() {
  emit('cancel')
}
</script>

<style scoped>
.mask {
  position: fixed;
  inset: 0;
  z-index: var(--z-modal);
  display: grid;
  place-items: center;
  padding: var(--space-4);
  background: rgb(0 0 0 / 55%);
}

.dialog {
  width: min(440px, 100%);
  padding: var(--space-5);
  background: var(--bg-elev-2);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-lg);
}

.dialog__title {
  font-size: var(--text-lg);
  font-weight: var(--weight-semibold);
}

.dialog__msg {
  margin-top: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
  white-space: pre-wrap;
}

.dialog__name {
  margin-top: var(--space-4);
}

.dialog__opts {
  margin-top: var(--space-4);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.dialog__acts {
  margin-top: var(--space-5);
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
}

.fade-enter-active,
.fade-leave-active {
  transition: opacity var(--duration-fast) var(--ease-standard);
}

.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}

/* 手机态（<768px，PLAN-MOBILE M8）：确认框沉底全宽，按钮横向铺满、大触区。 */
@media (max-width: 767px) {
  .mask {
    align-items: flex-end;
    padding: 0;
  }

  .dialog {
    width: 100%;
    border-radius: var(--radius-lg) var(--radius-lg) 0 0;
    padding-bottom: calc(var(--space-5) + env(safe-area-inset-bottom));
  }

  .dialog__acts {
    margin-top: var(--space-4);
  }

  .dialog__acts .btn {
    flex: 1;
    min-height: 48px;
  }
}
</style>
