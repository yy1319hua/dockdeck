import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchServers, isOnline, type Server } from '@/api/servers'
import { fetchComposeProjects, type ComposeProject } from '@/api/compose'
import { REFRESH_MS } from './overview'

/**
 * Compose 页数据（S7.5）。与镜像/容器 store 同构：全局页 + 顶部服务器切换器，
 * 30s 轮询与采集周期对齐；项目清单一行就是一个 ComposeProject（含服务快照）。
 *
 * 边界说明——**编辑态不进 store**：当前文件内容、草稿、diff 预览、写入中标记
 * 都是「打开哪个项目的编辑面板」的临时 UI 态，随页面卸载即弃，放 pinia 会让
 * 它们跨路由存活（切走再回来看到半个编辑会话）。故本 store 只持有共享数据
 * （servers/选中项/项目清单），编辑流程由页面组件用本地 ref 管理；
 * 写入成功后页面主动调 reload 刷新清单（up/down 后项目状态会变）。
 */
export const useComposeStore = defineStore('compose', () => {
  const servers = ref<Server[]>([])
  const selectedId = ref(0)
  const projects = ref<ComposeProject[]>([])
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
      projects.value = []
      return
    }
    projects.value = await fetchComposeProjects(id)
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

  /** 后台轮询与容器/镜像页同款：不动 loading，失败保留上次数据只记原因。 */
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

  /** 写入/动作成功后由页面调用：重新拉项目清单（状态与服务快照可能已变）。 */
  async function reload(): Promise<void> {
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
    projects,
    loading,
    refreshing,
    error,
    load,
    poll,
    selectServer,
    reload,
    start,
    stop,
  }
})
