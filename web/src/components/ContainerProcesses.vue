<!--
  #91 容器进程明细抽屉：点行内「进程」从右侧滑出，不离开容器列表。

  数据口径：Agent 从宿主 /proc 按 cgroup 归属容器后返回（需 Agent 以 pid:host
  运行）。读不到时面板会给出提示，而不是空表——用户据此判断是「没进程」还是
  「Agent 没开 pid:host」。

  只读展示，不含任何写/杀进程操作（杀进程属独立高危动作，未纳入 #91）。
-->
<template>
  <Teleport to="body">
    <Transition name="fade">
      <div v-if="open" class="cp__mask" @click="close" />
    </Transition>
    <Transition name="slide">
      <aside v-if="open" class="cp__panel" role="dialog" aria-label="容器进程">
        <header class="cp__head">
          <div class="cp__title">
            <span class="cp__name" :title="name">{{ name }}</span>
            <code class="cp__id">{{ shortId }}</code>
            <span class="cp__count">{{ rows.length }} 个进程</span>
          </div>
          <button type="button" class="btn cp__close" aria-label="关闭" @click="close">×</button>
        </header>

        <div class="cp__bar">
          <button type="button" class="btn" :disabled="loading" @click="reload">
            {{ loading ? '加载中…' : '刷新' }}
          </button>
          <span class="cp__hint">需 Agent 以 pid:host 运行</span>
        </div>

        <p v-if="error" class="cp__err">{{ error }}</p>

        <div class="cp__body">
          <table v-if="rows.length > 0" class="cp__table">
            <thead>
              <tr>
                <th class="num">PID</th>
                <th class="num">PPID</th>
                <th>命令</th>
                <th>用户</th>
                <th>状态</th>
                <th class="num">内存</th>
                <th class="num">线程</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="p in rows"
                :key="p.pid"
                class="cp__row"
                :class="{ 'cp__row--active': selectedPid === p.pid }"
                @click="selectProcess(p)"
              >
                <td class="num">{{ p.pid }}</td>
                <td class="num">{{ p.ppid }}</td>
                <td class="cp__cmd" :title="p.command">{{ p.command || p.comm }}</td>
                <td>{{ p.user || '—' }}</td>
                <td :title="stateLabel(p.state)">{{ p.state || '—' }}</td>
                <td class="num">{{ p.mem_rss > 0 ? formatBytes(p.mem_rss * 1024) : '—' }}</td>
                <td class="num">{{ p.threads || '—' }}</td>
              </tr>
            </tbody>
          </table>
          <p v-else-if="!loading && !error" class="cp__empty">
            {{ error ? '' : '没有读到进程。若容器有进程，通常是 Agent 未以 pid:host 运行。' }}
          </p>
        </div>

        <footer v-if="selectedPid !== null" class="cp__detail">
          <p class="cp__detail-title">进程 {{ selectedPid }} · 详情</p>
          <p v-if="detailLoading" class="cp__dim">加载中…</p>
          <p v-else-if="detailError" class="cp__err cp__err--inline">{{ detailError }}</p>
          <dl v-else-if="detail" class="cp__dl">
            <dt>exe</dt>
            <dd :title="detail.exe">{{ detail.exe || '—' }}</dd>
            <dt>cwd</dt>
            <dd :title="detail.cwd">{{ detail.cwd || '—' }}</dd>
            <dt>打开文件</dt>
            <dd>{{ detail.open_files?.length ?? 0 }} 个</dd>
          </dl>
          <ul v-if="detail && detail.open_files && detail.open_files.length" class="cp__fds">
            <li v-for="(f, i) in detail.open_files" :key="i" :title="f">{{ f }}</li>
          </ul>
        </footer>
      </aside>
    </Transition>
  </Teleport>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { fetchContainerProcessDetail, fetchContainerProcesses, type ContainerProcess, type ContainerProcessDetail } from '@/api/containers'
import { formatBytes } from '@/utils/format'

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

const rows = ref<ContainerProcess[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

// 单进程详情（#91 v2）：点行展开 exe/cwd。
const selectedPid = ref<number | null>(null)
const detail = ref<ContainerProcessDetail | null>(null)
const detailLoading = ref(false)
const detailError = ref<string | null>(null)

function clearDetail() {
  selectedPid.value = null
  detail.value = null
  detailError.value = null
  detailLoading.value = false
}

async function selectProcess(p: ContainerProcess) {
  // 再次点击同一行 = 收起。
  if (selectedPid.value === p.pid) {
    clearDetail()
    return
  }
  selectedPid.value = p.pid
  detail.value = null
  detailError.value = null
  detailLoading.value = true
  try {
    const d = await fetchContainerProcessDetail(props.serverId, props.cid, p.pid)
    // 竞态保护：期间用户又点了别的行，丢弃这次结果。
    if (selectedPid.value === p.pid) {
      detail.value = d
    }
  } catch (e) {
    if (selectedPid.value === p.pid) {
      detailError.value = e instanceof Error ? e.message : String(e)
    }
  } finally {
    if (selectedPid.value === p.pid) {
      detailLoading.value = false
    }
  }
}

const shortId = computed(() => (props.cid.length > 12 ? props.cid.slice(0, 12) : props.cid))

/** 状态字母的易读说明（用于 title，不改变原始字母展示）。 */
function stateLabel(s: string): string {
  switch (s) {
    case 'R':
      return '运行中（R）'
    case 'S':
      return '可中断睡眠（S）'
    case 'D':
      return '不可中断睡眠（D）'
    case 'T':
      return '已停止（T）'
    case 'Z':
      return '僵尸（Z）'
    case 'I':
      return '空闲（I）'
    case 'X':
      return '已死亡（X）'
    default:
      return s
  }
}

async function reload() {
  loading.value = true
  error.value = null
  clearDetail()
  try {
    rows.value = await fetchContainerProcesses(props.serverId, props.cid)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    rows.value = []
  } finally {
    loading.value = false
  }
}

function close() {
  emit('close')
}

// 打开即加载；容器换了重新加载；关闭时清空。
watch(
  () => props.open,
  (v) => {
    if (v) {
      void reload()
    } else {
      rows.value = []
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

// 抽屉开着锁 body 滚动（与 ContainerLogs 同款）。
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
.cp__mask {
  position: fixed;
  inset: 0;
  z-index: var(--z-modal);
  background: rgb(0 0 0 / 45%);
}

.cp__panel {
  position: fixed;
  inset: 0 0 0 auto;
  z-index: var(--z-modal);
  width: min(760px, 100%);
  display: flex;
  flex-direction: column;
  background: var(--bg-elev-1);
  border-left: 1px solid var(--border-default);
  box-shadow: var(--shadow-lg);
}

.cp__head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
}

.cp__title {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  min-width: 0;
}

.cp__name {
  font-size: var(--text-base);
  font-weight: var(--weight-semibold);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.cp__id {
  flex: none;
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.cp__count {
  flex: none;
  color: var(--fg-muted);
  font-size: var(--text-xs);
}

.cp__close {
  margin-left: auto;
  flex: none;
  width: 32px;
  height: 32px;
  padding: 0;
  font-size: var(--text-lg);
  line-height: 1;
}

.cp__bar {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
}

.cp__hint {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.cp__err {
  margin: var(--space-3) var(--space-4) 0;
  padding: var(--space-2) var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

.cp__body {
  flex: 1;
  overflow: auto;
  padding: var(--space-3) var(--space-4) var(--space-4);
}

.cp__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.cp__table th {
  position: sticky;
  top: 0;
  padding: var(--space-2);
  background: var(--bg-elev-1);
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-subtle);
  font-weight: var(--weight-normal);
  text-align: left;
}

.cp__table td {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
  vertical-align: middle;
}

.cp__table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.cp__cmd {
  max-width: 280px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs);
}

.cp__row {
  cursor: pointer;
}

.cp__row:hover td {
  background: var(--bg-elev-2);
}

.cp__row--active td {
  background: var(--accent-subtle);
}

.cp__detail {
  border-top: 1px solid var(--border-subtle);
  padding: var(--space-3) var(--space-4);
  background: var(--bg-inset);
}

.cp__detail-title {
  margin-bottom: var(--space-2);
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.cp__dl {
  margin: 0;
  display: grid;
  grid-template-columns: 48px minmax(0, 1fr);
  gap: var(--space-1) var(--space-3);
  font-size: var(--text-sm);
}

.cp__dl dt {
  color: var(--fg-subtle);
}

.cp__dl dd {
  margin: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs);
}

.cp__fds {
  margin: 0;
  padding: var(--space-2) 0 0;
  list-style: none;
  max-height: 140px;
  overflow: auto;
  border-top: 1px dashed var(--border-subtle);
}

.cp__fds li {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs);
  color: var(--fg-muted);
}

.cp__dim {
  color: var(--fg-subtle);
  font-size: var(--text-sm);
}

.cp__err--inline {
  margin: 0;
}

.cp__empty {
  padding: var(--space-6);
  color: var(--fg-subtle);
  font-size: var(--text-sm);
  text-align: center;
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
