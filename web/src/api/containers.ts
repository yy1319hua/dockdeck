/*
 * 容器与容器日志（S7.3）。
 *
 * 字段口径注意：后端 `protocol.ContainerSummary` **有** json tag（Engine 风格
 * 大写：Id/Names/State……），与 servers 列表那组无 tag 的 Go 导出名是两个世界，
 * 别照抄 toServer 的思路——这里 Wire 与前端用的 camelCase 之间仍是 toContainer
 * 一处映射，但线上形态本来就可读，映射只做「Names 取首个、去前导斜杠」。
 */
import { ApiError } from './errors'
import { postJSON, request } from './client'

/** 容器清单的一条：protocol.ContainerSummary 的线上形态。 */
export interface ContainerWire {
  Id: string
  Names: string[]
  Image: string
  ImageID: string
  Command: string
  Created: number
  State: string
  Status: string
  Labels: Record<string, string>
}

export interface Container {
  id: string
  /** Names[0] 去前导斜杠；Engine 保证至少一个名字，兜底用 ID 前 12 位。 */
  name: string
  image: string
  /** 引擎状态词：running / exited / paused / created / restarting / dead…… */
  state: string
  status: string
  created: number
}

export function toContainer(w: ContainerWire): Container {
  const name = (w.Names?.[0] ?? '').replace(/^\//, '')
  return {
    id: w.Id,
    name: name || w.Id.slice(0, 12),
    image: w.Image,
    state: w.State,
    status: w.Status,
    created: w.Created,
  }
}

/** all=true 含已停止容器（省略为 false，与 docker ps 默认口径一致）。 */
export async function fetchContainers(serverId: number, all = false): Promise<Container[]> {
  const q = all ? '?all=true' : ''
  const rows = await request<ContainerWire[]>(`/servers/${serverId}/containers${q}`)
  return (rows ?? []).map(toContainer)
}

export type ContainerAction = 'start' | 'stop' | 'restart' | 'pause' | 'unpause' | 'kill' | 'resize'

export interface ActionOptions {
  /** stop：SIGTERM 后的等待秒数，省略用引擎默认。 */
  timeout_sec?: number
  /** kill：信号名（后端白名单校验），省略用引擎默认。 */
  signal?: string
}

/** 写操作一次失败即如实回报（后端不重试），成功响应体为 {"ok":true}。 */
export async function containerAction(
  serverId: number,
  cid: string,
  action: ContainerAction,
  options?: ActionOptions,
): Promise<void> {
  await postJSON<{ ok: boolean }>(
    `/servers/${serverId}/containers/${encodeURIComponent(cid)}/${action}`,
    options ?? {},
  )
}

export interface RemoveOptions {
  /** 强删运行中的容器。 */
  force?: boolean
  /** 连同匿名卷删除。 */
  volumes?: boolean
  /** 必须与目标容器 ID/名字完全一致——后端 guard 的名字确认门。 */
  confirm: string
}

export async function removeContainer(serverId: number, cid: string, opts: RemoveOptions): Promise<void> {
  const q = new URLSearchParams()
  if (opts.force) q.set('force', '1')
  if (opts.volumes) q.set('v', '1')
  await request<void>(`/servers/${serverId}/containers/${encodeURIComponent(cid)}?${q.toString()}`, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ confirm: opts.confirm }),
  })
}

export interface LogsQuery {
  /** 尾部行数：'all' 或纯数字，省略用后端默认。 */
  tail?: string
  timestamps?: boolean
  since?: string
  until?: string
}

function logsPath(serverId: number, cid: string, q: LogsQuery, exportMode = false): string {
  const p = new URLSearchParams()
  if (q.tail) p.set('tail', q.tail)
  if (q.timestamps) p.set('timestamps', '1')
  if (q.since) p.set('since', q.since)
  if (q.until) p.set('until', q.until)
  if (exportMode) p.set('export', '1')
  return `/servers/${serverId}/containers/${encodeURIComponent(cid)}/logs?${p.toString()}`
}

/*
 * 日志是全表唯一 NoEnvelope 端点：裸流 text/plain，成功时没有 {data} 信封，
 * 失败时却又是 {error} 信封——所以这条通道不能套 request()，单独直 fetch。
 * 同源 cookie 天然带上（与 client.ts 同一理由，不动 credentials）。
 */
async function logsFetch(url: string, signal?: AbortSignal): Promise<Response> {
  let res: Response
  try {
    res = await fetch(`/api/v1${url}`, { signal, headers: { Accept: 'text/plain' } })
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') {
      throw e // 调用方主动中止：原样上抛，由它静默收尾
    }
    throw new ApiError(0, 'network_error', e instanceof Error ? e.message : '网络请求失败')
  }
  if (!res.ok) {
    let message = res.statusText
    try {
      const body = (await res.json()) as { error?: { message?: string } }
      if (body?.error?.message) message = body.error.message
    } catch {
      // 非 JSON 失败体：保留 statusText
    }
    throw new ApiError(res.status, 'logs_error', message)
  }
  return res
}

/** 非 follow：一次性取回全文。 */
export async function fetchLogsText(
  serverId: number,
  cid: string,
  q: LogsQuery,
  signal?: AbortSignal,
): Promise<string> {
  const res = await logsFetch(logsPath(serverId, cid, q), signal)
  return await res.text()
}

/**
 * follow：边收边通过 onChunk 吐出，直到对端关闭或 signal 中止。
 * 后端不设 Hub 侧读取超时，流的寿命归浏览器 ctx——中止即结束。
 */
export async function streamLogs(
  serverId: number,
  cid: string,
  q: LogsQuery,
  onChunk: (text: string) => void,
  signal?: AbortSignal,
): Promise<void> {
  const res = await logsFetch(logsPath(serverId, cid, q), signal)
  if (!res.body) {
    onChunk(await res.text())
    return
  }
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) {
        break
      }
      if (value) {
        onChunk(decoder.decode(value, { stream: true }))
      }
    }
    onChunk(decoder.decode()) // flush 解码器尾部
  } finally {
    reader.releaseLock()
  }
}

/** 导出：Engine 原始字节流（octet-stream + Content-Disposition），触发浏览器下载。 */
export async function exportLogs(serverId: number, cid: string, name: string): Promise<void> {
  const res = await logsFetch(logsPath(serverId, cid, { tail: 'all' }, true))
  const blob = await res.blob()
  // 文件名优先用后端给的 Content-Disposition（Agent 侧生成），拿不到再自造。
  const disposition = res.headers.get('Content-Disposition') ?? ''
  const m = /filename="?([^"]+)"?/.exec(disposition)
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = m?.[1] ?? `${name}-logs.txt`
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(a.href)
}
