import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchServers, isOnline, type Server } from '@/api/servers'
import { fetchContainers, type Container } from '@/api/containers'
import { fetchLatest, type ContainerStatWire } from '@/api/metrics'
import { REFRESH_MS } from './overview'

/**
 * 一行容器 = 清单信息（身份/状态，来自 Agent 实时转发）+ 实时占用
 * （来自 Hub 指标轮询快照，可能缺席——Agent 没跑 Docker 时整个为 null）。
 * 两个数据源分开取：清单失败（Agent 不可达）是错误；占用缺席是降级，不算失败。
 */
export interface ContainerRow {
  container: Container
  stat: ContainerStatWire | null
}

export const useContainersStore = defineStore('containers', () => {
  const servers = ref<Server[]>([])
  const selectedId = ref(0)
  /** 显示已停止容器（与 docker ps 的 -a 同义，默认不显示）。 */
  const showAll = ref(false)
  const rows = ref<ContainerRow[]>([])
  const loading = ref(false)
  const refreshing = ref(false)
  const error = ref<string | null>(null)
  let timer: ReturnType<typeof setInterval> | undefined

  const selected = computed(() => servers.value.find((s) => s.id === selectedId.value) ?? null)

  /** 默认选中：第一台在线机 → 第一台启用机 → 第一台。用户选过就被 autoSelect 放过。 */
  function autoSelect(): void {
    if (selectedId.value && servers.value.some((s) => s.id === selectedId.value)) {
      return
    }
    const online = servers.value.find((s) => s.enabled && isOnline(s))
    const enabled = servers.value.find((s) => s.enabled)
    selectedId.value = (online ?? enabled ?? servers.value[0])?.id ?? 0
  }

  async function pull(): Promise<void> {
    const list = await fetchServers()
    servers.value = list
    autoSelect()
    const id = selectedId.value
    if (!id) {
      rows.value = []
      return
    }
    const containers = await fetchContainers(id, showAll.value)
    // 占用列是加分项：拿不到（无指标/无 Docker）就只显示清单，静默降级。
    let stats: ContainerStatWire[] = []
    try {
      const latest = await fetchLatest(id)
      stats = latest.containers ?? []
    } catch {
      stats = []
    }
    // 按 ID 合并；个别引擎 stats 的 ID 与清单不完全一致时退到按名字匹配。
    const byId = new Map(stats.map((s) => [s.id, s]))
    const byName = new Map(stats.map((s) => [s.name, s]))
    rows.value = containers.map((c) => ({
      container: c,
      stat: byId.get(c.id) ?? byName.get(c.name) ?? null,
    }))
  }

  async function load(): Promise<void> {
    loading.value = true
    try {
      await pull()
      error.value = null
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 后台轮询与详情页同款：不动 loading，失败保留上次数据只记原因。 */
  async function poll(): Promise<void> {
    refreshing.value = true
    try {
      await pull()
      error.value = null
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      refreshing.value = false
    }
  }

  async function selectServer(id: number): Promise<void> {
    if (selectedId.value === id) {
      return
    }
    selectedId.value = id
    await load()
  }

  async function toggleShowAll(): Promise<void> {
    showAll.value = !showAll.value
    await load()
  }

  function start(): void {
    void load()
    stop()
    timer = setInterval(() => void poll(), REFRESH_MS)
  }

  function stop(): void {
    if (timer !== undefined) {
      clearInterval(timer)
      timer = undefined
    }
  }

  return {
    servers,
    selectedId,
    selected,
    showAll,
    rows,
    loading,
    refreshing,
    error,
    load,
    poll,
    selectServer,
    toggleShowAll,
    start,
    stop,
  }
})
