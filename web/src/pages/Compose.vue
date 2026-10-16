<!--
  S7.5 Compose 页：全局页 + 顶部主机切换器（与容器/镜像页同构，用户拍板）。
  消费 S4.2/S4.3/S4.7 已落地的六个端点：项目清单、四动作、读文件、diff、PUT。

  确认范围（用户拍板 2026-10-16，沿用 S7.3 房规）：
   * up / restart / pull —— 直行 + toast 回报 compose 输出首行（白名单动词，
                          业务成败写在输出里而非状态码，完整输出进 <details>）；
   * down               —— 简单确认（一次停该项目全部容器，面广但可逆）；
   * 写入文件            —— 两段式：先 diff 预览（不落盘、不产生备份），用户看过
                          变更再弹带 +N/-N 统计的确认框，PUT 由 Agent 侧先备份
                          （.bak.<RFC3339>）后覆盖，返回备份路径 toast 展示。

  编辑器：纯 textarea + --font-mono（零依赖原则，不引任何编辑器库）；
  diff 着色：按代码 diff 通行约定（增行绿、删行红）——这是代码语义，
  与金融口径涨红跌绿相反，别「统一」错方向。
-->
<template>
  <div class="cp">
    <header class="cp__head">
      <div>
        <h1 class="cp__title">Compose</h1>
        <p class="cp__sub">
          {{ cp.selected ? cp.selected.name : '未选择主机' }} · {{ cp.projects.length }} 个项目<template v-if="cp.refreshing"> · 刷新中…</template>
        </p>
      </div>
      <button type="button" class="btn" :disabled="cp.loading" @click="cp.load()">刷新</button>
    </header>

    <div class="cp__bar">
      <label class="cp__host">
        主机
        <select
          class="field__input cp__select"
          :value="cp.selectedId"
          :disabled="cp.servers.length === 0 || editing !== null"
          :title="editing !== null ? '编辑文件期间不能切换主机' : ''"
          @change="onSelectHost"
        >
          <option v-for="s in cp.servers" :key="s.id" :value="s.id">
            {{ s.name }}{{ isOnline(s) ? '' : s.enabled ? '（离线）' : '（已禁用）' }}
          </option>
        </select>
      </label>
      <span v-if="editing !== null" class="cp__bar-hint">正在编辑 {{ editing.name }}，返回列表后才能切换主机</span>
    </div>

    <p v-if="cp.error && editing === null" class="cp__err">{{ cp.error }}</p>

    <!-- 编辑面板：选中项目后主体切换。编辑态是页面本地 ref，不进 store
         （临时 UI 态跨路由存活问题，见 stores/compose.ts 注释）。 -->
    <section v-else-if="editing !== null" class="cp__edit">
      <div class="cp__edit-head">
        <div class="cp__edit-id">
          <h2 class="cp__edit-title">编辑 {{ editing.name }}</h2>
          <p class="cp__edit-path" :title="configFile">{{ fileLoading ? '（读取中…）' : configFile }}</p>
        </div>
        <span v-if="!fileLoading" class="cp__edit-state" :class="dirty ? 'cp__edit-state--dirty' : 'cp__edit-state--saved'">
          {{ dirty ? '有未保存修改' : '已保存' }}
        </span>
      </div>

      <p v-if="cp.error" class="cp__edit-err">后台刷新失败：{{ cp.error }}（不影响编辑，写入成功后会重试）</p>

      <p v-if="fileLoading" class="cp__edit-loading">正在读取 compose 文件…</p>
      <template v-else>
        <textarea
          v-model="draft"
          class="cp__editor"
          spellcheck="false"
          :disabled="writing"
          placeholder="compose 文件内容"
          @keydown.tab.prevent="onEditorTab"
        />

        <div class="cp__tools">
          <button type="button" class="btn" :disabled="!dirty || diffLoading || writing" @click="onPreview">
            {{ diffLoading ? '计算中…' : '预览变更' }}
          </button>
          <button type="button" class="btn btn--primary" :disabled="!canWrite" @click="writeAsk = true">
            {{ writing ? '写入中…' : '写入' }}
          </button>
          <button type="button" class="btn" :disabled="writing" @click="onLeaveEdit">返回列表</button>
          <span v-if="previewing && diffText.trim() !== ''" class="cp__tools-stat">
            +{{ diffStats.added }} / -{{ diffStats.removed }}
          </span>
        </div>

        <div v-if="previewing" class="cp__diff-wrap">
          <p v-if="diffText.trim() === ''" class="cp__diff-empty">与宿主机当前文件相比无变化。</p>
          <pre v-else class="cp__diff"><span
              v-for="(line, i) in diffLines"
              :key="i"
              class="cp__diff-line"
              :class="diffLineClass(line)"
            >{{ line || ' ' }}</span></pre>
        </div>
      </template>
    </section>

    <table v-else-if="cp.projects.length > 0" class="cp__table">
      <thead>
        <tr>
          <th>项目</th>
          <th>状态</th>
          <th class="num">服务</th>
          <th>compose 文件</th>
          <th class="cp__acts-col">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="p in cp.projects" :key="p.name">
          <td class="cp__name" :title="p.path">{{ p.name }}</td>
          <td>
            <span class="cp__badge" :class="`cp__badge--${p.status}`">{{ statusLabel(p.status) }}</span>
          </td>
          <td class="num">{{ p.services.length }}</td>
          <td class="cp__file" :title="p.configFile">{{ p.configFile }}</td>
          <td class="cp__acts">
            <button type="button" class="link" :disabled="acting" @click="openEdit(p)">编辑</button>
            <button
              v-for="a in ACTIONS"
              :key="a.name"
              type="button"
              class="link"
              :class="a.danger ? 'link--danger' : ''"
              :disabled="acting"
              @click="askAction(a, p)"
            >
              {{ a.label }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>

    <div v-else-if="!cp.loading && !cp.error" class="cp__empty">
      <template v-if="cp.servers.length === 0">
        <p class="cp__empty-title">还没有纳管的主机</p>
        <p class="cp__empty-hint">纳管方式见 S5.8：Hub 签发 join_secret，Agent 启动时自注册。</p>
      </template>
      <template v-else>
        <p class="cp__empty-title">没有 compose 项目</p>
        <p class="cp__empty-hint">Agent 只扫描宿主机 compose 白名单目录下的 docker-compose.yml / compose.yaml，未发现项目。</p>
      </template>
    </div>

    <!-- 停止：简单确认（操作可逆，但一次停全部容器，需人点头） -->
    <AppConfirm
      :open="downTarget !== null"
      :title="`停止项目 ${downTarget ? downTarget.name : ''}`"
      message="将停止该项目全部容器。项目定义与数据卷不受影响，之后可随时重新启动。"
      danger
      confirm-label="停止"
      @confirm="onConfirmDown"
      @cancel="downTarget = null"
    />

    <!-- 写入：两段式第二段。diff 预览（第一段）不落盘不备份；这里带变更统计与备份说明。 -->
    <AppConfirm
      :open="writeAsk"
      title="写入 compose 文件"
      :message="writeMessage"
      confirm-label="写入"
      @confirm="onConfirmWrite"
      @cancel="writeAsk = false"
    />

    <!-- 编辑中返回列表：有未保存修改，放弃确认 -->
    <AppConfirm
      :open="closeAsk"
      title="放弃未保存的修改"
      message="有未保存的修改，返回列表后将丢弃这些修改。宿主机文件当前内容不变。"
      danger
      confirm-label="放弃修改"
      @confirm="onConfirmClose"
      @cancel="closeAsk = false"
    />

    <!-- 最近一次 compose 动作的完整输出（业务成败写在流内，toast 只展示首行） -->
    <details v-if="lastOutput" class="cp__output">
      <summary>最近一次 compose 输出</summary>
      <pre>{{ lastOutput }}</pre>
    </details>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import AppConfirm from '@/components/AppConfirm.vue'
import {
  composeAction,
  diffComposeFile,
  fetchComposeFile,
  putComposeFile,
  type ComposeActionName,
  type ComposeProject,
  type ComposeStatus,
} from '@/api/compose'
import { isOnline } from '@/api/servers'
import { useComposeStore } from '@/stores/compose'
import { useUiStore } from '@/stores/ui'

const cp = useComposeStore()
const ui = useUiStore()

onMounted(() => cp.start())
onUnmounted(() => cp.stop())

/** 任一 compose 动作在飞时禁用全表操作按钮，防连点。 */
const acting = ref(false)

/* ---------- 项目清单操作 ---------- */

interface ActionDef {
  name: ComposeActionName
  label: string
  danger?: boolean
}

/** 四个白名单动词（与 protocol.ValidComposeAction 一致）。up/restart/pull 直行；down 走简单确认。 */
const ACTIONS: ActionDef[] = [
  { name: 'up', label: '启动' },
  { name: 'restart', label: '重启' },
  { name: 'pull', label: '拉取' },
  { name: 'down', label: '停止', danger: true },
]

const STATUS_LABELS: Record<ComposeStatus, string> = {
  running: '运行中',
  partial: '部分运行',
  stopped: '已停止',
  unknown: '未知',
}

function statusLabel(s: ComposeStatus): string {
  return STATUS_LABELS[s] ?? s
}

const downTarget = ref<ComposeProject | null>(null)
const lastOutput = ref('')

/** compose 输出多行，toast 只取首行非空行；完整输出进 <details>。 */
function firstLine(output: string): string {
  const line = output.split('\n').find((l) => l.trim().length > 0)
  return line ? line.trim().slice(0, 120) : '完成'
}

function askAction(a: ActionDef, p: ComposeProject): void {
  if (a.name === 'down') {
    downTarget.value = p
    return
  }
  void runAction(a.name, p)
}

async function runAction(action: ComposeActionName, p: ComposeProject): Promise<void> {
  if (acting.value) {
    return
  }
  acting.value = true
  try {
    const r = await composeAction(cp.selectedId, p.name, action)
    lastOutput.value = `$ docker compose ${action} ${p.name}\n${r.output}`
    ui.notifyNotice(`${p.name}：${firstLine(r.output)}`)
    // 动作后项目状态/服务快照可能已变，重新拉清单。
    await cp.reload()
  } catch (e) {
    ui.notifyError(e)
  } finally {
    acting.value = false
  }
}

async function onConfirmDown(): Promise<void> {
  const p = downTarget.value
  downTarget.value = null
  if (p) {
    await runAction('down', p)
  }
}

/* ---------- 编辑面板（页面本地态，不进 store） ---------- */

const editing = ref<ComposeProject | null>(null)
const fileLoading = ref(false)
const configFile = ref('')
const original = ref('')
const draft = ref('')
const diffText = ref('')
const previewing = ref(false)
const diffLoading = ref(false)
const writeAsk = ref(false)
const writing = ref(false)
const closeAsk = ref(false)

const dirty = computed(() => editing.value !== null && draft.value !== original.value)
const canWrite = computed(() => dirty.value && previewing.value && diffText.value.trim() !== '' && !writing.value)

const diffLines = computed(() => diffText.value.split('\n'))

const diffStats = computed(() => {
  let added = 0
  let removed = 0
  for (const line of diffText.value.split('\n')) {
    // 跳过 +++/--- 文件头行，它们不是增删内容。
    if (line.startsWith('+++') || line.startsWith('---')) {
      continue
    }
    if (line.startsWith('+')) {
      added += 1
    } else if (line.startsWith('-')) {
      removed += 1
    }
  }
  return { added, removed }
})

const writeMessage = computed(() =>
  `将覆盖写入 ${editing.value ? editing.value.name : ''} 的 compose 文件（+${diffStats.value.added} / -${diffStats.value.removed} 行）。写入前会自动备份原文件为 .bak.<时间戳>，备份落盘后才会覆盖。`,
)

function diffLineClass(line: string): string {
  if (line.startsWith('@@')) {
    return 'cp__diff-hunk'
  }
  if (line.startsWith('+++') || line.startsWith('---')) {
    return 'cp__diff-meta'
  }
  if (line.startsWith('+')) {
    return 'cp__diff-add'
  }
  if (line.startsWith('-')) {
    return 'cp__diff-del'
  }
  return ''
}

/** Tab 插入两空格而非切焦点——YAML 缩进全靠它。 */
async function onEditorTab(e: KeyboardEvent): Promise<void> {
  const ta = e.target as HTMLTextAreaElement
  const s = ta.selectionStart
  const t = ta.selectionEnd
  if (s === null || t === null) {
    return
  }
  draft.value = draft.value.slice(0, s) + '  ' + draft.value.slice(t)
  await nextTick()
  ta.selectionStart = ta.selectionEnd = s + 2
}

async function openEdit(p: ComposeProject): Promise<void> {
  if (fileLoading.value) {
    return
  }
  editing.value = p
  fileLoading.value = true
  configFile.value = ''
  original.value = ''
  draft.value = ''
  diffText.value = ''
  previewing.value = false
  try {
    const f = await fetchComposeFile(cp.selectedId, p.name)
    // 读取期间用户可能已离开面板（切换器编辑中禁用，双保险）；对象变了就丢弃结果。
    if (editing.value !== p) {
      return
    }
    configFile.value = f.configFile
    draft.value = f.content
    original.value = f.content
  } catch (e) {
    ui.notifyError(e)
    editing.value = null
  } finally {
    fileLoading.value = false
  }
}

/** 两段式第一段：算 diff（不落盘、不备份）。无变化时给出提示，写入保持禁用。 */
async function onPreview(): Promise<void> {
  if (diffLoading.value || editing.value === null) {
    return
  }
  diffLoading.value = true
  try {
    diffText.value = await diffComposeFile(cp.selectedId, editing.value.name, draft.value)
    previewing.value = true
  } catch (e) {
    ui.notifyError(e)
  } finally {
    diffLoading.value = false
  }
}

/** 两段式第二段：PUT 覆盖写（Agent 侧先备份）。成功后原值跟进草稿，留在面板可继续改。 */
async function onConfirmWrite(): Promise<void> {
  const p = editing.value
  if (p === null || writing.value) {
    return
  }
  writeAsk.value = false
  writing.value = true
  try {
    const backup = await putComposeFile(cp.selectedId, p.name, draft.value)
    ui.notifyNotice(`已写入 ${p.name}，原文件备份至 ${backup}`)
    original.value = draft.value
    diffText.value = ''
    previewing.value = false
    await cp.reload()
  } catch (e) {
    ui.notifyError(e)
  } finally {
    writing.value = false
  }
}

function onLeaveEdit(): void {
  if (editing.value === null) {
    return
  }
  if (dirty.value) {
    closeAsk.value = true
    return
  }
  editing.value = null
}

function onConfirmClose(): void {
  closeAsk.value = false
  editing.value = null
  configFile.value = ''
  original.value = ''
  draft.value = ''
  diffText.value = ''
  previewing.value = false
}

/* ---------- 杂项 ---------- */

function onSelectHost(e: Event): void {
  const v = Number((e.target as HTMLSelectElement).value)
  if (Number.isFinite(v) && v > 0) {
    void cp.selectServer(v)
  }
}
</script>

<style scoped>
.cp {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.cp__head {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.cp__title {
  font-size: var(--text-xl);
}

.cp__sub {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.cp__head .btn {
  margin-left: auto;
  flex: none;
}

.cp__bar {
  display: flex;
  align-items: center;
  gap: var(--space-4);
  flex-wrap: wrap;
}

.cp__host {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.cp__select {
  height: 32px;
  width: auto;
  min-width: 180px;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

.cp__bar-hint {
  color: var(--warning);
  font-size: var(--text-sm);
}

.cp__err,
.cp__edit-err {
  padding: var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

/* ---------- 项目表 ---------- */

.cp__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.cp__table th {
  padding: var(--space-2);
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

.cp__name {
  font-weight: var(--weight-medium);
}

.cp__file {
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--fg-muted);
}

.cp__acts {
  white-space: nowrap;
}

.cp__acts-col {
  width: 1%;
}

.cp__badge {
  display: inline-block;
  padding: 0 var(--space-2);
  border-radius: var(--radius-sm);
  font-size: var(--text-xs);
  line-height: 1.8;
}

.cp__badge--running {
  background: var(--success-subtle);
  border: 1px solid var(--success-border);
  color: var(--success);
}

.cp__badge--partial {
  background: var(--warning-subtle);
  border: 1px solid var(--warning-border);
  color: var(--warning);
}

.cp__badge--stopped {
  background: var(--bg-elev-2);
  border: 1px solid var(--border-default);
  color: var(--fg-muted);
}

.cp__badge--unknown {
  background: var(--bg-elev-1);
  border: 1px dashed var(--border-default);
  color: var(--fg-subtle);
}

.link {
  padding: 0 var(--space-1);
  background: none;
  border: none;
  color: var(--accent);
  font-size: var(--text-sm);
  cursor: pointer;
}

.link:hover:not(:disabled) {
  color: var(--accent-hover);
  text-decoration: underline;
}

.link:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.link--danger {
  color: var(--danger);
}

/* ---------- 空态 ---------- */

.cp__empty {
  padding: var(--space-6);
  background: var(--bg-elev-1);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-md);
  text-align: center;
}

.cp__empty-title {
  font-weight: var(--weight-medium);
}

.cp__empty-hint {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

/* ---------- 编辑面板 ---------- */

.cp__edit {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.cp__edit-head {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.cp__edit-id {
  min-width: 0;
}

.cp__edit-title {
  font-size: var(--text-lg);
  font-weight: var(--weight-semibold);
}

.cp__edit-path {
  margin-top: var(--space-1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.cp__edit-state {
  margin-left: auto;
  flex: none;
  padding: 0 var(--space-2);
  border-radius: var(--radius-sm);
  font-size: var(--text-xs);
  line-height: 1.8;
}

.cp__edit-state--saved {
  background: var(--success-subtle);
  border: 1px solid var(--success-border);
  color: var(--success);
}

.cp__edit-state--dirty {
  background: var(--warning-subtle);
  border: 1px solid var(--warning-border);
  color: var(--warning);
}

.cp__edit-loading {
  padding: var(--space-6);
  text-align: center;
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.cp__editor {
  width: 100%;
  min-height: 360px;
  padding: var(--space-3);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  line-height: 1.6;
  resize: vertical;
}

.cp__editor:focus {
  border-color: var(--accent);
  outline: none;
}

.cp__editor:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

.cp__tools {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}

.cp__tools-stat {
  color: var(--fg-muted);
  font-size: var(--text-sm);
  font-variant-numeric: tabular-nums;
}

.cp__diff-wrap {
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  overflow: hidden;
}

.cp__diff {
  margin: 0;
  padding: var(--space-3);
  max-height: 420px;
  overflow: auto;
  background: var(--bg-elev-1);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: 1.6;
}

.cp__diff-line {
  display: block;
  white-space: pre;
}

.cp__diff-add {
  background: var(--success-subtle);
  color: var(--success);
}

.cp__diff-del {
  background: var(--danger-subtle);
  color: var(--danger);
}

.cp__diff-hunk {
  color: var(--fg-subtle);
}

.cp__diff-meta {
  color: var(--fg-subtle);
  font-weight: var(--weight-medium);
}

.cp__diff-empty {
  padding: var(--space-3);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

/* ---------- compose 完整输出 ---------- */

.cp__output {
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  background: var(--bg-elev-1);
}

.cp__output summary {
  padding: var(--space-2) var(--space-3);
  color: var(--fg-muted);
  font-size: var(--text-sm);
  cursor: pointer;
}

.cp__output pre {
  margin: 0;
  padding: 0 var(--space-3) var(--space-3);
  max-height: 320px;
  overflow: auto;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  white-space: pre-wrap;
}
</style>
