import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchServers, isOnline, type Server } from '@/api/servers'
import { fetchVolumes, fetchNetworks, type Volume, type Network } from '@/api/resources'
import { REFRESH_MS } from './overview'

/**
 * 卷与网络页数据（S7.9）。与容器/镜像 store 同构：全局页 + 顶部主机切换器，
 * 30s 轮询与采集周期对齐。
 *
 * 两个数据一把拉（Promise.all）：卷与网络同属一台机的资源，任一失败整块报错
 * 重试——渲染半屏空白（有卷无网）比整块重试更容易让人误读成「这台机没有网络」。
 *
 * 为什么保留 30s 轮询（与文件页的「无轮询」刻意分叉）：文件不会自己变，
 * 卷与网络会——容器启停会接入/断开网络、compose up 会建卷建网，而「哪些容器
 * 此刻接在这个网络上」正是本页最有价值的信息。量级也小（几十条），不值得省。
 */
export const useResourcesStore = defineStore('resources', () => {
  const servers = ref<Server[]>([])
  const selectedId = ref(0)
  const volumes = ref<Volume[]>([])
  const networks = ref<Network[]>([])
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

  function clear(): void {
    volumes.value = []
    networks.value = []
  }

  async function pull(): Promise<void> {
    const list = await fetchServers()
    servers.value = list
    autoSelect()
    const id = selectedId.value
    if (!id) {
      clear()
      return
    }
    const [v, n] = await Promise.all([fetchVolumes(id), fetchNetworks(id)])
    volumes.value = v
    networks.value = n
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

  /** 后台轮询：不动 loading，失败保留上次数据只记原因（同容器页口径）。 */
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
    volumes,
    networks,
    loading,
    refreshing,
    error,
    load,
    poll,
    selectServer,
    start,
    stop,
  }
})
