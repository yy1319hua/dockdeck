<!--
  S7.7 审计日志：全局页（方案 Q3=A）——**无服务器切换器**（审计是 Hub 单点写入的
  全站数据，不是按机维度；server_id 只是过滤维度之一）。

  交互口径（与方案四条对应）：
  * Q1 倒序 + before_id 游标分页：表格最新在前，「加载更多」按游标追加；
  * Q2 四维过滤：from/to（datetime-local，本地时区输入、秒级 RFC3339 发出）、
        server_id、action 前缀、result；点「查询」才应用（输入即查询会让前缀
        每敲一个字符发一次请求）；
  * Q3 本页即全局页本身；
  * Q4 查询不写审计（后端不包 Audit 中间件）：刷新/翻页/导出不留痕，故手动
        刷新按钮可放心给，无需节流。

  无轮询：审计是「已发生的事」的记录，只有手动刷新（同 S7.6 文件页纪律）。
  导出 CSV 的是**当前已加载的行**——游标分页下没翻到的页不在文件里，导出按钮
  旁明示；要全量先翻到底。
-->
<template>
  <div class="au">
    <header class="au__head">
      <div>
        <h1 class="au__title">审计</h1>
        <p class="au__sub">
          共 {{ au.logs.length }} 条<template v-if="au.nextBefore > 0">（还有更多，翻到底或先导出当前）</template
          ><template v-if="au.filterActive"> · 过滤生效</template>
        </p>
      </div>
      <div class="au__head-btns">
        <button type="button" class="btn" :disabled="au.loading" @click="au.load()">刷新</button>
        <button type="button" class="btn" :disabled="au.logs.length === 0" @click="exportCsv">导出 CSV</button>
      </div>
    </header>

    <!-- 过滤条：输入进 draft，点「查询」才应用（store 持 draft/applied 两个态） -->
    <div class="au__bar">
      <label class="au__field">
        从
        <input v-model="au.draftFrom" class="field__input au__dt" type="datetime-local" />
      </label>
      <label class="au__field">
        到
        <input v-model="au.draftTo" class="field__input au__dt" type="datetime-local" />
      </label>
      <label class="au__field">
        服务器 ID
        <input
          v-model="au.draftServerId"
          class="field__input au__sid"
          type="number"
          min="1"
          placeholder="不限"
        />
      </label>
      <label class="au__field">
        动作前缀
        <input v-model="au.draftAction" class="field__input au__act" type="text" placeholder="如 container" />
      </label>
      <label class="au__field">
        结果
        <select v-model="au.draftResult" class="field__input au__res">
          <option value="">不限</option>
          <option value="ok">ok</option>
          <option value="error">error</option>
        </select>
      </label>
      <button type="button" class="btn btn--primary" :disabled="au.loading" @click="au.applyFilter()">查询</button>
      <button type="button" class="btn" :disabled="au.loading" @click="au.resetFilter()">重置</button>
    </div>

    <p v-if="au.error" class="au__err">{{ au.error }}</p>

    <table v-else-if="au.logs.length > 0" class="au__table">
      <thead>
        <tr>
          <th>时间</th>
          <th>用户</th>
          <th class="num">服务器</th>
          <th>动作</th>
          <th>结果</th>
          <th class="au__acts-col">详情</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="l in au.logs" :key="l.id">
          <tr>
            <td class="au__when">{{ formatRFC3339(l.ts) }}</td>
            <td>{{ l.username || '—' }}</td>
            <td class="num">{{ l.serverId || '—' }}</td>
            <td class="au__action">{{ l.action }}</td>
            <td>
              <span class="au__badge" :class="l.result === 'error' ? 'au__badge--err' : 'au__badge--ok'">{{
                l.result || '—'
              }}</span>
            </td>
            <td class="au__acts">
              <button type="button" class="link" @click="toggleExpand(l.id)">
                {{ expandedId === l.id ? '收起' : '展开' }}
              </button>
            </td>
          </tr>
          <!-- 行展开：排障要看的五列（target/detail/error/ip/request_id）都在这里 -->
          <tr v-if="expandedId === l.id" class="au__detail-row">
            <td colspan="6">
              <dl class="au__detail">
                <div><dt>ID</dt><dd>{{ l.id }}</dd></div>
                <div><dt>目标</dt><dd class="au__mono">{{ l.target || '—' }}</dd></div>
                <div><dt>结果</dt><dd>{{ l.result || '—' }}</dd></div>
                <div v-if="l.error" class="au__detail-err"><dt>错误</dt><dd class="au__mono">{{ l.error }}</dd></div>
                <div v-if="l.detail"><dt>明细</dt><dd class="au__mono">{{ l.detail }}</dd></div>
                <div><dt>来源 IP</dt><dd class="au__mono">{{ l.ip || '—' }}</dd></div>
                <div><dt>请求 ID</dt><dd class="au__mono">{{ l.requestId || '—' }}</dd></div>
                <div><dt>操作用户</dt><dd>{{ l.username || '—' }}（uid {{ l.userId }}）</dd></div>
              </dl>
            </td>
          </tr>
        </template>
      </tbody>
    </table>

    <!-- 手机态（<768px，PLAN-MOBILE M7）：表格退役转卡片；展开复用同一 detail 区块。 -->
    <div v-if="!au.loading && !au.error && au.logs.length > 0" class="au__cards">
      <article v-for="l in au.logs" :key="l.id" class="acard">
        <header class="acard__head">
          <span class="acard__action" :title="l.action">{{ l.action }}</span>
          <span class="au__badge" :class="l.result === 'error' ? 'au__badge--err' : 'au__badge--ok'">{{ l.result || '—' }}</span>
        </header>
        <p class="acard__meta">
          <span>{{ formatRFC3339(l.ts) }}</span>
          <span>{{ l.username || '—' }}</span>
          <span>服务器 {{ l.serverId || '—' }}</span>
        </p>
        <footer class="acard__acts">
          <button type="button" class="link" @click="toggleExpand(l.id)">{{ expandedId === l.id ? '收起' : '展开' }}</button>
        </footer>
        <dl v-if="expandedId === l.id" class="au__detail acard__detail">
          <div><dt>ID</dt><dd>{{ l.id }}</dd></div>
          <div><dt>目标</dt><dd class="au__mono">{{ l.target || '—' }}</dd></div>
          <div><dt>结果</dt><dd>{{ l.result || '—' }}</dd></div>
          <div v-if="l.error" class="au__detail-err"><dt>错误</dt><dd class="au__mono">{{ l.error }}</dd></div>
          <div v-if="l.detail"><dt>明细</dt><dd class="au__mono">{{ l.detail }}</dd></div>
          <div><dt>来源 IP</dt><dd class="au__mono">{{ l.ip || '—' }}</dd></div>
        </dl>
      </article>
    </div>

    <div v-else-if="!au.loading" class="au__empty">
      <template v-if="au.filterActive">
        <p class="au__empty-title">没有匹配的审计记录</p>
        <p class="au__empty-hint">放宽过滤条件（如清空动作前缀）或重置后重试。</p>
      </template>
      <template v-else>
        <p class="au__empty-title">暂无审计记录</p>
        <p class="au__empty-hint">后端默认窗口为最近 7 天；操作留痕自 S1 起全量记录，查询本身不写审计。</p>
      </template>
    </div>

    <div v-if="au.nextBefore > 0" class="au__more">
      <button type="button" class="btn" :disabled="au.loadingMore" @click="au.loadMore()">
        {{ au.loadingMore ? '加载中…' : '加载更多' }}
      </button>
      <span class="au__more-hint">导出仅含当前已加载行；翻到底后再导出即全量</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useAuditStore } from '@/stores/audit'
import { formatRFC3339 } from '@/utils/format'
import { auditLogsToCsv } from '@/utils/csv'

const au = useAuditStore()

onMounted(() => void au.load())

/** 当前展开的行 id（易失 UI 态，按 S7.5/S7.6 纪律留页面本地，不进 store）。 */
const expandedId = ref(0)

function toggleExpand(id: number): void {
  expandedId.value = expandedId.value === id ? 0 : id
}

/** 导出当前已加载行：BOM + CRLF + RFC 4180 转义在 utils/csv.ts。 */
function exportCsv(): void {
  const blob = new Blob([auditLogsToCsv(au.logs)], { type: 'text/csv;charset=utf-8' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = `audit-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '')}.csv`
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(a.href)
}
</script>

<style scoped>
.au {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.au__head {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.au__title {
  font-size: var(--text-xl);
}

.au__sub {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.au__head-btns {
  margin-left: auto;
  display: flex;
  gap: var(--space-2);
  flex: none;
}

/* ---------- 过滤条 ---------- */

.au__bar {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  flex-wrap: wrap;
  padding: var(--space-3);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
}

.au__field {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
  white-space: nowrap;
}

.au__dt {
  width: auto;
  min-width: 190px;
  height: 32px;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

.au__sid {
  width: 90px;
  height: 32px;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

.au__act {
  width: 160px;
  height: 32px;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

.au__res {
  width: auto;
  min-width: 88px;
  height: 32px;
  padding: 0 var(--space-2);
  font-size: var(--text-sm);
}

/* ---------- 表格 ---------- */

.au__err {
  padding: var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

.au__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.au__table th {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-subtle);
  font-weight: var(--weight-normal);
  text-align: left;
}

.au__table td {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
  vertical-align: middle;
}

.au__table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.au__when {
  color: var(--fg-muted);
  white-space: nowrap;
}

.au__action {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}

.au__acts {
  white-space: nowrap;
}

.au__badge {
  display: inline-block;
  padding: 0 var(--space-2);
  border-radius: var(--radius-sm);
  font-size: var(--text-xs);
  line-height: 20px;
}

.au__badge--ok {
  background: var(--success-subtle);
  border: 1px solid var(--success-border);
  color: var(--success);
}

.au__badge--err {
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  color: var(--danger);
}

/* ---------- 行展开 ---------- */

.au__detail-row td {
  background: var(--bg-sunken);
}

.au__detail {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: var(--space-2) var(--space-4);
  margin: 0;
  padding: var(--space-2) 0;
  font-size: var(--text-xs);
}

.au__detail > div {
  display: flex;
  gap: var(--space-2);
  min-width: 0;
}

.au__detail dt {
  flex: none;
  color: var(--fg-subtle);
}

.au__detail dd {
  margin: 0;
  min-width: 0;
  word-break: break-all;
}

.au__mono {
  font-family: var(--font-mono);
}

.au__detail-err dd {
  color: var(--danger);
}

/* ---------- 空态 / 加载更多 ---------- */

.au__empty {
  padding: var(--space-6);
  background: var(--bg-elev-1);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-md);
  text-align: center;
}

.au__empty-title {
  font-weight: var(--weight-medium);
}

.au__empty-hint {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.au__more {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.au__more-hint {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

/* 手机态（<768px，M7）：桌面只见表格，手机只见卡片。 */
.au__cards {
  display: none;
}

@media (max-width: 767px) {
  .au__table {
    display: none;
  }

  .au__cards {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
}

.acard {
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  padding: var(--space-3);
}

.acard__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  min-width: 0;
}

.acard__action {
  font-family: var(--font-mono);
  font-weight: var(--weight-medium);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.acard__meta {
  margin-top: var(--space-1);
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  color: var(--fg-muted);
  font-size: var(--text-xs);
  font-variant-numeric: tabular-nums;
}

.acard__acts {
  margin-top: var(--space-2);
  display: flex;
  justify-content: flex-end;
}

.acard__detail {
  margin-top: var(--space-2);
  padding-top: var(--space-2);
  border-top: 1px solid var(--border-subtle);
}
</style>
