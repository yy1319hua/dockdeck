<!--
  S7.3 容器列表：全局页 + 顶部服务器切换器（用户拍板），行内操作 + 二次确认，
  日志走同页抽屉（tail/时间戳/follow/导出）。

  数据口径（两个来源，分开降级）：
   * 身份与状态来自 Agent 实时转发（GET /servers/{id}/containers）——失败即错误；
   * CPU/内存占用来自 Hub 指标轮询快照（metrics_latest）——拿不到只显示「—」，
     不算失败（Agent 没跑 Docker 时整个为 null）。
   * 在线点以后端 last_seen 为准（isOnline），与总览页同一把尺。

  确认范围（用户拍板）：remove 输名确认（后端 guard 硬契约，confirm 须与 URL 里的
  cid 完全一致——故删除用容器名作 cid，让用户输名字而不是 64 位 ID）；
  stop/kill 简单确认；start/restart/pause/unpause 直接执行、toast 回报。
-->
<template>
  <div class="ct">
    <header class="ct__head">
      <div>
        <h1 class="ct__title">容器</h1>
        <p class="ct__sub">
          {{ ct.selected ? ct.selected.name : '未选择服务器' }} · {{ ct.rows.length }} 个容器<template v-if="ct.refreshing"> · 刷新中…</template>
        </p>
      </div>
      <button type="button" class="btn" :disabled="ct.loading" @click="ct.load()">刷新</button>
    </header>

    <div class="ct__bar">
      <label class="ct__host">
        服务器
        <select
          class="field__input ct__select"
          :value="ct.selectedId"
          :disabled="ct.servers.length === 0"
          @change="onSelectHost"
        >
          <option v-for="s in ct.servers" :key="s.id" :value="s.id">
            {{ s.name }}{{ isOnline(s) ? '' : s.enabled ? '（离线）' : '（已禁用）' }}
          </option>
        </select>
      </label>
      <label class="ct__all">
        <input type="checkbox" :checked="ct.showAll" @change="ct.toggleShowAll()" />
        显示已停止
      </label>
    </div>

    <p v-if="ct.error" class="ct__err">{{ ct.error }}</p>

    <table v-else-if="ct.rows.length > 0" class="ct__table">
      <thead>
        <tr>
          <th>容器</th>
          <th>镜像</th>
          <th>状态</th>
          <th class="num">CPU</th>
          <th class="num">内存</th>
          <th class="ct__acts-col">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="row in ct.rows" :key="row.container.id">
          <td class="ct__name" :title="row.container.name">{{ row.container.name }}</td>
          <td class="ct__image" :title="row.container.image">{{ row.container.image }}</td>
          <td>
            <span class="badge" :class="badgeClass(row.container.state)">{{ row.container.status || row.container.state }}</span>
          </td>
          <td class="num" :title="row.stat ? cpuTip(row.stat) : ''">
            {{ row.stat ? formatPercent(row.stat.cpu_percent_normalized) : '—' }}
            <span v-if="row.stat" class="ct__dim"> / {{ formatCores(row.stat.cpu_cores) }}</span>
          </td>
          <td class="num">{{ memText(row) }}</td>
          <td class="ct__acts">
            <template v-if="row.container.state === 'running'">
              <button type="button" class="link" :disabled="acting" @click="askStop(row)">停止</button>
              <button type="button" class="link" :disabled="acting" @click="direct(row, 'restart', '重启')">重启</button>
              <button type="button" class="link" :disabled="acting" @click="direct(row, 'pause', '暂停')">暂停</button>
              <button type="button" class="link link--warn" :disabled="acting" @click="askKill(row)">杀死</button>
            </template>
            <template v-else-if="row.container.state === 'paused'">
              <button type="button" class="link" :disabled="acting" @click="direct(row, 'unpause', '恢复')">恢复</button>
              <button type="button" class="link" :disabled="acting" @click="askStop(row)">停止</button>
              <button type="button" class="link link--warn" :disabled="acting" @click="askKill(row)">杀死</button>
            </template>
            <template v-else>
              <button type="button" class="link" :disabled="acting" @click="direct(row, 'start', '启动')">启动</button>
            </template>
            <button type="button" class="link" @click="openLogs(row)">日志</button>
            <button type="button" class="link" @click="openProcesses(row)">进程</button>
            <button type="button" class="link link--danger" :disabled="acting" @click="askRemove(row)">删除</button>
          </td>
        </tr>
      </tbody>
    </table>

    <!-- 手机态（<768px，PLAN-MOBILE M4）：表格退役转卡片列表，动作收进「···」底部面板。 -->
    <div v-if="!ct.loading && !ct.error && ct.rows.length > 0" class="ct__cards">
      <article v-for="row in ct.rows" :key="row.container.id" class="ccard">
        <header class="ccard__head">
          <span class="ccard__name" :title="row.container.name">{{ row.container.name }}</span>
          <span class="badge" :class="badgeClass(row.container.state)">{{ row.container.status || row.container.state }}</span>
        </header>
        <p class="ccard__image" :title="row.container.image">{{ row.container.image }}</p>
        <p class="ccard__stats">
          <span>CPU {{ row.stat ? formatPercent(row.stat.cpu_percent_normalized) : '—' }} / {{ row.stat ? formatCores(row.stat.cpu_cores) : '—' }}</span>
          <span>内存 {{ memText(row) }}</span>
        </p>
        <footer class="ccard__acts">
          <button type="button" class="btn btn--sm" :disabled="acting" @click="openLogs(row)">日志</button>
          <button type="button" class="btn btn--sm ccard__more" :disabled="acting" aria-label="更多操作" @click="openSheet(row)">···</button>
        </footer>
      </article>
    </div>

    <!-- 「···」动作底部面板：48px 触区行，遮罩点击关闭，锁 body 滚动。 -->
    <div v-if="sheetRow" class="sheet" @click.self="sheetRow = null">
      <div class="sheet__panel" role="dialog" aria-modal="true" :aria-label="`${sheetRow.container.name} 操作`">
        <p class="sheet__title">{{ sheetRow.container.name }}</p>
        <button
          v-for="a in sheetActions"
          :key="a.label"
          type="button"
          class="sheet__act"
          :class="{ 'sheet__act--danger': a.danger }"
          :disabled="acting"
          @click="a.run()"
        >
          {{ a.label }}
        </button>
        <button type="button" class="sheet__act sheet__act--cancel" @click="sheetRow = null">取消</button>
      </div>
    </div>

    <div v-if="!ct.loading && !ct.error && ct.rows.length === 0" class="ct__empty">
      <template v-if="ct.servers.length === 0">
        <p class="ct__empty-title">还没有纳管的服务器</p>
        <p class="ct__empty-hint">纳管方式见 S5.8：Hub 签发 join_secret，Agent 启动时自注册。</p>
      </template>
      <template v-else>
        <p class="ct__empty-title">没有容器</p>
        <p class="ct__empty-hint">该机当前没有运行中的容器；可勾选「显示已停止」查看全部。</p>
      </template>
    </div>

    <!-- 删除：输名确认 + force/v 选项 -->
    <AppConfirm
      :open="confirming?.kind === 'remove'"
      :title="`删除容器 ${confirming?.row.container.name ?? ''}`"
      :message="confirmMessage"
      danger
      :require-name="confirming?.row.container.name ?? ''"
      confirm-label="删除"
      @confirm="onConfirmAction"
      @cancel="confirming = null"
    >
      <label class="opt"><input v-model="removeForce" type="checkbox" /> 强制删除（容器运行中时必需）</label>
      <label class="opt"><input v-model="removeVolumes" type="checkbox" /> 连同匿名卷删除</label>
      <label class="opt"><input v-model="removeImage" type="checkbox" /> 联动删除镜像（被其他容器引用时自动跳过）</label>
      <label class="opt">
        <input v-model="removeDeployDir" type="checkbox" :disabled="!confirming?.row.container.isCompose" />
        联动删除 compose 部署目录
        <span v-if="!confirming?.row.container.isCompose" class="opt__hint">（非 compose 容器不可用）</span>
      </label>
    </AppConfirm>

    <!-- 停止：简单确认 + 可选超时 -->
    <AppConfirm
      :open="confirming?.kind === 'stop'"
      :title="`停止容器 ${confirming?.row.container.name ?? ''}`"
      :message="confirmMessage"
      @confirm="onConfirmAction"
      @cancel="confirming = null"
    >
      <label class="opt-row">
        停止超时（秒，留空用引擎默认）
        <input v-model="stopTimeout" class="field__input opt-row__input" type="number" min="0" step="1" placeholder="引擎默认" />
      </label>
    </AppConfirm>

    <!-- 杀死：简单确认 + 信号选择 -->
    <AppConfirm
      :open="confirming?.kind === 'kill'"
      :title="`杀死容器 ${confirming?.row.container.name ?? ''}`"
      :message="confirmMessage"
      danger
      @confirm="onConfirmAction"
      @cancel="confirming = null"
    >
      <label class="opt-row">
        信号
        <select v-model="killSignal" class="field__input opt-row__input">
          <option>SIGTERM</option>
          <option>SIGKILL</option>
          <option>SIGINT</option>
          <option>SIGHUP</option>
          <option>SIGQUIT</option>
          <option>SIGUSR1</option>
          <option>SIGUSR2</option>
        </select>
      </label>
    </AppConfirm>

    <ContainerLogs
      :open="logTarget !== null"
      :server-id="ct.selectedId"
      :cid="logTarget?.container.id ?? ''"
      :name="logTarget?.container.name ?? ''"
      @close="logTarget = null"
    />

    <ContainerProcesses
      :open="procTarget !== null"
      :server-id="ct.selectedId"
      :cid="procTarget?.container.id ?? ''"
      :name="procTarget?.container.name ?? ''"
      @close="procTarget = null"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import AppConfirm from '@/components/AppConfirm.vue'
import ContainerLogs from '@/components/ContainerLogs.vue'
import ContainerProcesses from '@/components/ContainerProcesses.vue'
import { containerAction, removeContainer } from '@/api/containers'
import { isOnline } from '@/api/servers'
import { formatBytes, formatCores, formatPercent } from '@/utils/format'
import { useContainersStore, type ContainerRow } from '@/stores/containers'
import { useUiStore } from '@/stores/ui'
import type { ContainerStatWire } from '@/api/metrics'

const ct = useContainersStore()
const ui = useUiStore()

onMounted(() => ct.start())
onUnmounted(() => ct.stop())

/** 任一写操作在飞时禁用全表操作按钮，防连点。 */
const acting = ref(false)

type ConfirmKind = 'remove' | 'stop' | 'kill'
const confirming = ref<{ kind: ConfirmKind; row: ContainerRow } | null>(null)
const removeForce = ref(false)
const removeVolumes = ref(false)
const removeImage = ref(true)
const removeDeployDir = ref(true)
const stopTimeout = ref('')
const killSignal = ref('SIGKILL')
const logTarget = ref<ContainerRow | null>(null)
const procTarget = ref<ContainerRow | null>(null)

// 「···」动作面板（M4）：手机态行内只留高频「日志」，其余收进底部面板。
const sheetRow = ref<ContainerRow | null>(null)

function openSheet(row: ContainerRow) {
  sheetRow.value = row
}

interface SheetAction {
  label: string
  danger?: boolean
  run: () => void
}

const sheetActions = computed<SheetAction[]>(() => {
  const row = sheetRow.value
  if (!row) return []
  const st = row.container.state
  const acts: SheetAction[] = []
  const wrap = (fn: () => void): (() => void) => () => {
    sheetRow.value = null
    fn()
  }
  if (st === 'running') {
    acts.push({ label: '重启', run: wrap(() => direct(row, 'restart', '重启')) })
    acts.push({ label: '暂停', run: wrap(() => direct(row, 'pause', '暂停')) })
    acts.push({ label: '停止…', run: wrap(() => askStop(row)) })
    acts.push({ label: '杀死…', danger: true, run: wrap(() => askKill(row)) })
  } else if (st === 'paused') {
    acts.push({ label: '恢复', run: wrap(() => direct(row, 'unpause', '恢复')) })
    acts.push({ label: '停止…', run: wrap(() => askStop(row)) })
    acts.push({ label: '杀死…', danger: true, run: wrap(() => askKill(row)) })
  } else {
    acts.push({ label: '启动', run: wrap(() => direct(row, 'start', '启动')) })
  }
  acts.push({ label: '进程', run: wrap(() => openProcesses(row)) })
  acts.push({ label: '日志', run: wrap(() => openLogs(row)) })
  acts.push({ label: '删除…', danger: true, run: wrap(() => askRemove(row)) })
  return acts
})

// 面板开着时锁 body 滚动（#12 口径）。
watch(sheetRow, (v) => {
  document.body.style.overflow = v ? 'hidden' : ''
})
onUnmounted(() => {
  document.body.style.overflow = ''
})

const confirmMessage = computed(() => {
  const name = confirming.value?.row.container.name ?? ''
  switch (confirming.value?.kind) {
    case 'remove':
      return `删除不可恢复。请输入 ${name} 以确认。`
    case 'stop':
      return `将向 ${name} 发送停止信号，正在处理中的请求会中断。`
    case 'kill':
      return killSignal.value === 'SIGTERM'
        ? `将向 ${name} 发送 SIGTERM，容器可优雅退出。`
        : `将立即向 ${name} 发送 ${killSignal.value}，不给优雅退出的机会。`
    default:
      return ''
  }
})

function badgeClass(state: string): string {
  switch (state) {
    case 'running':
      return 'badge--ok'
    case 'paused':
      return 'badge--warn'
    case 'restarting':
      return 'badge--info'
    default:
      return 'badge--off'
  }
}

// CPU 口径说明：主值是整机归一（与主机同量纲），同时给出 docker stats 原始口径便于排障。
function cpuTip(s: ContainerStatWire): string {
  return [
    '主值为整机归一口径（与主机 CPU 同量纲，可直接比较）。',
    `占用核数：${formatCores(s.cpu_cores)}。`,
    `docker stats 原始口径：${formatPercent(s.cpu_percent)}（单核百分比，100% = 吃满一个核，可超过 100%）。`,
  ].join('\n')
}

function memText(row: ContainerRow): string {
  const s = row.stat
  if (!s) {
    return '—'
  }
  // MemLimit 为 0 表示未设内存上限，此时百分比无意义（protocol 约定，不展示）。
  const pct = s.mem_limit > 0 ? ` / ${formatPercent(s.mem_percent)}` : ''
  return `${formatBytes(s.mem_usage)}${pct}`
}

function onSelectHost(e: Event) {
  const v = Number((e.target as HTMLSelectElement).value)
  if (Number.isFinite(v) && v > 0) {
    void ct.selectServer(v)
  }
}

/** 无需确认的动作：执行 → toast → 重拉清单反映新状态。 */
async function direct(row: ContainerRow, action: 'start' | 'restart' | 'pause' | 'unpause', label: string) {
  if (acting.value) {
    return
  }
  acting.value = true
  try {
    await containerAction(ct.selectedId, row.container.id, action)
    ui.notifyNotice(`已${label} ${row.container.name}`)
    await ct.load()
  } catch (e) {
    ui.notifyError(e)
  } finally {
    acting.value = false
  }
}

function askStop(row: ContainerRow) {
  stopTimeout.value = ''
  confirming.value = { kind: 'stop', row }
}

function askKill(row: ContainerRow) {
  killSignal.value = 'SIGKILL'
  confirming.value = { kind: 'kill', row }
}

function askRemove(row: ContainerRow) {
  removeForce.value = false
  removeVolumes.value = false
  // 联动清理默认全勾（#90）：每次打开弹窗都回到「默认清理」状态。
  removeImage.value = true
  removeDeployDir.value = true
  confirming.value = { kind: 'remove', row }
}

function openLogs(row: ContainerRow) {
  logTarget.value = row
}

function openProcesses(row: ContainerRow) {
  procTarget.value = row
}

async function onConfirmAction() {
  const cur = confirming.value
  if (!cur) {
    return
  }
  confirming.value = null
  acting.value = true
  try {
    if (cur.kind === 'remove') {
      // cid 用容器名：后端 confirm 须与 URL 里的 cid 完全一致，
      // 让用户输名字而不是 64 位 ID（Engine 接受名字作容器引用）。
      const res = await removeContainer(ct.selectedId, cur.row.container.name, {
        force: removeForce.value,
        volumes: removeVolumes.value,
        removeImage: removeImage.value,
        removeDeployDir: removeDeployDir.value,
        confirm: cur.row.container.name,
      })
      ui.notifyNotice(`已删除 ${cur.row.container.name}`)
      // 联动清理结果逐条回显（删镜像跳过原因 / 部署目录清理结果等）。
      for (const m of res.messages ?? []) {
        ui.notifyNotice(m)
      }
    } else if (cur.kind === 'stop') {
      const t = stopTimeout.value.trim()
      await containerAction(ct.selectedId, cur.row.container.id, 'stop', t ? { timeout_sec: Number(t) } : {})
      ui.notifyNotice(`已停止 ${cur.row.container.name}`)
    } else {
      await containerAction(ct.selectedId, cur.row.container.id, 'kill', { signal: killSignal.value })
      ui.notifyNotice(`已向 ${cur.row.container.name} 发送 ${killSignal.value}`)
    }
    await ct.load()
  } catch (e) {
    ui.notifyError(e)
  } finally {
    acting.value = false
  }
}
</script>

<style scoped>
.ct {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.ct__head {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.ct__title {
  font-size: var(--text-xl);
}

.ct__sub {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.ct__head .btn {
  margin-left: auto;
  flex: none;
}

.ct__bar {
  display: flex;
  align-items: center;
  gap: var(--space-4);
  flex-wrap: wrap;
}

.ct__host,
.ct__all {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.ct__select {
  height: 32px;
  width: auto;
  min-width: 180px;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

.ct__err {
  padding: var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

.ct__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

/* 手机态互斥（M4）：桌面只见表格，手机只见卡片。 */
.ct__cards {
  display: none;
}

@media (max-width: 767px) {
  .ct__table {
    display: none;
  }

  .ct__cards {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
}

.ccard {
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  padding: var(--space-3);
}

.ccard__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
}

.ccard__name {
  font-weight: var(--weight-medium);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ccard__image {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-xs);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ccard__stats {
  margin-top: var(--space-2);
  display: flex;
  gap: var(--space-4);
  font-size: var(--text-sm);
  font-variant-numeric: tabular-nums;
}

.ccard__acts {
  margin-top: var(--space-3);
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
}

.ccard__more {
  min-width: 48px;
}

.sheet {
  position: fixed;
  inset: 0;
  z-index: var(--z-modal);
  background: rgb(0 0 0 / 50%);
  display: flex;
  align-items: flex-end;
}

.sheet__panel {
  width: 100%;
  background: var(--bg-elev-1);
  border-top: 1px solid var(--border-default);
  border-radius: var(--radius-lg) var(--radius-lg) 0 0;
  padding: var(--space-3) var(--space-3) calc(var(--space-3) + env(safe-area-inset-bottom));
  display: flex;
  flex-direction: column;
}

.sheet__title {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
  text-align: center;
  padding-bottom: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.sheet__act {
  background: none;
  border: none;
  border-bottom: 1px solid var(--border-subtle);
  color: var(--fg-default);
  font-size: var(--text-base);
  text-align: center;
  padding: var(--space-3);
  min-height: 48px;
  cursor: pointer;
}

.sheet__act:disabled {
  color: var(--fg-subtle);
}

.sheet__act--danger {
  color: var(--danger);
}

.sheet__act--cancel {
  color: var(--fg-muted);
  border-bottom: none;
}

.ct__table th {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-subtle);
  font-weight: var(--weight-normal);
  text-align: left;
}

.ct__table td {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
  vertical-align: middle;
}

.ct__table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.ct__dim {
  color: var(--fg-subtle);
}

.ct__name {
  max-width: 200px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-weight: var(--weight-medium);
}

.ct__image {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--fg-muted);
}

.badge {
  display: inline-block;
  padding: 0 var(--space-2);
  border-radius: var(--radius-sm);
  font-size: var(--text-xs);
  white-space: nowrap;
}

.badge--ok {
  background: var(--success-subtle);
  border: 1px solid var(--success-border);
  color: var(--success);
}

.badge--warn {
  background: var(--warning-subtle);
  border: 1px solid var(--warning-border);
  color: var(--warning);
}

.badge--info {
  background: var(--accent-subtle);
  border: 1px solid var(--accent-border);
  color: var(--fg-default);
}

.badge--off {
  background: var(--bg-elev-2);
  border: 1px solid var(--border-default);
  color: var(--fg-subtle);
}

.ct__acts-col {
  width: 1%;
  white-space: nowrap;
}

.ct__acts {
  white-space: nowrap;
}

.ct__empty {
  padding: var(--space-6);
  background: var(--bg-elev-1);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-md);
  text-align: center;
}

.ct__empty-title {
  color: var(--fg-muted);
  font-size: var(--text-base);
}

.ct__empty-hint {
  margin-top: var(--space-2);
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.opt,
.opt-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.opt-row__input {
  height: 32px;
  width: 140px;
  font-size: var(--text-sm);
}
</style>
