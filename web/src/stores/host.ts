import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchServers, isOnline, type Server } from '@/api/servers'
import { REFRESH_MS } from './overview'
import {
  fetchLatest,
  fetchOverview,
  rangeWindow,
  type ContainerStatWire,
  type HostMetricsWire,
  type HostPointWire,
  type RangeKey,
} from '@/api/metrics'

/** 容器表最多展示的行数：整屏几十个容器时，列表比图表更难读，取占用最高的若干。 */
export const CONTAINER_TOP_N = 10

export const useHostStore = defineStore('host', () => {
  const server = ref<Server | null>(null)
  const metrics = ref<HostMetricsWire | null>(null)
  const containers = ref<ContainerStatWire[]>([])
  const points = ref<HostPointWire[]>([])
  const range = ref<RangeKey>('1h')
  const loading = ref(false)
  /** 后台轮询中（与 loading 分开：轮询不该让区间按钮反复失效）。 */
  const refreshing = ref(false)
  const error = ref<string | null>(null)
  let timer: ReturnType<typeof setInterval> | undefined

  const online = computed(() => (server.value ? isOnline(server.value) : false))
  /** 按 CPU 占用降序：运维看详情页，第一个问题总是「谁在吃资源」。 */
  const containersSorted = computed(() =>
    [...containers.value].sort((a, b) => b.cpu_percent - a.cpu_percent),
  )
  const containersTop = computed(() => containersSorted.value.slice(0, CONTAINER_TOP_N))
  const containersHidden = computed(() =>
    Math.max(0, containersSorted.value.length - containersTop.value.length),
  )

  async function loadRange(id: number): Promise<void> {
    const { from, to } = rangeWindow(range.value)
    const r = await fetchOverview(id, from, to)
    points.value = r.points ?? []
  }

  /** 实时快照 + 当前区间曲线一把拉完（轮询与首载共用，口径不劈叉）。 */
  async function pull(id: number): Promise<void> {
    // 清单信息（名称/在线）与实时指标并行拉，互不依赖。
    const [list, latest] = await Promise.all([fetchServers(), fetchLatest(id)])
    server.value = list.find((s) => s.id === id) ?? null
    metrics.value = latest.metrics
    containers.value = latest.containers ?? []
    await loadRange(id)
  }

  async function load(id: number): Promise<void> {
    loading.value = true
    try {
      await pull(id)
      error.value = null
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /**
   * 后台轮询：与首载同一条 pull，但不动 loading（区间按钮不跟着反复失效），
   * 失败时保留上次成功的数据、只把原因记进 error——监控页被一次网络抖动
   * 清空成空白比显示旧数据更糟。
   */
  async function poll(id: number): Promise<void> {
    refreshing.value = true
    try {
      await pull(id)
      error.value = null
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      refreshing.value = false
    }
  }

  /** 切区间只重拉曲线，不动实时快照——快照刚拿过，没必要再请求一次。 */
  async function setRange(id: number, key: RangeKey): Promise<void> {
    if (range.value === key) {
      return
    }
    range.value = key
    loading.value = true
    try {
      await loadRange(id)
      error.value = null
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 进页面起表、离开停表；节奏 REFRESH_MS 与总览页、后端采集周期（30s）对齐。 */
  function start(id: number): void {
    void load(id)
    stop()
    timer = setInterval(() => void poll(id), REFRESH_MS)
  }

  function stop(): void {
    if (timer !== undefined) {
      clearInterval(timer)
      timer = undefined
    }
  }

  function reset() {
    server.value = null
    metrics.value = null
    containers.value = []
    points.value = []
    error.value = null
  }

  return {
    server,
    metrics,
    containers,
    containersSorted,
    containersTop,
    containersHidden,
    points,
    range,
    loading,
    refreshing,
    error,
    online,
    load,
    setRange,
    start,
    stop,
    reset,
  }
})
