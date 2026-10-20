import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchServers, isOnline, type Server } from '@/api/servers'
import { fetchFiles, type FileEntry, type FilesPage } from '@/api/files'

/**
 * 文件管理页数据（S7.6）。与容器/镜像/Compose store 的两处刻意分叉：
 *
 * 1. **无轮询**：文件清单非实时数据——容器状态会自己变，文件不会。故只有手动
 *    refresh，没有 timer/start/stop（进页面 onMounted 调 load 即首载）。
 * 2. **目录位置进 store，编辑态不进**：currentRoot/currentPath 是「这台机上正在
 *    看哪个目录」的稳定位置（面包屑/文件表/工具栏共享，切走再回来停在原目录是
 *    合理预期）；而「打开哪个文件的编辑面板、草稿改了什么」是进行中的易失会话，
 *    与 Compose 页同理留在页面本地 ref，写入成功后页面调 refresh。
 *
 * root 槽位探测（Q2=A）：后端没有「列根」端点（根数量与宿主机绝对路径属敏感
 * 信息），页面按静态槽位 0..3 逐个试列：200 → 可用，400 → 灰置。探测并行发，
 * ok 槽的目录内容直接充当首屏，不额外再发一次请求。探测本身失败（服务器不在线）
 * 或全槽 400（未配 --fs-root）会置 error——「这台机没开文件管理」是真实状态，
 * 不能静默显示空目录。
 */
export const useFilesStore = defineStore('files', () => {
  /** 静态探测槽位数：拍板值。坏槽由探测收敛，多配的根超出部分看不到（后端无列根端点）。 */
  const ROOT_SLOTS = 4

  /**
   * 在线状态轮询周期：与默认采集周期（30s）对齐。文件清单本身不轮询（文件不会自己变），
   * 但服务器「（离线）」标签以后端 last_seen 为准，不持续刷新就会与总览页矛盾（总览每
   * 30s 刷 last_seen）——表现为「总览显示在线、文件页却标（离线）」。
   */
  const STATUS_REFRESH_MS = 30_000
  let statusTimer: ReturnType<typeof setInterval> | undefined

  const servers = ref<Server[]>([])
  const selectedId = ref(0)
  const rootSlots = ref<{ index: number; status: 'unknown' | 'ok' | 'bad' }[]>([])
  const currentRoot = ref(0)
  /** 相对当前根的路径；'' = 根自身。分段用 '/'（后端 FileEntry.Path 同口径）。 */
  const currentPath = ref('')
  const entries = ref<FileEntry[]>([])
  const truncated = ref(false)
  const loading = ref(false)
  const refreshing = ref(false)
  const probing = ref(false)
  const error = ref<string | null>(null)

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

  async function pullServers(): Promise<void> {
    const list = await fetchServers()
    servers.value = list
    autoSelect()
  }

  /**
   * 探测 0..3 槽位并把第一个 ok 槽的列表直接作首屏。
   * 全 bad 时按「服务器在不在线」给不同的 error：离线是网络问题，在线则说明该机
   * 未配 --fs-root（Agent 一条文件路由都没注册）。
   */
  async function probeRoots(id: number): Promise<void> {
    probing.value = true
    try {
      const results = await Promise.allSettled(
        Array.from({ length: ROOT_SLOTS }, (_, i) => fetchFiles(id, i, '', true)),
      )
      rootSlots.value = results.map((r, i) => ({
        index: i,
        status: r.status === 'fulfilled' ? ('ok' as const) : ('bad' as const),
      }))
      const firstOk = results.findIndex((r) => r.status === 'fulfilled')
      if (firstOk >= 0) {
        const page = (results[firstOk] as PromiseFulfilledResult<FilesPage>).value
        currentRoot.value = firstOk
        currentPath.value = ''
        entries.value = page.entries
        truncated.value = page.truncated
        error.value = null
        return
      }
      currentRoot.value = 0
      currentPath.value = ''
      entries.value = []
      truncated.value = false
      const srv = servers.value.find((s) => s.id === id)
      error.value = srv && isOnline(srv)
        ? '该服务器未配置文件管理根（Agent 未配 --fs-root，文件模块未启用）'
        : '该服务器不在线，无法探测文件根'
    } finally {
      probing.value = false
    }
  }

  /** 拉取指定目录（path 相对 currentRoot；'' = 根自身）。 */
  async function loadDir(path: string): Promise<void> {
    const id = selectedId.value
    if (!id) {
      return
    }
    loading.value = true
    try {
      const page = await fetchFiles(id, currentRoot.value, path)
      currentPath.value = path
      entries.value = page.entries
      truncated.value = page.truncated
      error.value = null
    } catch (e) {
      // 失败保留上次数据只记原因：被一次网络抖动清成空白比显示旧列表更糟。
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 首载：拉服务器清单 → 自动选中 → 探测根槽位（探测结果即首屏）。 */
  async function load(): Promise<void> {
    loading.value = true
    try {
      await pullServers()
      const id = selectedId.value
      if (!id) {
        entries.value = []
        return
      }
      await probeRoots(id)
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
      startStatusPoll()
    }
  }

  /** 手动刷新：顺带拉服务器状态（切换器要显示在线/离线），再重拉当前目录。 */
  async function refresh(): Promise<void> {
    refreshing.value = true
    try {
      await pullServers()
      await loadDir(currentPath.value)
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      refreshing.value = false
    }
  }

  /**
   * 只刷服务器在线状态（切换器的「（离线）」标签），不重拉文件清单。
   * 失败静默：保留上次清单，别为一次抖动弹错或清空服务器选择器（同其它 store 后台轮询口径）。
   */
  async function pollServers(): Promise<void> {
    try {
      await pullServers()
    } catch {
      // 状态轮询失败：静默保留上次数据。
    }
  }

  /** 进入页面开始轮询在线状态；离开务必 stop，否则定时器常驻后台（同 overview 房规）。 */
  function startStatusPoll(): void {
    stopStatusPoll()
    statusTimer = setInterval(() => void pollServers(), STATUS_REFRESH_MS)
  }

  function stopStatusPoll(): void {
    if (statusTimer !== undefined) {
      clearInterval(statusTimer)
      statusTimer = undefined
    }
  }

  /** 切换服务器：每台机的根配置不同，槽位全部作废重探。 */
  async function selectServer(id: number): Promise<void> {
    if (selectedId.value === id) {
      return
    }
    selectedId.value = id
    rootSlots.value = []
    await probeRoots(id)
  }

  /** 切换根槽位（探测亮的才能切；页面置灰的不发射）。 */
  async function setRoot(index: number): Promise<void> {
    if (index === currentRoot.value) {
      return
    }
    currentRoot.value = index
    await loadDir('')
  }

  function enterDir(path: string): Promise<void> {
    return loadDir(path)
  }

  /** 上一级：去掉末段；已在根则空等（页面应置灰）。 */
  function goUp(): Promise<void> {
    const i = currentPath.value.lastIndexOf('/')
    return loadDir(i < 0 ? '' : currentPath.value.slice(0, i))
  }

  /** 面包屑：根目录 + 逐级路径段，每段可点。 */
  const crumbs = computed(() => {
    const parts = currentPath.value ? currentPath.value.split('/') : []
    const list = [{ label: '根目录', path: '' }]
    let acc = ''
    for (const p of parts) {
      acc = acc ? `${acc}/${p}` : p
      list.push({ label: p, path: acc })
    }
    return list
  })

  return {
    servers,
    selectedId,
    selected,
    rootSlots,
    currentRoot,
    currentPath,
    entries,
    truncated,
    loading,
    refreshing,
    probing,
    error,
    crumbs,
    load,
    refresh,
    stopStatusPoll,
    selectServer,
    setRoot,
    enterDir,
    goUp,
  }
})
