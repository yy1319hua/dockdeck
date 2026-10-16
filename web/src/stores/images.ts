import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchServers, isOnline, type Server } from '@/api/servers'
import { fetchImages, type Image } from '@/api/images'
import { REFRESH_MS } from './overview'

/**
 * 镜像页数据（S7.4）。与容器 store 同构：全局页 + 顶部主机切换器，
 * 30s 轮询与采集周期对齐；镜像没有「实时占用」第二数据源，一行就是一条 Image。
 *
 * 两个显示口径（与 docker images 的习惯对齐）：
 *   * danglingOnly —— 只看悬空镜像（RepoTags 为空的孤儿层，prune 的清理对象）；
 *   * showIntermediate —— 含中间层（-a）。默认都不开，与 docker images 默认一致。
 */
export const useImagesStore = defineStore('images', () => {
  const servers = ref<Server[]>([])
  const selectedId = ref(0)
  const danglingOnly = ref(false)
  const showIntermediate = ref(false)
  const images = ref<Image[]>([])
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
      images.value = []
      return
    }
    images.value = await fetchImages(id, { all: showIntermediate.value, dangling: danglingOnly.value })
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

  /** 后台轮询与容器页同款：不动 loading，失败保留上次数据只记原因。 */
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

  async function toggleDangling(): Promise<void> {
    danglingOnly.value = !danglingOnly.value
    await load()
  }

  async function toggleIntermediate(): Promise<void> {
    showIntermediate.value = !showIntermediate.value
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
    danglingOnly,
    showIntermediate,
    images,
    loading,
    refreshing,
    error,
    load,
    poll,
    selectServer,
    toggleDangling,
    toggleIntermediate,
    start,
    stop,
  }
})
