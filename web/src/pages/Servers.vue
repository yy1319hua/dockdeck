<!--
  服务器列表：全局清单 + 在线状态（2026-10-20 用户要求进侧栏，原入口只在总览卡片）。

  数据口径与总览页同一把尺：身份与在线状态来自 GET /servers（last_seen 判定），
  本页不拉实时指标——那是总览/详情页的职责，清单页保持轻量。
  30s 轮询与后端采集周期对齐；离开页面停表（onUnmounted），不泄漏定时器
  （与文件页在线状态轮询同模式）。行点击进 /servers/:id 详情。
-->
<template>
  <div class="sv">
    <header class="sv__head">
      <div>
        <h1 class="sv__title">服务器列表</h1>
        <p class="sv__sub">
          共 {{ rows.length }} 台 · 在线 {{ onlineCount }} 台<template v-if="loading"> · 刷新中…</template>
        </p>
      </div>
      <div class="sv__actions">
        <button type="button" class="btn" :disabled="loading" @click="load">刷新</button>
        <button type="button" class="btn btn--primary" @click="goAddNode">添加节点</button>
      </div>
    </header>

    <p v-if="error" class="sv__err">{{ error }}</p>

    <div v-else-if="loaded && rows.length === 0" class="sv__empty">
      <p class="sv__empty-title">还没有纳管的服务器</p>
      <p class="sv__empty-hint">点「添加节点」生成一次性纳管命令，在目标机器上执行即可。</p>
      <button type="button" class="btn btn--primary" @click="goAddNode">添加节点</button>
    </div>

    <table v-else-if="rows.length > 0" class="sv__table">
      <thead>
        <tr>
          <th>服务器</th>
          <th>地址</th>
          <th>架构</th>
          <th>Docker</th>
          <th>状态</th>
          <th>最后通讯</th>
          <th class="sv__acts-col">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="s in rows" :key="s.id" class="sv__row" @click="goDetail(s)">
          <td class="sv__name">
            <span class="sv__dot" :class="onlineOf(s) ? 'sv__dot--on' : 'sv__dot--off'" />
            {{ s.name }}
          </td>
          <td class="sv__url" :title="s.baseUrl">{{ s.baseUrl || '—' }}</td>
          <td>{{ s.arch || '—' }}</td>
          <td>{{ s.dockVer || '—' }}</td>
          <td>
            <span class="badge" :class="statusClass(s)">{{ statusText(s) }}</span>
          </td>
          <td>{{ formatAgo(s.lastSeen) }}</td>
          <td class="sv__acts">
            <button type="button" class="link" @click.stop="goDetail(s)">详情</button>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { fetchServers, isOnline, type Server } from '@/api/servers'
import { formatAgo } from '@/utils/format'

/** 与后端采集周期对齐（30s）：在线状态以后端 last_seen 为准，轮询只刷清单。 */
const REFRESH_MS = 30_000

const router = useRouter()
const rows = ref<Server[]>([])
const loading = ref(false)
const loaded = ref(false)
const error = ref<string | null>(null)
let timer: ReturnType<typeof setInterval> | undefined

const onlineCount = computed(() => rows.value.filter((s) => onlineOf(s)).length)

/** 禁用机不再被采集，一律算离线（与各页顶部切换器口径一致）。 */
function onlineOf(s: Server): boolean {
  return s.enabled && isOnline(s)
}

function statusText(s: Server): string {
  if (!s.enabled) return '已禁用'
  return onlineOf(s) ? '在线' : '离线'
}

function statusClass(s: Server): string {
  if (!s.enabled) return 'badge--off'
  return onlineOf(s) ? 'badge--ok' : 'badge--off'
}

async function load(): Promise<void> {
  loading.value = true
  try {
    rows.value = await fetchServers()
    error.value = null
    loaded.value = true
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

/** 进页面即拉一次并起表；离开务必停，否则定时器挂在后台（同总览/文件页纪律）。 */
function start(): void {
  void load()
  stop()
  timer = setInterval(() => void load(), REFRESH_MS)
}

function stop(): void {
  if (timer !== undefined) {
    clearInterval(timer)
    timer = undefined
  }
}

function goDetail(s: Server): void {
  void router.push({ name: 'host-detail', params: { id: s.id } })
}

function goAddNode(): void {
  void router.push({ name: 'add-node' })
}

onMounted(start)
onUnmounted(stop)
</script>

<style scoped>
.sv__head {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  align-items: flex-start;
  justify-content: space-between;
  margin-bottom: var(--space-5);
}

.sv__title {
  margin: 0;
  font-size: var(--text-2xl);
  font-weight: var(--weight-semibold);
}

.sv__sub {
  margin: var(--space-1) 0 0;
  color: var(--fg-subtle);
  font-size: var(--text-sm);
}

.sv__actions {
  display: flex;
  gap: var(--space-2);
}

.sv__err {
  margin: 0 0 var(--space-4);
  padding: var(--space-3) var(--space-4);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--fg-default);
  font-size: var(--text-sm);
}

.sv__empty {
  padding: var(--space-7) var(--space-4);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-lg);
  text-align: center;
}

.sv__empty-title {
  margin: 0 0 var(--space-2);
  font-size: var(--text-lg);
  font-weight: var(--weight-medium);
}

.sv__empty-hint {
  margin: 0 0 var(--space-4);
  color: var(--fg-subtle);
  font-size: var(--text-sm);
}

.sv__table {
  width: 100%;
  border-collapse: collapse;
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-lg);
  overflow: hidden;
}

.sv__table th {
  padding: var(--space-3) var(--space-4);
  background: var(--bg-elev-2);
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-muted);
  font-size: var(--text-xs);
  font-weight: var(--weight-medium);
  text-align: left;
  white-space: nowrap;
}

.sv__table td {
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border-subtle);
  font-size: var(--text-sm);
  vertical-align: middle;
}

.sv__table tbody tr:last-child td {
  border-bottom: none;
}

.sv__row {
  cursor: pointer;
}

.sv__row:hover {
  background: var(--bg-elev-2);
}

.sv__name {
  font-weight: var(--weight-medium);
  white-space: nowrap;
}

.sv__dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  margin-right: var(--space-2);
  border-radius: var(--radius-full);
  vertical-align: middle;
}

.sv__dot--on {
  background: var(--success);
}

.sv__dot--off {
  background: var(--fg-subtle);
}

.sv__url {
  max-width: 220px;
  overflow: hidden;
  color: var(--fg-muted);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.sv__acts {
  white-space: nowrap;
}

.sv__acts-col {
  width: 1%;
}

/* 手机端（<768px，PLAN-MOBILE 口径）：表格横向滚动，不转卡片——
   清单页信息密度低，一屏纵列全看完比横滑更省事。 */
@media (max-width: 767px) {
  .sv__table {
    display: block;
    overflow-x: auto;
    white-space: nowrap;
  }

  .sv__table thead,
  .sv__table tbody,
  .sv__table tr {
    display: revert;
  }
}
</style>
