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

        <!-- 写前备份（S7.11）：只读分区，与编辑面板同页而不是新开一页（Q6-A）。
             预览复用现有 diff 端点——拿备份正文去算「与当前文件的差」，
             与写入的两段式完全同款；不提供删除，备份只增不减。 -->
        <section class="cp__bk">
          <div class="cp__bk-head">
            <button type="button" class="link" :disabled="bkLoading" @click="onToggleBackups">
              {{ backupsOpen ? '收起' : '展开' }}写前备份<template v-if="backups.length > 0">（{{ backups.length }}）</template>
            </button>
            <button v-if="backupsOpen" type="button" class="link" :disabled="bkLoading || bkBusy" @click="loadBackups">
              刷新
            </button>
            <span class="cp__bk-hint">只读 · 不提供删除</span>
          </div>

          <template v-if="backupsOpen">
            <p v-if="bkLoading" class="cp__bk-loading">正在读取备份清单…</p>
            <table v-else-if="backups.length > 0" class="cp__bk-table">
              <thead>
                <tr>
                  <th>备份文件</th>
                  <th class="num">大小</th>
                  <th>时间</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="b in backups" :key="b.name">
                  <td class="cp__bk-name" :title="b.name">{{ b.name }}</td>
                  <td class="num">{{ formatBytes(b.size) }}</td>
                  <td>{{ formatRFC3339(b.modTime) }}</td>
                  <td class="cp__bk-acts">
                    <button type="button" class="link" :disabled="bkBusy" @click="onPreviewBackup(b.name)">预览</button>
                    <button type="button" class="link link--danger" :disabled="bkBusy" @click="onAskRestore(b.name)">恢复</button>
                  </td>
                </tr>
              </tbody>
            </table>
            <p v-else class="cp__bk-empty">这个项目还没有写前备份——每次写入 compose 文件都会自动生成一份。</p>

            <div v-if="bkPreviewName" class="cp__bk-prev">
              <p class="cp__bk-prev-head">
                与当前文件相比：{{ bkPreviewName }}
                <span v-if="bkDiff.trim() !== ''" class="cp__tools-stat">+{{ bkStats.added }} / -{{ bkStats.removed }}</span>
                <span v-else>（两者无差异）</span>
              </p>
              <pre v-if="bkDiff.trim() !== ''" class="cp__diff"><span
                  v-for="(line, i) in bkDiffLines"
                  :key="i"
                  class="cp__diff-line"
                  :class="diffLineClass(line)"
                >{{ line || ' ' }}</span></pre>
            </div>
          </template>
        </section>
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

    <!-- 恢复备份：同样是覆盖写，故同样两段式——预览（上一步）看过差异再确认。
         文案必须点明「改 YAML 不会直接改动容器，需再执行启动」。 -->
    <AppConfirm
      :open="restoreAsk"
      title="恢复 compose 备份"
      :message="restoreMessage"
      confirm-label="恢复"
      @confirm="onConfirmRestore"
      @cancel="restoreAsk = false"
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
  fetchComposeBackupContent,
  fetchComposeBackups,
  fetchComposeFile,
  putComposeFile,
  restoreComposeBackup,
  type ComposeActionName,
  type ComposeBackup,
  type ComposeProject,
  type ComposeStatus,
} from '@/api/compose'
import { isOnline } from '@/api/servers'
import { formatBytes, formatRFC3339 } from '@/utils/format'
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

/* ---------- 写前备份（S7.11，页面本地态，不进 store——同编辑态纪律） ---------- */

const backupsOpen = ref(false)
const backups = ref<ComposeBackup[]>([])
const bkLoading = ref(false)
/** 预览/恢复在飞：两种都要等网络，禁掉重复点击。 */
const bkBusy = ref(false)
const bkPreviewName = ref('')
const bkDiff = ref('')
const restoreAsk = ref(false)
const restoreTarget = ref('')

const bkDiffLines = computed(() => bkDiff.value.split('\n'))

/** 与上方的 diffStats 同算法，但作用在备份 diff 上（不复用同一份状态：
 *  写入流程的「有未保存修改」判定不能被备份预览搅乱）。 */
const bkStats = computed(() => {
  let added = 0
  let removed = 0
  for (const line of bkDiff.value.split('\n')) {
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

const restoreMessage = computed(
  () =>
    `将用备份 ${restoreTarget.value} 覆盖 ${editing.value ? editing.value.name : ''} 当前的 compose 文件。` +
    `恢复前会先把当前内容备份为新的 .bak.<时间戳>（所以回滚不是单向门）。` +
    `注意：改 YAML 不会直接改动运行中的容器，需再执行「启动」才生效。`,
)

/** 换项目/退出面板时清空备份态——它绑定的是「某一个项目」，不能带过去。 */
function resetBackups(): void {
  backupsOpen.value = false
  backups.value = []
  bkPreviewName.value = ''
  bkDiff.value = ''
  restoreAsk.value = false
  restoreTarget.value = ''
}

async function loadBackups(): Promise<void> {
  const p = editing.value
  if (p === null) {
    return
  }
  bkLoading.value = true
  try {
    backups.value = await fetchComposeBackups(cp.selectedId, p.name)
  } catch (e) {
    ui.notifyError(e)
  } finally {
    bkLoading.value = false
  }
}

/** 懒加载：展开时才拉清单（多数编辑会话根本不看备份，不必每次进面板都发请求）。 */
async function onToggleBackups(): Promise<void> {
  if (backupsOpen.value) {
    backupsOpen.value = false
    return
  }
  backupsOpen.value = true
  if (backups.value.length === 0 && !bkLoading.value) {
    await loadBackups()
  }
}

/** 预览：拿备份正文去调现有 diff 端点算「与当前文件的差」——不新开预览端点（Q1-A）。 */
async function onPreviewBackup(name: string): Promise<void> {
  const p = editing.value
  if (p === null || bkBusy.value) {
    return
  }
  bkBusy.value = true
  try {
    const content = await fetchComposeBackupContent(cp.selectedId, p.name, name)
    bkDiff.value = await diffComposeFile(cp.selectedId, p.name, content)
    bkPreviewName.value = name
  } catch (e) {
    ui.notifyError(e)
  } finally {
    bkBusy.value = false
  }
}

function onAskRestore(name: string): void {
  restoreTarget.value = name
  restoreAsk.value = true
}

async function onConfirmRestore(): Promise<void> {
  const p = editing.value
  const name = restoreTarget.value
  restoreAsk.value = false
  restoreTarget.value = ''
  if (p === null || bkBusy.value) {
    return
  }
  bkBusy.value = true
  try {
    const backup = await restoreComposeBackup(cp.selectedId, p.name, name)
    ui.notifyNotice(`已用 ${name} 恢复 ${p.name}，恢复前的内容备份至 ${backup}`)
    // 宿主机文件已被覆盖：草稿必须重新对齐，否则「有未保存修改」是假的、
    // 再写入会把旧草稿盖回去（那就是一次无备份的破坏性写入）。
    const f = await fetchComposeFile(cp.selectedId, p.name)
    configFile.value = f.configFile
    draft.value = f.content
    original.value = f.content
    diffText.value = ''
    previewing.value = false
    bkDiff.value = ''
    bkPreviewName.value = ''
    await loadBackups()
    await cp.reload()
  } catch (e) {
    ui.notifyError(e)
  } finally {
    bkBusy.value = false
  }
}

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
  resetBackups()
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
  resetBackups()
}

function onConfirmClose(): void {
  closeAsk.value = false
  editing.value = null
  resetBackups()
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

/* ---- 写前备份分区（S7.11）：附在编辑面板内，不另开页 ---- */
.cp__bk {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding-top: var(--space-3);
  border-top: 1px solid var(--border-subtle);
}

.cp__bk-head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  flex-wrap: wrap;
}

.cp__bk-hint {
  font-size: var(--text-sm);
  color: var(--fg-subtle);
}

.cp__bk-loading,
.cp__bk-empty {
  margin: 0;
  font-size: var(--text-sm);
  color: var(--fg-muted);
}

.cp__bk-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.cp__bk-table th,
.cp__bk-table td {
  padding: var(--space-1) var(--space-2);
  text-align: left;
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-default);
}

.cp__bk-table th {
  color: var(--fg-muted);
  font-weight: 500;
  white-space: nowrap;
}

.cp__bk-table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
}

.cp__bk-name {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.cp__bk-acts {
  display: flex;
  gap: var(--space-2);
  white-space: nowrap;
}

.cp__bk-prev {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.cp__bk-prev-head {
  margin: 0;
  font-size: var(--text-sm);
  color: var(--fg-muted);
}
</style>
