<!--
  容器日志抽屉（S7.3）：点行内「日志」从右侧滑出，不离开容器列表。

  三个实现要点：
    * 日志端点是**裸流 text/plain 无信封**，不能走 request()，用 api/containers 的
      专用通道；follow=1 是长连接，用 fetch + ReadableStream 边收边显示，
      AbortController 负责停止（浏览器断开后端即结束，见 containerLogs 注解）。
    * 文本设 2 MB 上限：follow 一晚上能刷出几十 MB，全留着会把浏览器拖死，
      超限从头部截断——看日志的人只关心最新的一段。
    * 自动贴底：新内容到达即滚到底部（日志查看器的默认预期）；导出走后端
      export=1 原始字节流，与 docker CLI 可消费的形态一致。

  Transition 拆成两条（遮罩淡入、面板滑入）：slide 的 transform 若包住
  position:fixed 的面板，变换中的祖先会成为固定定位的包含块，面板会错位。
-->
<template>
  <Teleport to="body">
    <Transition name="fade">
      <div v-if="open" class="logs__mask" @click="close" />
    </Transition>
    <Transition name="slide">
      <aside v-if="open" class="logs__panel" role="dialog" aria-label="容器日志">
        <header class="logs__head">
          <div class="logs__title">
            <span class="logs__name" :title="name">{{ name }}</span>
            <code class="logs__id">{{ shortId }}</code>
          </div>
          <button type="button" class="btn logs__close" aria-label="关闭" @click="close">×</button>
        </header>

        <div class="logs__bar">
          <label class="logs__tail">
            行数
            <select v-model="tail" class="field__input logs__select" :disabled="following">
              <option value="100">100</option>
              <option value="200">200</option>
              <option value="500">500</option>
              <option value="all">all</option>
            </select>
          </label>
          <label class="logs__check">
            <input v-model="timestamps" type="checkbox" :disabled="following" />
            时间戳
          </label>
          <button type="button" class="btn" :disabled="loading" @click="reload">刷新</button>
          <button
            type="button"
            class="btn"
            :class="following ? '' : 'btn--primary'"
            :disabled="loading"
            @click="toggleFollow"
          >
            {{ following ? '停止跟随' : '跟随' }}
          </button>
          <button type="button" class="btn" :disabled="exporting" @click="onExport">
            {{ exporting ? '导出中…' : '导出' }}
          </button>
        </div>

        <p v-if="error" class="logs__err">{{ error }}</p>
        <p v-if="truncated" class="logs__trunc">内容过长，仅保留最新约 2 MB</p>

        <pre ref="bodyEl" class="logs__body">{{ text || emptyHint }}</pre>
      </aside>
    </Transition>
  </Teleport>
</template>

<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, watch } from 'vue'
import { exportLogs, fetchLogsText, streamLogs, type LogsQuery } from '@/api/containers'

const props = withDefaults(
  defineProps<{
    open: boolean
    serverId: number
    cid: string
    name: string
  }>(),
  { cid: '', name: '' },
)

const emit = defineEmits<{
  (e: 'close'): void
}>()

const MAX_CHARS = 2_000_000
const emptyHint = '（加载中或暂无日志）'

const tail = ref('200')
const timestamps = ref(false)
const text = ref('')
const loading = ref(false)
const following = ref(false)
const exporting = ref(false)
const error = ref<string | null>(null)
const truncated = ref(false)
const bodyEl = ref<HTMLPreElement | null>(null)
let abort: AbortController | null = null

const shortId = computed(() => (props.cid.length > 12 ? props.cid.slice(0, 12) : props.cid))

function query(): LogsQuery {
  return { tail: tail.value, timestamps: timestamps.value }
}

function scrollToBottom() {
  void nextTick(() => {
    if (bodyEl.value) {
      bodyEl.value.scrollTop = bodyEl.value.scrollHeight
    }
  })
}

function append(chunk: string) {
  if (!chunk) {
    return
  }
  text.value += chunk
  if (text.value.length > MAX_CHARS) {
    text.value = text.value.slice(-MAX_CHARS)
    truncated.value = true
  }
  scrollToBottom()
}

async function reload() {
  stopFollow()
  loading.value = true
  error.value = null
  try {
    text.value = await fetchLogsText(props.serverId, props.cid, query())
    truncated.value = false
    scrollToBottom()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

async function toggleFollow() {
  if (following.value) {
    stopFollow()
    return
  }
  error.value = null
  text.value = ''
  truncated.value = false
  following.value = true
  const controller = new AbortController()
  abort = controller
  try {
    await streamLogs(props.serverId, props.cid, query(), append, controller.signal)
    // 流自然结束（对端关闭）：退出跟随态但保留已收到内容。
    following.value = false
  } catch (e) {
    // 主动停止（abort）不是错误，静默收尾。
    if (!controller.signal.aborted) {
      error.value = e instanceof Error ? e.message : String(e)
    }
    following.value = false
  } finally {
    if (abort === controller) {
      abort = null
    }
  }
}

function stopFollow() {
  abort?.abort()
  abort = null
  following.value = false
}

async function onExport() {
  exporting.value = true
  error.value = null
  try {
    await exportLogs(props.serverId, props.cid, props.name || 'container')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    exporting.value = false
  }
}

function close() {
  stopFollow()
  emit('close')
}

// 打开即加载；容器换了重新加载；关闭时中止在途请求并清空。
watch(
  () => props.open,
  (v) => {
    if (v) {
      void reload()
    } else {
      stopFollow()
      text.value = ''
      error.value = null
    }
  },
)
watch(
  () => props.cid,
  () => {
    if (props.open) {
      void reload()
    }
  },
)
// 参数变更（行数/时间戳）立即重取——follow 态下这两个控件是禁用的，不会走到这。
watch([tail, timestamps], () => {
  if (props.open && !following.value) {
    void reload()
  }
})

// 抽屉开着锁 body 滚动（PLAN-MOBILE M6 / HANDOFF #12 后半：背景可点问题随遮罩
// 已有，滚动穿透补上）；关闭/卸载时必须还原，否则整页失去滚动。
watch(
  () => props.open,
  (v) => {
    document.body.style.overflow = v ? 'hidden' : ''
  },
)
onUnmounted(() => {
  document.body.style.overflow = ''
})
</script>

<style scoped>
.logs__mask {
  position: fixed;
  inset: 0;
  z-index: var(--z-modal);
  background: rgb(0 0 0 / 45%);
}

.logs__panel {
  position: fixed;
  inset: 0 0 0 auto;
  z-index: var(--z-modal);
  width: min(640px, 100%);
  display: flex;
  flex-direction: column;
  background: var(--bg-elev-1);
  border-left: 1px solid var(--border-default);
  box-shadow: var(--shadow-lg);
}

.logs__head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
}

.logs__title {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  min-width: 0;
}

.logs__name {
  font-size: var(--text-base);
  font-weight: var(--weight-semibold);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.logs__id {
  flex: none;
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.logs__close {
  margin-left: auto;
  flex: none;
  width: 32px;
  height: 32px;
  padding: 0;
  font-size: var(--text-lg);
  line-height: 1;
}

.logs__bar {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  flex-wrap: wrap;
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.logs__tail {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.logs__select {
  height: 30px;
  width: auto;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

.logs__check {
  display: flex;
  align-items: center;
  gap: var(--space-1);
}

.logs__err {
  margin: var(--space-3) var(--space-4) 0;
  padding: var(--space-2) var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

.logs__trunc {
  margin: var(--space-3) var(--space-4) 0;
  color: var(--warning);
  font-size: var(--text-xs);
}

.logs__body {
  flex: 1;
  margin: var(--space-3) var(--space-4) var(--space-4);
  padding: var(--space-3);
  overflow: auto;
  background: var(--bg-inset);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  color: var(--fg-default);
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}

.fade-enter-active,
.fade-leave-active {
  transition: opacity var(--duration-fast) var(--ease-standard);
}

.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}

.slide-enter-active,
.slide-leave-active {
  transition: transform var(--duration-base) var(--ease-standard);
}

.slide-enter-from,
.slide-leave-to {
  transform: translateX(100%);
}
</style>
