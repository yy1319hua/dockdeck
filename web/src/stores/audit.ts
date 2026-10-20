/*
 * 审计日志页数据（S7.7）。与容器/镜像/Compose store 同构点与分叉点：
 *
 * 1. **无轮询**（同 files）：审计是「已经发生过的事」的记录，不是实时状态，
 *    只有手动刷新，没有 timer/start/stop。
 * 2. **无服务器维度**（与 files/containers 不同）：审计是全站数据（Hub 单点写入，
 *    行上带 server_id 但查询是全局的），故本 store 不持 servers/selectedId，
 *    页面也没有服务器切换器（方案 Q3=A）。server_id 只是过滤维度之一。
 * 3. **过滤条件进 store**：draft（表单态，未应用）与 applied（已应用，真正参与
 *    查询的）分开——输入即查询会让前缀输入每敲一个字符都发一次请求；点「查询」
 *    才 apply。切走再回来停在原过滤是合理预期。
 * 4. **展开态不进 store**：「当前展开看哪一行」是易失 UI 态，按 S7.5/S7.6 纪律
 *    留页面本地 ref。
 */
import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchAuditLogs, type AuditFilter, type AuditLog } from '@/api/audit'

export const useAuditStore = defineStore('audit', () => {
  const logs = ref<AuditLog[]>([])
  /** 0 = 没有更多；否则是下一页的 before_id 游标。 */
  const nextBefore = ref(0)
  const loading = ref(false)
  const loadingMore = ref(false)
  const error = ref<string | null>(null)

  /* ---------- 过滤：draft 表单态 / applied 已应用 ---------- */

  /** from/to 存 datetime-local 的 value 形态（YYYY-MM-DDTHH:mm，本地时区无时区缀）。 */
  const draftFrom = ref('')
  const draftTo = ref('')
  const draftServerId = ref('')
  /** 动作前缀，如 container。 */
  const draftAction = ref('')
  /** '' = 不限；ok | error。 */
  const draftResult = ref('')

  const applied = ref<AuditFilter>({})

  /** 是否有非缺省过滤生效（供空态文案区分「没有数据」与「没有匹配」）。 */
  const filterActive = computed(() => Object.keys(applied.value).length > 0)

  /** draft → applied：时间串转 Date 再归一成秒级 RFC3339（api 层 isoSeconds 口径）。 */
  function applyFilter(): void {
    const f: AuditFilter = {}
    if (draftFrom.value) {
      const d = new Date(draftFrom.value)
      if (!Number.isNaN(d.getTime())) f.from = d.toISOString().replace(/\.\d+Z$/, 'Z')
    }
    if (draftTo.value) {
      const d = new Date(draftTo.value)
      if (!Number.isNaN(d.getTime())) f.to = d.toISOString().replace(/\.\d+Z$/, 'Z')
    }
    const sid = Number(draftServerId.value)
    if (Number.isFinite(sid) && sid > 0) f.serverId = Math.floor(sid)
    if (draftAction.value.trim()) f.action = draftAction.value.trim()
    if (draftResult.value) f.result = draftResult.value
    applied.value = f
    void load()
  }

  /** 清空 draft 与 applied，回到「缺省窗口、不限」的初始查询。 */
  function resetFilter(): void {
    draftFrom.value = ''
    draftTo.value = ''
    draftServerId.value = ''
    draftAction.value = ''
    draftResult.value = ''
    applied.value = {}
    void load()
  }

  /* ---------- 查询 ---------- */

  /** 首载/应用过滤：重置列表拉第一页。 */
  async function load(): Promise<void> {
    loading.value = true
    error.value = null
    try {
      const page = await fetchAuditLogs(applied.value)
      logs.value = page.logs
      nextBefore.value = page.nextBefore
    } catch (e) {
      // 失败保留上次数据只记原因：被一次网络抖动清成空白比显示旧列表更糟。
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 加载更多：游标翻页，追加到表尾。nextBefore=0 时页面不应发射（按钮隐藏）。 */
  async function loadMore(): Promise<void> {
    if (!nextBefore.value || loadingMore.value) {
      return
    }
    loadingMore.value = true
    error.value = null
    try {
      const page = await fetchAuditLogs(applied.value, nextBefore.value)
      logs.value = logs.value.concat(page.logs)
      nextBefore.value = page.nextBefore
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loadingMore.value = false
    }
  }

  return {
    logs,
    nextBefore,
    loading,
    loadingMore,
    error,
    draftFrom,
    draftTo,
    draftServerId,
    draftAction,
    draftResult,
    applied,
    filterActive,
    applyFilter,
    resetFilter,
    load,
    loadMore,
  }
})
