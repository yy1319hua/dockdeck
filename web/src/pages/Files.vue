<!--
  S7.6 文件管理：全局页 + 顶部服务器切换器（与容器/镜像/Compose 页同构）。

  四项用户拍板（2026-10-16）的落点：
   * Q1=A 面包屑 + 单层列表——每进一层一次 depth=1 请求，截断按层清晰；
   * Q2=A 根槽位探测——后端无「列根」端点，静态槽位 0..3 试列，200 亮/400 灰；
   * Q3=A 确认范围——delete 输名确认（无备份不可恢复，对齐删容器房规）、
           rename 输入新名（无二次确认）、mkdir 输入名字直行；
   * Q4=A 同页内嵌编辑面板——文本文件「预览 → 编辑 → 保存（PUT）」，二进制/
           超限只给下载；保存不设 diff 预览段（文件管理后端无 diff 端点，
           备份由 Agent 写前自动完成，备份路径在 toast 展示）。

  与 Compose 页同款的两段式差异：那边 diff 预览强制先行（后端有 diff 端点），
  这边保存只有 AppConfirm 一段（带备份说明）——形态差异由后端能力决定。
-->
<template>
  <div class="fl">
    <header class="fl__head">
      <div>
        <h1 class="fl__title">文件</h1>
        <p class="fl__sub">
          {{ fs.selected ? fs.selected.name : '未选择服务器' }} · {{ pathLabel }} ·
          {{ fs.entries.length }} 项<template v-if="fs.refreshing"> · 刷新中…</template>
        </p>
      </div>
      <button type="button" class="btn" :disabled="fs.loading" @click="fs.refresh()">刷新</button>
    </header>

    <div class="fl__bar">
      <label class="fl__host">
        服务器
        <select
          class="field__input fl__select"
          :value="fs.selectedId"
          :disabled="fs.servers.length === 0 || editingActive"
          :title="editingActive ? '编辑进行中，先关闭编辑面板再切服务器' : ''"
          @change="onSelectHost"
        >
          <option v-for="s in fs.servers" :key="s.id" :value="s.id">
            {{ s.name }}{{ isOnline(s) ? '' : s.enabled ? '（离线）' : '（已禁用）' }}
          </option>
        </select>
      </label>

      <div class="fl__roots">
        <span class="fl__roots-label">根</span>
        <button
          v-for="slot in fs.rootSlots"
          :key="slot.index"
          type="button"
          class="btn fl__root"
          :class="{ 'btn--primary': slot.index === fs.currentRoot }"
          :disabled="slot.status !== 'ok' || editingActive"
          :title="slot.status === 'ok' ? `文件根 ${slot.index}` : '该根不可用（未配置或超出范围）'"
          @click="fs.setRoot(slot.index)"
        >
          {{ slot.index }}
        </button>
        <span v-if="fs.probing" class="fl__probing">探测中…</span>
      </div>

      <button type="button" class="btn" :disabled="!canWrite" @click="askMkdir">新建目录</button>
      <button type="button" class="btn" :disabled="!canWrite" @click="pickUpload">上传</button>
      <!-- 隐藏的文件选择器：点「上传」才唤起，选了即传（写操作有审计与自动备份） -->
      <input ref="uploadInput" class="fl__upload" type="file" @change="onUploadPick" />
    </div>

    <!-- 编辑面板：打开文件后主体切换（同 Compose 页同页内嵌范式） -->
    <div v-if="editingActive" class="fl__edit">
      <div class="fl__edit-head">
        <span class="fl__edit-name">{{ editing ? editing.name : '' }}</span>
        <span class="fl__edit-meta">
          {{ editing ? formatBytes(editing.size) : '' }} · {{ editing ? editing.mode : ''
          }}<template v-if="truncatedFlag"> · 超读取上限，内容已被截断</template>
        </span>
        <div class="fl__edit-btns">
          <template v-if="editable">
            <button v-if="!previewing" type="button" class="btn" @click="previewing = true">预览</button>
            <button v-else type="button" class="btn btn--primary" :disabled="!dirty" @click="previewing = false">编辑</button>
            <button type="button" class="btn btn--primary" :disabled="!dirty || saving" @click="writeAsk = true">保存</button>
          </template>
          <button v-else type="button" class="btn" @click="download(editing!)">下载</button>
          <button type="button" class="btn" @click="closeEditor()">关闭</button>
        </div>
      </div>
      <div v-if="fileLoading" class="fl__loading">加载中…</div>
      <pre v-else-if="!editable" class="fl__binary">该文件不是文本内容（{{ binaryType || '未知类型' }}），不提供在线编辑，请下载后处理。</pre>
      <pre v-else-if="previewing" class="fl__preview">{{ draft }}</pre>
      <textarea v-else v-model="draft" class="field__input fl__editor" spellcheck="false" @keydown.tab.prevent="onTab"></textarea>
    </div>

    <template v-else>
      <nav class="fl__crumbs">
        <button type="button" class="link" :disabled="fs.currentPath === ''" @click="fs.goUp()">.. 上级</button>
        <template v-for="(c, i) in fs.crumbs" :key="c.path">
          <span class="fl__sep">/</span>
          <button v-if="i < fs.crumbs.length - 1" type="button" class="link" @click="fs.enterDir(c.path)">{{ c.label }}</button>
          <span v-else class="fl__crumb-cur">{{ c.label }}</span>
        </template>
        <span v-if="fs.truncated" class="fl__trunc">条目超上限，仅显示前 {{ fs.entries.length }} 条</span>
      </nav>

      <p v-if="fs.error" class="fl__err">{{ fs.error }}</p>

      <table v-else-if="fs.entries.length > 0" class="fl__table">
        <thead>
          <tr>
            <th>名称</th>
            <th class="num">大小</th>
            <th>权限</th>
            <th>修改时间</th>
            <th class="fl__acts-col">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="e in fs.entries" :key="e.path">
            <td class="fl__name">
              <button v-if="e.isDir" type="button" class="link fl__dir" @click="fs.enterDir(e.path)">{{ e.name }}/</button>
              <button v-else type="button" class="link" @click="openFile(e)">{{ e.name }}</button>
            </td>
            <td class="num">{{ e.isDir ? '—' : formatBytes(e.size) }}</td>
            <td class="fl__mode">{{ e.mode }}</td>
            <td class="fl__when">{{ formatRFC3339(e.modTime) }}</td>
            <td class="fl__acts">
              <button v-if="!e.isDir" type="button" class="link" :disabled="acting" @click="openFile(e)">查看/编辑</button>
              <button v-if="!e.isDir" type="button" class="link" :disabled="acting" @click="download(e)">下载</button>
              <button type="button" class="link" :disabled="acting" @click="askRename(e)">重命名</button>
              <button type="button" class="link link--danger" :disabled="acting" @click="askDelete(e)">删除</button>
            </td>
          </tr>
        </tbody>
      </table>

    <!-- 手机态（<768px，PLAN-MOBILE M6）：表格退役转卡片；目录名可点进层。 -->
      <div v-if="!fs.loading && !fs.error && fs.entries.length > 0" class="fl__cards">
        <article v-for="e in fs.entries" :key="e.path" class="fcard">
          <header class="fcard__head">
            <button v-if="e.isDir" type="button" class="link fl__dir fcard__name" @click="fs.enterDir(e.path)">{{ e.name }}/</button>
            <button v-else type="button" class="link fcard__name" @click="openFile(e)">{{ e.name }}</button>
            <span class="fcard__size">{{ e.isDir ? '目录' : formatBytes(e.size) }}</span>
          </header>
          <p class="fcard__meta">{{ e.mode }} · {{ formatRFC3339(e.modTime) }}</p>
          <footer class="fcard__acts">
            <button v-if="!e.isDir" type="button" class="link" :disabled="acting" @click="download(e)">下载</button>
            <button type="button" class="link" :disabled="acting" @click="askRename(e)">重命名</button>
            <button type="button" class="link link--danger" :disabled="acting" @click="askDelete(e)">删除</button>
          </footer>
        </article>
      </div>

      <div v-else-if="!fs.loading && !fs.probing" class="fl__empty">
        <template v-if="fs.servers.length === 0">
          <p class="fl__empty-title">还没有纳管的服务器</p>
          <p class="fl__empty-hint">纳管方式见 S5.8：Hub 签发 join_secret，Agent 启动时自注册。</p>
        </template>
        <template v-else-if="fs.rootSlots.length > 0 && fs.rootSlots.every((s) => s.status !== 'ok')">
          <p class="fl__empty-title">没有可用的文件根</p>
          <p class="fl__empty-hint">该服务器的 Agent 未配置 --fs-root（文件模块未启用），或服务器不在线。</p>
        </template>
        <template v-else>
          <p class="fl__empty-title">空目录</p>
          <p class="fl__empty-hint">用上方「上传」或「新建目录」开始。</p>
        </template>
      </div>
    </template>

    <!-- 删除：输名确认（Q3=A——文件删除无备份、不可恢复，对齐删容器房规） -->
    <AppConfirm
      :open="deleteAsk !== null"
      :title="`删除 ${deleteAsk ? deleteAsk.name : ''}`"
      message="删除后不可恢复——文件管理没有回收站，删除操作也不产生备份。输入名称以确认："
      danger
      :require-name="deleteAsk ? deleteAsk.name : ''"
      confirm-label="删除"
      @confirm="onConfirmDelete"
      @cancel="deleteAsk = null"
    />

    <!-- 保存写入：简单确认（Q4=A——后端无 diff 端点，备份由 Agent 自动完成） -->
    <AppConfirm
      :open="writeAsk"
      :title="`保存 ${editing ? editing.name : ''}`"
      message="将覆盖写入该文件。Agent 侧写前自动备份（.bak.<时间戳>），备份落盘成功才覆盖；备份路径在保存后展示。新文件（原本不存在）不产生备份。"
      confirm-label="保存"
      @confirm="onConfirmWrite"
      @cancel="writeAsk = false"
    />

    <!-- 放弃未保存更改 -->
    <AppConfirm
      :open="closeAsk"
      title="放弃未保存的更改"
      message="当前编辑内容尚未保存，关闭后将丢失。"
      danger
      confirm-label="放弃并关闭"
      @confirm="onConfirmClose"
      @cancel="closeAsk = false"
    />

    <!-- 新建目录 / 重命名：默认插槽放名字输入（Q3=A——输入后即执行，无二次确认） -->
    <AppConfirm
      :open="nameAsk !== null"
      :title="nameAsk && nameAsk.mode === 'mkdir' ? '新建目录' : '重命名'"
      :message="
        nameAsk && nameAsk.mode === 'mkdir'
          ? `在 ${pathLabel} 下创建目录：`
          : `将 ${nameAsk && nameAsk.target ? nameAsk.target.name : ''} 改名为：`
      "
      confirm-label="确定"
      @confirm="onConfirmName"
      @cancel="nameAsk = null"
    >
      <input
        v-model="nameInput"
        class="field__input"
        type="text"
        autocomplete="off"
        spellcheck="false"
        :placeholder="nameAsk && nameAsk.mode === 'mkdir' ? '目录名' : '新名称'"
        @keyup.enter="onConfirmName"
      />
    </AppConfirm>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import AppConfirm from '@/components/AppConfirm.vue'
import { downloadFile, fetchFileContent, fileAction, putFileContent, type FileEntry } from '@/api/files'
import { isOnline } from '@/api/servers'
import { formatBytes, formatRFC3339 } from '@/utils/format'
import { useFilesStore } from '@/stores/files'
import { useUiStore } from '@/stores/ui'

const fs = useFilesStore()
const ui = useUiStore()

onMounted(() => void fs.load())
// 离开页面务必停掉在线状态轮询，否则定时器常驻后台（同其它页面房规）。
onUnmounted(() => fs.stopStatusPoll())

/** 任一写操作在飞时禁用全表操作按钮，防连点。 */
const acting = ref(false)

/** 编辑面板打开时禁用服务器/根切换（防误切丢态，同 Compose 页纪律）。 */
const editingActive = computed(() => editing.value !== null)
const canWrite = computed(() => fs.selectedId > 0 && !editingActive.value && !acting.value)

const pathLabel = computed(() => (fs.currentPath === '' ? '根目录' : fs.currentPath))

function onSelectHost(e: Event): void {
  const v = Number((e.target as HTMLSelectElement).value)
  if (Number.isFinite(v) && v > 0) {
    void fs.selectServer(v)
  }
}

/* ---------- 查看 / 编辑 ---------- */

const editing = ref<FileEntry | null>(null)
const fileLoading = ref(false)
const original = ref('')
const draft = ref('')
const previewing = ref(true)
const saving = ref(false)
const writeAsk = ref(false)
const closeAsk = ref(false)
const truncatedFlag = ref(false)
/** 不可编辑（二进制/超限）时的内容类型，展示用。 */
const binaryType = ref('')
const editable = ref(false)

/** 超过此大小的文本不加载进编辑器（防卡死）；仍可下载。阈值是前端自定，非后端上限。 */
const EDIT_MAX_BYTES = 1 << 20

function isTextLike(ct: string): boolean {
  return (
    ct.startsWith('text/') ||
    ct.includes('json') ||
    ct.includes('yaml') ||
    ct.includes('xml') ||
    ct.includes('javascript')
  )
}

const dirty = computed(() => draft.value !== original.value)

async function openFile(e: FileEntry): Promise<void> {
  if (e.isDir) {
    void fs.enterDir(e.path)
    return
  }
  editing.value = e
  fileLoading.value = true
  previewing.value = true
  try {
    const c = await fetchFileContent(fs.selectedId, fs.currentRoot, e.path)
    truncatedFlag.value = c.truncated
    if (isTextLike(c.contentType) && c.blob.size <= EDIT_MAX_BYTES) {
      editable.value = true
      binaryType.value = ''
      original.value = await c.blob.text()
      draft.value = original.value
    } else {
      // 二进制或过大：只读信息卡 + 下载，不强拉进 textarea。
      editable.value = false
      binaryType.value = c.contentType
      original.value = ''
      draft.value = ''
    }
  } catch (err) {
    ui.notifyError(err)
    editing.value = null
  } finally {
    fileLoading.value = false
  }
}

function closeEditor(): void {
  if (dirty.value) {
    closeAsk.value = true
    return
  }
  editing.value = null
}

function onConfirmClose(): void {
  closeAsk.value = false
  editing.value = null
}

/** Tab 插两空格（同 Compose 编辑器口径——YAML/配置文件缩进用）。 */
function onTab(e: KeyboardEvent): void {
  const el = e.target as HTMLTextAreaElement
  const s = el.selectionStart ?? draft.value.length
  const en = el.selectionEnd ?? s
  draft.value = `${draft.value.slice(0, s)}  ${draft.value.slice(en)}`
  requestAnimationFrame(() => {
    el.selectionStart = el.selectionEnd = s + 2
  })
}

async function onConfirmWrite(): Promise<void> {
  const e = editing.value
  if (!e) {
    return
  }
  writeAsk.value = false
  saving.value = true
  try {
    const r = await putFileContent(
      fs.selectedId,
      fs.currentRoot,
      e.path,
      new Blob([draft.value], { type: 'text/plain;charset=utf-8' }),
    )
    ui.notifyNotice(r.backupFile ? `已保存 ${e.name}（备份 ${r.backupFile}）` : `已保存 ${e.name}（新文件，无备份）`)
    original.value = draft.value
    await fs.refresh()
  } catch (err) {
    ui.notifyError(err)
  } finally {
    saving.value = false
  }
}

/* ---------- 下载 / 上传 ---------- */

async function download(e: FileEntry): Promise<void> {
  try {
    await downloadFile(fs.selectedId, fs.currentRoot, e.path)
  } catch (err) {
    ui.notifyError(err)
  }
}

const uploadInput = ref<HTMLInputElement | null>(null)

function pickUpload(): void {
  uploadInput.value?.click()
}

function onUploadPick(e: Event): void {
  const el = e.target as HTMLInputElement
  const f = el.files?.[0]
  el.value = '' // 同一文件再次选择也要能触发 change
  if (f) {
    void doUpload(f)
  }
}

async function doUpload(f: File): Promise<void> {
  acting.value = true
  try {
    const r = await putFileContent(fs.selectedId, fs.currentRoot, joinPath(fs.currentPath, f.name), f)
    ui.notifyNotice(r.backupFile ? `已上传 ${f.name}（备份 ${r.backupFile}）` : `已上传 ${f.name}（新文件）`)
    await fs.refresh()
  } catch (err) {
    ui.notifyError(err)
  } finally {
    acting.value = false
  }
}

/* ---------- 新建目录 / 重命名 / 删除 ---------- */

const nameAsk = ref<{ mode: 'mkdir' | 'rename'; target: FileEntry | null } | null>(null)
const nameInput = ref('')

function askMkdir(): void {
  nameAsk.value = { mode: 'mkdir', target: null }
  nameInput.value = ''
}

function askRename(e: FileEntry): void {
  nameAsk.value = { mode: 'rename', target: e }
  nameInput.value = e.name
}

function joinPath(base: string, name: string): string {
  return base ? `${base}/${name}` : name
}

async function onConfirmName(): Promise<void> {
  const ask = nameAsk.value
  const name = nameInput.value.trim()
  if (!ask || !name) {
    return
  }
  nameAsk.value = null
  acting.value = true
  try {
    if (ask.mode === 'mkdir') {
      await fileAction(fs.selectedId, fs.currentRoot, 'mkdir', joinPath(fs.currentPath, name))
      ui.notifyNotice(`已创建目录 ${name}`)
    } else if (ask.target) {
      await fileAction(fs.selectedId, fs.currentRoot, 'rename', ask.target.path, joinPath(fs.currentPath, name))
      ui.notifyNotice(`已重命名 ${ask.target.name} → ${name}`)
    }
    await fs.refresh()
  } catch (err) {
    ui.notifyError(err)
  } finally {
    acting.value = false
  }
}

const deleteAsk = ref<FileEntry | null>(null)

function askDelete(e: FileEntry): void {
  deleteAsk.value = e
}

async function onConfirmDelete(): Promise<void> {
  const t = deleteAsk.value
  if (!t) {
    return
  }
  deleteAsk.value = null
  acting.value = true
  try {
    await fileAction(fs.selectedId, fs.currentRoot, 'delete', t.path)
    ui.notifyNotice(`已删除 ${t.name}${t.isDir ? '（空目录）' : ''}`)
    await fs.refresh()
  } catch (err) {
    ui.notifyError(err)
  } finally {
    acting.value = false
  }
}
</script>

<style scoped>
.fl {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.fl__head {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.fl__title {
  font-size: var(--text-xl);
}

.fl__sub {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.fl__path {
  font-family: var(--font-mono);
}

.fl__head .btn {
  margin-left: auto;
  flex: none;
}

.fl__bar {
  display: flex;
  align-items: center;
  gap: var(--space-4);
  flex-wrap: wrap;
}

.fl__host,
.fl__roots {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.fl__select {
  height: 32px;
  width: auto;
  min-width: 180px;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

.fl__root {
  min-width: 34px;
  padding: 0 var(--space-2);
  font-variant-numeric: tabular-nums;
}

.fl__probing {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.fl__upload {
  display: none;
}

.fl__crumbs {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  flex-wrap: wrap;
  font-size: var(--text-sm);
}

/* 手机态（<768px，M6）：深层路径单行横滑，不换行不撑破视口。 */
@media (max-width: 767px) {
  .fl__crumbs {
    flex-wrap: nowrap;
    overflow-x: auto;
    white-space: nowrap;
    -webkit-overflow-scrolling: touch;
    padding-bottom: var(--space-1);
  }
}

.fl__sep {
  color: var(--fg-subtle);
}

.fl__crumb-cur {
  font-family: var(--font-mono);
  font-weight: var(--weight-medium);
}

.fl__trunc {
  margin-left: var(--space-3);
  padding: 0 var(--space-2);
  background: var(--warning-subtle);
  border: 1px solid var(--warning-border);
  border-radius: var(--radius-sm);
  color: var(--warning);
  font-size: var(--text-xs);
}

.fl__err {
  padding: var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

.fl__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.fl__table th {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-subtle);
  font-weight: var(--weight-normal);
  text-align: left;
}

.fl__table td {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
  vertical-align: middle;
}

.fl__table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.fl__name {
  max-width: 420px;
  word-break: break-all;
}

.fl__dir {
  font-weight: var(--weight-medium);
}

.fl__mode,
.fl__when {
  color: var(--fg-muted);
  white-space: nowrap;
}

.fl__acts {
  white-space: nowrap;
}

/* ---------- 编辑面板（同页内嵌） ---------- */

.fl__edit {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-4);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
}

.fl__edit-head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  flex-wrap: wrap;
}

.fl__edit-name {
  font-family: var(--font-mono);
  font-weight: var(--weight-medium);
}

.fl__edit-meta {
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.fl__edit-btns {
  margin-left: auto;
  display: flex;
  gap: var(--space-2);
}

.fl__loading {
  padding: var(--space-6);
  text-align: center;
  color: var(--fg-muted);
}

.fl__preview,
.fl__binary {
  max-height: 480px;
  overflow: auto;
  margin: 0;
  padding: var(--space-3);
  background: var(--bg-sunken);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
}

.fl__binary {
  color: var(--fg-muted);
}

.fl__editor {
  min-height: 360px;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: 1.5;
  resize: vertical;
  white-space: pre;
}

.fl__empty {
  padding: var(--space-6);
  background: var(--bg-elev-1);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-md);
  text-align: center;
}

.fl__empty-title {
  font-weight: var(--weight-medium);
}

.fl__empty-hint {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

/* 手机态（<768px，M6）：桌面只见表格，手机只见卡片。 */
.fl__cards {
  display: none;
}

@media (max-width: 767px) {
  .fl__table {
    display: none;
  }

  .fl__cards {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
}

.fcard {
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  padding: var(--space-3);
}

.fcard__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  min-width: 0;
}

.fcard__name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  text-align: left;
}

.fcard__size {
  flex: none;
  color: var(--fg-muted);
  font-size: var(--text-xs);
  font-variant-numeric: tabular-nums;
}

.fcard__meta {
  margin-top: var(--space-1);
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.fcard__acts {
  margin-top: var(--space-2);
  display: flex;
  justify-content: flex-end;
  gap: var(--space-3);
}
</style>
