/*
 * 文件管理（S7.6）。消费 S5.1–S5.4/S5.6（Agent 侧）+ S5.5（Hub 侧转发）已落地的
 * 端点；路径白名单、realpath 解引用、写前备份、敏感黑名单全在 Agent 侧，
 * Hub 只做鉴权审计与转发——前端同样不复制任何路径校验。
 *
 * 三处与既有页面不同的口径，动前先读：
 * 1. **寻址全走查询参数**（root/path），不经 URL 路径段——与 Compose 的 {p}
 *    路径段是两个世界；path 一律 URLSearchParams 编码（可能含空格/中文），
 *    与后端 agentcli 的 fileItemQuery 同一套编码口径。
 * 2. **content 读是全表第二个 NoEnvelope 端点**（继容器日志之后）：裸流，成功无
 *    信封、元信息走 X-DD-File-* 响应头，失败又是 {error} 信封——GET 不能套
 *    request()，仿 containers.ts 的 logsFetch 直 fetch；PUT 的请求体是裸文件
 *    内容（不套信封），响应却是信封 JSON（FileWritten），可套 request()。
 * 3. **root 是多根索引**（缺省 0）：根数量与宿主机绝对路径属敏感信息，后端没有
 *    「列根」端点，页面按静态槽位探测可用性（见 stores/files.ts）。
 *
 * 动作白名单：mkdir/delete/rename 与 protocol.ValidFileAction 一致，前端只出
 * 三个按钮，不给用户输入动词的入口（绝不把字符串透传给文件操作）。
 *
 * stat 端点（GET /files/stat）本期不接：列表项已带 name/size/mode/mod_time，
 * 预览截断看 content 响应头即可——等真有「打开前先看元信息」的需求再接。
 */
import { ApiError } from './errors'
import { postJSON, request } from './client'

/** 目录列表的一条：protocol.FileEntry 的线上形态（snake_case tag）。 */
export interface FileEntry {
  /** 文件名（不含路径）。 */
  name: string
  /** 相对白名单根的路径（"a/b.txt"）——后端刻意不外泄宿主机绝对路径。 */
  path: string
  isDir: boolean
  /** 目录无意义，约定 0。 */
  size: number
  /** 八进制权限串，形如 "0644"。 */
  mode: string
  /** RFC3339 修改时间；格式化在页面层做，api 层原样透传。 */
  modTime: string
}

interface FileEntryWire {
  name: string
  path: string
  is_dir: boolean
  size: number
  mode: string
  mod_time: string
}

function toFileEntry(w: FileEntryWire): FileEntry {
  return {
    name: w.name,
    path: w.path,
    isDir: w.is_dir,
    size: w.size,
    mode: w.mode,
    modTime: w.mod_time,
  }
}

export interface FilesPage {
  entries: FileEntry[]
  /** 条目超上限被截断（后端默认 2000）——页面须标注，不能当「就这么多」。 */
  truncated: boolean
}

/** root/path 查询串：root 缺省 0，path 为空表示根自身（合法查询，照发不跳过）。 */
function itemQuery(root: number, path: string): string {
  const q = new URLSearchParams()
  if (root !== 0) q.set('root', String(root))
  q.set('path', path)
  return q.toString()
}

/**
 * 列举目录内容（GET /files?root=&path=&depth=）。
 * depth 只发 1（单层口径）：后端 depth>1 是递归扁平列表，整树预拉会撞
 * maxEntries 上限且截断语义按层才说得清（S7.6 拍板 Q1=A）。
 */
export async function fetchFiles(
  serverId: number,
  root: number,
  path: string,
  probe = false,
): Promise<FilesPage> {
  // probe=true 仅用于 store 的 root 槽位探测：带 X-DD-Probe 头，让 Hub 审计中间件
  // 跳过这类「预期失败」的试探——越界 root 返 400 是探测的正常结果，不该进审计表。
  const res = await request<{ entries?: FileEntryWire[]; truncated?: boolean }>(
    `/servers/${serverId}/files?${itemQuery(root, path)}&depth=1`,
    probe ? { headers: { 'X-DD-Probe': '1' } } : undefined,
  )
  return {
    entries: (res.entries ?? []).map(toFileEntry),
    truncated: res.truncated ?? false,
  }
}

/** content 裸流的一次读取结果。Body 的关闭责任在调用方。 */
export interface FileContent {
  blob: Blob
  /** 后端探测的内容类型（未知为 application/octet-stream）。 */
  contentType: string
  /** 原始字节数（元信息，非 blob 长度——截断时两者不同）。 */
  size: number
  /** 超单文件上限被截断：此时 blob 不是完整文件，UI 必须标注。 */
  truncated: boolean
}

/*
 * content 读：全表第二个 NoEnvelope 端点，单独直 fetch（理由见文件头第 2 条）。
 * 同源 cookie 天然带上（与 client.ts 同一理由，不动 credentials）。
 */
async function contentFetch(serverId: number, root: number, path: string): Promise<Response> {
  let res: Response
  try {
    res = await fetch(`/api/v1/servers/${serverId}/files/content?${itemQuery(root, path)}`, {
      headers: { Accept: '*/*' },
    })
  } catch (e) {
    throw new ApiError(0, 'network_error', e instanceof Error ? e.message : '网络请求失败')
  }
  if (!res.ok) {
    // 失败时是 {error} 信封（与成功无信封并存，本端点特有形态）。
    let message = res.statusText
    try {
      const body = (await res.json()) as { error?: { message?: string } }
      if (body?.error?.message) message = body.error.message
    } catch {
      // 非 JSON 失败体：保留 statusText
    }
    throw new ApiError(res.status, 'file_error', message)
  }
  return res
}

/** 读取文件正文（预览/下载共用）。元信息从 X-DD-File-* 响应头取。 */
export async function fetchFileContent(
  serverId: number,
  root: number,
  path: string,
): Promise<FileContent> {
  const res = await contentFetch(serverId, root, path)
  const size = Number(res.headers.get('X-DD-File-Size') ?? '0')
  return {
    blob: await res.blob(),
    contentType: res.headers.get('Content-Type') ?? 'application/octet-stream',
    size: Number.isFinite(size) ? size : 0,
    truncated: res.headers.get('X-DD-Truncated') === '1',
  }
}

/** 触发浏览器下载（文件名取 path 末段；后端不给了 Content-Disposition）。 */
export async function downloadFile(serverId: number, root: number, path: string): Promise<void> {
  const res = await contentFetch(serverId, root, path)
  const blob = await res.blob()
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = path.split('/').pop() || 'download'
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(a.href)
}

/*
 * 上传上限与鉴权签名体是同一个数（后端 protocol.MaxFileUploadBytes = 16 MiB）：
 * Hub 要把整个 body 读进内存验签，超限请求出门也是白跑（Hub 侧 413），
 * 故在 api 层就近拦掉。改上限需前后端一起发版，不是随手调参。
 */
const MAX_UPLOAD_BYTES = 16 << 20

/** 写入文件内容（PUT /files/content?root=&path=）。请求体即文件内容，不套信封。 */
export async function putFileContent(
  serverId: number,
  root: number,
  path: string,
  content: Blob,
): Promise<{ backupFile: string; created: boolean; size: number }> {
  if (content.size > MAX_UPLOAD_BYTES) {
    throw new ApiError(
      413,
      'too_large',
      `文件 ${content.size} 字节超过上传上限 ${MAX_UPLOAD_BYTES} 字节（16 MiB）`,
    )
  }
  // 响应是信封 JSON（FileWritten）——与 GET 的裸流形态不同，可走 request()。
  const res = await request<{ backup_file?: string; created?: boolean; size?: number }>(
    `/servers/${serverId}/files/content?${itemQuery(root, path)}`,
    { method: 'PUT', body: content },
  )
  return {
    backupFile: res.backup_file ?? '',
    created: res.created ?? false,
    size: res.size ?? 0,
  }
}

/** 写动作白名单：与 protocol.ValidFileAction 一致。 */
export type FileActionName = 'mkdir' | 'delete' | 'rename'

export interface FileActionDone {
  action: FileActionName
  path: string
  /** rename 后的新相对路径；其余动作为空。 */
  dest: string
}

/**
 * mkdir / delete（仅空目录）/ rename（dest 同守白名单）。
 * 动作名同时进 URL 段与请求体——Agent 侧要求二者一致，这里统一填同一值。
 */
export async function fileAction(
  serverId: number,
  root: number,
  action: FileActionName,
  path: string,
  dest = '',
): Promise<FileActionDone> {
  const res = await postJSON<{ action: string; path: string; dest?: string }>(
    `/servers/${serverId}/files/${action}?root=${root}`,
    { action, path, dest },
  )
  return { action: res.action as FileActionName, path: res.path, dest: res.dest ?? '' }
}
