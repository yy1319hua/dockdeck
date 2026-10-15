import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchServers, isOnline, type Server } from '@/api/servers'
import { fetchLatest, type HostMetricsWire } from '@/api/metrics'

/** 一台机的「清单信息 + 实时指标 + 在线判定」汇总。 */
export interface ServerLive {
  server: Server
  /** 以后端 last_seen 为准（轮询器每轮都会更新，成败都记）。 */
  online: boolean
  metrics: HostMetricsWire | null
  /** 拿不到指标的原因；为空表示正常。 */
  error: string | null
}

/** 与默认采集周期（30s）对齐：采完就看到，不必更快。 */
export const REFRESH_MS = 30_000

export const useOverviewStore = defineStore('overview', () => {
  const live = ref<ServerLive[]>([])
  const loading = ref(false)
  const loaded = ref(false)
  const error = ref<string | null>(null)
  let timer: ReturnType<typeof setInterval> | undefined

  const total = computed(() => live.value.length)
  const onlineCount = computed(() => live.value.filter((s) => s.online).length)
  /** 0–1；没有机器时为 0（不是 NaN——除零会让界面显示成 NaN%）。 */
  const onlineRate = computed(() => (total.value === 0 ? 0 : onlineCount.value / total.value))

  // 汇总只看「在线且拿到指标」的机器：离线机的数据是旧的，混进来会把均值拉偏。
  const measurable = computed(() => live.value.filter((s) => s.online && s.metrics))

  const avgCPU = computed(() => {
    const list = measurable.value
    if (list.length === 0) return 0
    const sum = list.reduce((acc, s) => acc + (s.metrics?.cpu.usage_percent ?? 0), 0)
    return sum / list.length
  })

  const memUsed = computed(() =>
    measurable.value.reduce((acc, s) => acc + (s.metrics?.memory.used ?? 0), 0),
  )
  const memTotal = computed(() =>
    measurable.value.reduce((acc, s) => acc + (s.metrics?.memory.total ?? 0), 0),
  )

  async function refresh(): Promise<void> {
    loading.value = true
    try {
      const servers = await fetchServers()
      // 并发拉各机实时指标：一台失败只影响它自己（Promise.allSettled 的语义，
      // 这里显式分支是为了把「后端记录的 last_error」也带上去）。
      live.value = await Promise.all(
        servers.map(async (server): Promise<ServerLive> => {
          let metrics: HostMetricsWire | null = null
          let err: string | null = null
          if (server.enabled) {
            try {
              const r = await fetchLatest(server.id)
              metrics = r.metrics
            } catch (e) {
              err = e instanceof Error ? e.message : String(e)
            }
          }
          return {
            server,
            online: isOnline(server),
            metrics,
            error: err ?? (server.lastError || null),
          }
        }),
      )
      error.value = null
      loaded.value = true
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 页面进入时开始轮询；离开务必 stop，否则定时器会一直挂在后台。 */
  function start(): void {
    void refresh()
    stop()
    timer = setInterval(() => void refresh(), REFRESH_MS)
  }

  function stop(): void {
    if (timer !== undefined) {
      clearInterval(timer)
      timer = undefined
    }
  }

  return {
    live,
    loading,
    loaded,
    error,
    total,
    onlineCount,
    onlineRate,
    avgCPU,
    memUsed,
    memTotal,
    refresh,
    start,
    stop,
  }
})
