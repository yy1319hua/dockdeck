/*
 * 设置页数据（S7.8）。与 audit/files store 的同构点与分叉点：
 *
 * 1. **无轮询**：设置是「配置」，不是实时状态——改一次存一次，只有手动
 *    刷新（同 S7.6/S7.7 纪律）。
 * 2. **写动作在页面、不在 store**（同 files/containers 范式）：store 只持
 *    服务端态与保护名单的编辑草稿；保存/签发/撤销由页面调用 api 并配
 *    toast，成功后回 store 刷新。唯一例外是保护名单草稿——它与 store 的
 *    patterns/usingDefault 强耦合（dirty 判定、保存后回显），apply 入口
 *    放 store。
 * 3. **draft/applied 两态**（同 audit 过滤）：textarea 文本是 draft，
 *    服务端确认过的 patterns 是 applied；保存成功即 applied 追上 draft，
 *    dirty 自然清零。切走再回来停原编辑是合理预期。
 * 4. **改密不进 store**：纯页面本地表单 + 直接调 api/auth——它不产生
 *    需要跨页面保留的服务端态（成功后全局跳转登录页，见 Settings.vue）。
 */
import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import {
  fetchProtectedContainers,
  fetchTlsFingerprint,
  type ProtectedView,
} from '@/api/settings'
import { fetchScopes, fetchTokens, type ApiToken, type ScopeItem } from '@/api/tokens'

export const useSettingsStore = defineStore('settings', () => {
  /* ---------- 保护名单（applied = 服务端确认态，draft = textarea 文本态） ---------- */

  const patterns = ref<string[]>([])
  /** true = 名单为空，容器守卫回落默认名单（^dockdeck-）——UI 须明示。 */
  const usingDefault = ref(false)
  const draftText = ref('')

  /** draft 的归一形态：一行一条、去首尾空白、丢空行（与后端 PUT 同口径）。 */
  const draftPatterns = computed(() =>
    draftText.value
      .split('\n')
      .map((s) => s.trim())
      .filter(Boolean),
  )

  /** 有未保存编辑（页面据此启用/禁用保存与重置按钮）。 */
  const protectedDirty = computed(
    () => JSON.stringify(draftPatterns.value) !== JSON.stringify(patterns.value),
  )

  /**
   * 用服务端视图更新 applied。仅当用户没有未保存编辑时同步 draft——
   * 手动刷新不该静默丢掉的编辑；有编辑时 draft 原样保留，dirty 按新
   * applied 重算。
   */
  function applyProtectedView(v: ProtectedView): void {
    patterns.value = v.patterns
    usingDefault.value = v.usingDefault
    if (!protectedDirty.value) {
      draftText.value = v.patterns.join('\n')
    }
  }

  /** 丢弃未保存编辑，draft 回当前 applied。 */
  function resetDraft(): void {
    draftText.value = patterns.value.join('\n')
  }

  /* ---------- TLS 指纹 ---------- */

  const fingerprint = ref('')
  /** true = 内置自签证书（有指纹可比对）；false = none 或外部终止 TLS。 */
  const selfSigned = ref(false)

  /* ---------- API Token ---------- */

  const tokens = ref<ApiToken[]>([])
  /** scope 字典（后端单点维护的中文说明）。静态数据，load 过一次即缓存。 */
  const scopes = ref<ScopeItem[]>([])

  /* ---------- 读取 ---------- */

  const loading = ref(false)
  const refreshing = ref(false)
  const error = ref<string | null>(null)

  /**
   * 首载 / 手动刷新：四块数据一把拉（Promise.all 失败即整体失败——
   * 任一区块读不到时给错误条与重试，不渲染半屏静默空白）。
   */
  async function load(): Promise<void> {
    loading.value = true
    error.value = null
    try {
      const [pv, tls, tks] = await Promise.all([
        fetchProtectedContainers(),
        fetchTlsFingerprint(),
        fetchTokens(),
      ])
      applyProtectedView(pv)
      fingerprint.value = tls.fingerprint
      selfSigned.value = tls.selfSigned
      tokens.value = tks
      if (scopes.value.length === 0) {
        scopes.value = await fetchScopes()
      }
    } catch (e) {
      // 失败保留上次数据只记原因：被一次网络抖动清成空白比显示旧数据更糟。
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 手动刷新（页头按钮）：与 load 同流程，另置 refreshing 供按钮态。 */
  async function refresh(): Promise<void> {
    refreshing.value = true
    try {
      await load()
    } finally {
      refreshing.value = false
    }
  }

  /** Token 列表刷新（签发/撤销后调用；scope 字典已缓存不重拉）。 */
  async function refreshTokens(): Promise<void> {
    try {
      tokens.value = await fetchTokens()
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
      throw e // 页面还要 toast，不能在这咽掉
    }
  }

  return {
    // 保护名单
    patterns,
    usingDefault,
    draftText,
    draftPatterns,
    protectedDirty,
    applyProtectedView,
    resetDraft,
    // TLS 指纹
    fingerprint,
    selfSigned,
    // Token
    tokens,
    scopes,
    // 读取态
    loading,
    refreshing,
    error,
    load,
    refresh,
    refreshTokens,
  }
})
