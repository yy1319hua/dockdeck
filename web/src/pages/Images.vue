<!--
  S7.4 镜像列表：全局页 + 顶部主机切换器（与容器页同构，用户拍板）。

  四个操作的确认范围（用户拍板 2026-10-16）：
   * pull  —— 输入 ref 直接执行，toast 回报；业务失败看流内 Err（HTTP 200 也是失败，
              api 层已统一成 throw）；
   * delete —— 简单确认（展示将删的 repo:tag / 短 ID）；被容器占用时 Engine 回 409，
              原样透传呈现；
   * prune —— 两段式（后端 dry-run 设计）：先 GET dangling=true 把将删清单拉下来在
              确认框里展示（N 个、合计多少），用户确认后才 POST prune 真删，
              结果按 Deleted/Untagged 汇总回收空间。

  显示口径与 docker images 习惯对齐：默认不含中间层、不含已停止之外的过滤项；
  「只看悬空」是 prune 的预演，「显示中间层」是 -a。
-->
<template>
  <div class="im">
    <header class="im__head">
      <div>
        <h1 class="im__title">镜像</h1>
        <p class="im__sub">
          {{ im.selected ? im.selected.name : '未选择主机' }} · {{ im.images.length }} 个镜像<template v-if="im.refreshing"> · 刷新中…</template>
        </p>
      </div>
      <button type="button" class="btn" :disabled="im.loading" @click="im.load()">刷新</button>
    </header>

    <div class="im__bar">
      <label class="im__host">
        主机
        <select
          class="field__input im__select"
          :value="im.selectedId"
          :disabled="im.servers.length === 0"
          @change="onSelectHost"
        >
          <option v-for="s in im.servers" :key="s.id" :value="s.id">
            {{ s.name }}{{ isOnline(s) ? '' : s.enabled ? '（离线）' : '（已禁用）' }}
          </option>
        </select>
      </label>
      <label class="im__check">
        <input type="checkbox" :checked="im.danglingOnly" @change="im.toggleDangling()" />
        只看悬空
      </label>
      <label class="im__check">
        <input type="checkbox" :checked="im.showIntermediate" @change="im.toggleIntermediate()" />
        显示中间层
      </label>
      <button
        type="button"
        class="btn im__prune-btn"
        :disabled="danglingCount === 0 || pruneLoading || acting"
        :title="danglingCount === 0 ? '当前没有悬空镜像' : `清理 ${danglingCount} 个悬空镜像`"
        @click="askPrune"
      >
        {{ pruneLoading ? '加载中…' : `清理悬空镜像${danglingCount ? `（${danglingCount}）` : ''}` }}
      </button>
    </div>

    <form class="im__pull" @submit.prevent="onPull">
      <input
        v-model="pullRef"
        class="field__input"
        type="text"
        autocomplete="off"
        spellcheck="false"
        placeholder="镜像引用，如 nginx:latest 或 registry.example.com:5000/app:v1"
        :disabled="pulling || !im.selectedId"
      />
      <button type="submit" class="btn btn--primary" :disabled="!canPull">拉取</button>
      <span v-if="pulling" class="im__pulling">拉取中，大镜像可能需要数十秒…</span>
      <span v-else-if="pullRef.trim() && !refOk" class="im__hint">引用含不支持的字符</span>
    </form>

    <p v-if="im.error" class="im__err">{{ im.error }}</p>

    <table v-else-if="im.images.length > 0" class="im__table">
      <thead>
        <tr>
          <th>镜像</th>
          <th>ID</th>
          <th class="num">大小</th>
          <th>架构</th>
          <th>创建时间</th>
          <th class="im__acts-col">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="img in im.images" :key="img.id">
          <td class="im__tags" :title="img.digests.join('\n')">
            <span v-if="img.dangling" class="im__none">&lt;none&gt;</span>
            <template v-else>
              <span v-for="t in img.tags" :key="t" class="im__tag">{{ t }}</span>
            </template>
          </td>
          <td class="im__id" :title="img.id">{{ shortId(img.id) }}</td>
          <td class="num">{{ formatBytes(img.size) }}</td>
          <td class="im__arch">{{ img.arch }}/{{ img.os }}</td>
          <td class="im__when">{{ formatWhen(img.created) }}</td>
          <td class="im__acts">
            <button type="button" class="link link--danger" :disabled="acting" @click="askDelete(img)">删除</button>
          </td>
        </tr>
      </tbody>
    </table>

    <!-- 手机态（<768px，PLAN-MOBILE M5）：表格退役转卡片；动作只有删除，直接行内放，不必收面板。 -->
    <div v-else-if="im.images.length > 0" class="im__cards">
      <article v-for="img in im.images" :key="img.id" class="icard">
        <header class="icard__head">
          <span v-if="img.dangling" class="im__none">&lt;none&gt;</span>
          <span v-else class="icard__tag" :title="img.tags.join(' | ')">{{ img.tags[0] || '&lt;none&gt;' }}</span>
          <span class="icard__size">{{ formatBytes(img.size) }}</span>
        </header>
        <p v-if="img.tags.length > 1" class="icard__more-tags" :title="img.tags.join(' | ')">另有 {{ img.tags.length - 1 }} 个 tag</p>
        <p class="icard__meta">
          <span>{{ shortId(img.id) }}</span>
          <span>{{ img.arch }}/{{ img.os }}</span>
          <span>{{ formatWhen(img.created) }}</span>
        </p>
        <footer class="icard__acts">
          <button type="button" class="link link--danger" :disabled="acting" @click="askDelete(img)">删除</button>
        </footer>
      </article>
    </div>

    <div v-else-if="!im.loading && !im.error" class="im__empty">
      <template v-if="im.servers.length === 0">
        <p class="im__empty-title">还没有纳管的主机</p>
        <p class="im__empty-hint">纳管方式见 S5.8：Hub 签发 join_secret，Agent 启动时自注册。</p>
      </template>
      <template v-else-if="im.danglingOnly">
        <p class="im__empty-title">没有悬空镜像</p>
        <p class="im__empty-hint">悬空镜像是无 tag 的孤儿层（重新构建产生的中间层），可安全清理。</p>
      </template>
      <template v-else>
        <p class="im__empty-title">没有镜像</p>
        <p class="im__empty-hint">用上方的输入框拉取第一个镜像，或勾选「显示中间层」查看全部。</p>
      </template>
    </div>

    <!-- 删除：简单确认（用户拍板——镜像 ref 是短形式，不需输名） -->
    <AppConfirm
      :open="deleting !== null"
      :title="`删除镜像 ${deleting ? refOf(deleting) : ''}`"
      message="删除后不可恢复。若该镜像正被容器使用，引擎会拒绝并返回冲突。镜像可随时重新拉取，故不需输入名称确认。"
      danger
      confirm-label="删除"
      @confirm="onConfirmDelete"
      @cancel="deleting = null"
    />

    <!-- 清理悬空镜像：两段式的第二段——先展示将删清单，确认后才真删 -->
    <AppConfirm
      :open="pruneList !== null"
      title="清理悬空镜像"
      :message="pruneMessage"
      danger
      confirm-label="清理"
      @confirm="onConfirmPrune"
      @cancel="pruneList = null"
    >
      <ul class="im__prune-list">
        <li v-for="it in pruneList ?? []" :key="it.id" class="im__prune-item">
          <span class="im__prune-tag">{{ it.tags[0] ?? shortId(it.id) }}</span>
          <span class="im__prune-size">{{ formatBytes(it.size) }}</span>
        </li>
      </ul>
    </AppConfirm>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import AppConfirm from '@/components/AppConfirm.vue'
import { deleteImage, fetchImages, looksLikeImageRef, pruneImages, pullImage, type Image } from '@/api/images'
import { isOnline } from '@/api/servers'
import { formatBytes, formatWhen } from '@/utils/format'
import { useImagesStore } from '@/stores/images'
import { useUiStore } from '@/stores/ui'

const im = useImagesStore()
const ui = useUiStore()

onMounted(() => im.start())
onUnmounted(() => im.stop())

/** 任一写操作在飞时禁用全表操作按钮，防连点。 */
const acting = ref(false)

/* ---------- 拉取 ---------- */

const pullRef = ref('')
const pulling = ref(false)

const refOk = computed(() => looksLikeImageRef(pullRef.value.trim()))
const canPull = computed(() => refOk.value && !pulling.value && im.selectedId > 0)

async function onPull(): Promise<void> {
  const ref = pullRef.value.trim()
  if (!refOk.value || pulling.value) {
    return
  }
  pulling.value = true
  try {
    const r = await pullImage(im.selectedId, ref)
    ui.notifyNotice(`已拉取 ${ref}${r.layers.length ? `（${r.layers.length} 层）` : ''}`)
    pullRef.value = ''
    await im.load()
  } catch (e) {
    ui.notifyError(e)
  } finally {
    pulling.value = false
  }
}

/* ---------- 删除 ---------- */

const deleting = ref<Image | null>(null)

/** 删除引用：有 tag 用第一个 repo:tag（ Engine 接受名字作引用），悬空镜像用完整 ID。 */
function refOf(img: Image): string {
  return img.tags[0] ?? img.id
}

function askDelete(img: Image): void {
  deleting.value = img
}

async function onConfirmDelete(): Promise<void> {
  const img = deleting.value
  if (!img) {
    return
  }
  deleting.value = null
  acting.value = true
  try {
    await deleteImage(im.selectedId, refOf(img))
    ui.notifyNotice(`已删除镜像 ${refOf(img)}`)
    await im.load()
  } catch (e) {
    ui.notifyError(e)
  } finally {
    acting.value = false
  }
}

/* ---------- 清理悬空镜像（两段式） ---------- */

const pruneLoading = ref(false)
const pruneList = ref<Image[] | null>(null)

const danglingCount = computed(() => im.images.filter((i) => i.dangling).length)

const pruneMessage = computed(() => {
  const list = pruneList.value ?? []
  const total = list.reduce((sum, i) => sum + i.size, 0)
  return `将清理 ${list.length} 个悬空镜像，合计 ${formatBytes(total)}。删除后不可恢复。`
})

async function askPrune(): Promise<void> {
  if (pruneLoading.value || danglingCount.value === 0) {
    return
  }
  pruneLoading.value = true
  try {
    // 第一段（dry-run 的「展示」）：只拉清单不删。 freshly 拉取而非用表内数据——
    // 表可能被「只看悬空」之外的过滤口径影响，且清单要与将删内容严格一致。
    const list = await fetchImages(im.selectedId, { dangling: true })
    if (list.length === 0) {
      ui.notifyNotice('没有悬空镜像了')
      await im.load()
      return
    }
    pruneList.value = list
  } catch (e) {
    ui.notifyError(e)
  } finally {
    pruneLoading.value = false
  }
}

async function onConfirmPrune(): Promise<void> {
  if (acting.value) {
    return
  }
  pruneList.value = null
  acting.value = true
  try {
    const r = await pruneImages(im.selectedId)
    const deleted = r.items.filter((i) => i.deleted).length
    const untagged = r.items.filter((i) => i.untagged).length
    const parts: string[] = []
    if (deleted) parts.push(`删除 ${deleted} 个镜像`)
    if (untagged) parts.push(`解绑 ${untagged} 个 tag`)
    ui.notifyNotice(`清理完成：${parts.join('、') || '无变更'}，回收 ${formatBytes(r.spaceReclaimed)}`)
    await im.load()
  } catch (e) {
    ui.notifyError(e)
  } finally {
    acting.value = false
  }
}

/* ---------- 杂项 ---------- */

function shortId(id: string): string {
  return id.replace(/^sha256:/, '').slice(0, 12)
}

function onSelectHost(e: Event): void {
  const v = Number((e.target as HTMLSelectElement).value)
  if (Number.isFinite(v) && v > 0) {
    void im.selectServer(v)
  }
}
</script>

<style scoped>
.im {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.im__head {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.im__title {
  font-size: var(--text-xl);
}

.im__sub {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.im__head .btn {
  margin-left: auto;
  flex: none;
}

.im__bar {
  display: flex;
  align-items: center;
  gap: var(--space-4);
  flex-wrap: wrap;
}

.im__host,
.im__check {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.im__select {
  height: 32px;
  width: auto;
  min-width: 180px;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

.im__prune-btn {
  margin-left: auto;
  flex: none;
}

.im__pull {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  flex-wrap: wrap;
}

.im__pull .field__input {
  flex: 1;
  min-width: 260px;
}

.im__pulling,
.im__hint {
  font-size: var(--text-sm);
  color: var(--fg-muted);
}

.im__err {
  padding: var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

.im__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.im__table th {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-subtle);
  font-weight: var(--weight-normal);
  text-align: left;
}

.im__table td {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
  vertical-align: middle;
}

.im__table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.im__tags {
  max-width: 300px;
}

.im__tag {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-weight: var(--weight-medium);
}

.im__none {
  color: var(--fg-subtle);
  font-style: italic;
}

.im__id,
.im__arch,
.im__when {
  color: var(--fg-muted);
  white-space: nowrap;
}

.im__acts {
  white-space: nowrap;
}

.im__prune-list {
  max-height: 240px;
  overflow-y: auto;
  margin: 0;
  padding: 0;
  list-style: none;
}

.im__prune-item {
  display: flex;
  justify-content: space-between;
  gap: var(--space-3);
  padding: var(--space-1) 0;
  border-bottom: 1px solid var(--border-subtle);
  font-size: var(--text-sm);
}

.im__prune-tag {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.im__prune-size {
  flex: none;
  color: var(--fg-muted);
  font-variant-numeric: tabular-nums;
}

.im__empty {
  padding: var(--space-6);
  background: var(--bg-elev-1);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-md);
  text-align: center;
}

.im__empty-title {
  font-weight: var(--weight-medium);
}

.im__empty-hint {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

/* 手机态（<768px，M5）：桌面只见表格，手机只见卡片。 */
.im__cards {
  display: none;
}

@media (max-width: 767px) {
  .im__table {
    display: none;
  }

  .im__cards {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
}

.icard {
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  padding: var(--space-3);
}

.icard__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  min-width: 0;
}

.icard__tag {
  font-weight: var(--weight-medium);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.icard__size {
  flex: none;
  color: var(--fg-muted);
  font-size: var(--text-sm);
  font-variant-numeric: tabular-nums;
}

.icard__more-tags {
  margin-top: var(--space-1);
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.icard__meta {
  margin-top: var(--space-2);
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  color: var(--fg-muted);
  font-size: var(--text-xs);
  font-variant-numeric: tabular-nums;
}

.icard__acts {
  margin-top: var(--space-2);
  display: flex;
  justify-content: flex-end;
}
</style>
