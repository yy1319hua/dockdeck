import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchServers, isOnline, type Server } from '@/api/servers'
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
  const error = ref<string | null>(null)

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

  async function load(id: number): Promise<void> {
    loading.value = true
    try {
      // 清单信息（名称/在线）与实时指标并行拉，互不依赖。
      const [list, latest] = await Promise.all([fetchServers(), fetchLatest(id)])
      server.value = list.find((s) => s.id === id) ?? null
      metrics.value = latest.metrics
      containers.value = latest.containers ?? []
      await loadRange(id)
      error.value = null
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
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
    error,
    online,
    load,
    setRange,
    reset,
  }
})
