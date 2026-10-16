/*
 * 镜像（S7.4）。
 *
 * 字段口径：后端 `protocol.ImageSummary` 是 Engine API GET /images/json 的
 * 原始投影（大写 json tag：Id/RepoTags/Size……），与 ContainerSummary 同一种
 * 世界——Wire 与前端 camelCase 之间由 toImage 一处映射。
 *
 * 四个端点的形态差异（S4.1 实测基线）：
 *   * list    —— 信封端点，ImageSummary[]；?all= 含中间层，?dangling=true 是
 *               prune「dry-run 两段式」的第一段（展示清单，不删）。
 *   * pull    —— body {"ref":...} 走 body 不走路径（ref 含 "/" ":" "@"）；
 *               成功也是 200，**业务失败看流内 Err 字段**（Engine 把拉取错误
 *               放在流里而非状态码），故 hasErr 判定在 api 层就做好。
 *   * delete  —— {ref...} 是路径尾通配符，ref 原样拼接**不做 encodeURIComponent**
 *               （编码后的 %2F 过不了尾通配符匹配）；字符集由后端 ValidImageRef
 *               校验，前端 looksLikeImageRef 只做「快速失败」不做权威判定。
 *   * prune   —— 信封端点，PruneResult 原样透传（Engine 清理清单 + 回收空间）。
 */
import { request, postJSON } from './client'

/** 镜像清单的一条：protocol.ImageSummary 的线上形态。 */
export interface ImageWire {
  Id: string
  RepoTags?: string[] | null
  RepoDigests?: string[] | null
  Created: number
  Size: number
  Architecture: string
  Os: string
  Labels?: Record<string, string> | null
}

export interface Image {
  id: string
  /** RepoTags 原样；空数组 = dangling（无 tag 的悬空镜像，prune 的清理对象）。 */
  tags: string[]
  digests: string[]
  /** Unix 秒（Engine 口径），展示层格式化。 */
  created: number
  size: number
  arch: string
  os: string
  /** RepoTags 为空即悬空。nil 与 [] 同判，UI 不区分。 */
  dangling: boolean
}

export function toImage(w: ImageWire): Image {
  const tags = w.RepoTags ?? []
  return {
    id: w.Id,
    tags,
    digests: w.RepoDigests ?? [],
    created: w.Created,
    size: w.Size,
    arch: w.Architecture,
    os: w.Os,
    dangling: tags.length === 0,
  }
}

export interface ImagesQuery {
  /** all=true 含中间层镜像（与 docker images -a 同义，默认不含）。 */
  all?: boolean
  /** dangling=true 只看悬空镜像（prune 两段式的第一段）。 */
  dangling?: boolean
}

export async function fetchImages(serverId: number, q: ImagesQuery = {}): Promise<Image[]> {
  const p = new URLSearchParams()
  if (q.all) p.set('all', 'true')
  if (q.dangling) p.set('dangling', 'true')
  const qs = p.toString()
  const rows = await request<ImageWire[]>(`/servers/${serverId}/images${qs ? `?${qs}` : ''}`)
  return (rows ?? []).map(toImage)
}

/** pull 的汇总结果：Agent 读 Engine 进度流汇总后透传。 */
export interface PullResult {
  /** Engine 最终状态串，如 "Status: Downloaded newer image for alpine:3.20"。 */
  status: string
  /** 流里出现过的层 ID（去重保序）——「拉了哪些层」的账单。 */
  layers: string[]
  /** 流内业务错误文本。**非空即拉取失败，即便 HTTP 200**（Engine 把错误放流里）。 */
  err: string
}

function toPull(w: { Status?: string; Layers?: string[]; Err?: string }): PullResult {
  return { status: w.Status ?? '', layers: w.Layers ?? [], err: w.Err ?? '' }
}

/** 拉取失败（含流内 Err）时抛出，让上层与网络失败走同一条 catch。 */
export async function pullImage(serverId: number, ref: string): Promise<PullResult> {
  const res = await postJSON<{ Status?: string; Layers?: string[]; Err?: string }>(
    `/servers/${serverId}/images/pull`,
    { ref },
  )
  const r = toPull(res)
  if (r.err) {
    throw new Error(r.err)
  }
  return r
}

/**
 * 前端快速失败用的字符集预检，与后端 ValidImageRef 同源思路（不猜语义）：
 * 非空、≤255、字符集 [a-zA-Z0-9_.:/@-]、首字符字母数字、拒 ".."/"//"、
 * 拒以 ":" "/" "@" 结尾。语义对错交给 Engine（404/400 原样透传）。
 */
export function looksLikeImageRef(ref: string): boolean {
  if (!ref || ref.length > 255) {
    return false
  }
  const first = ref[0]
  if (!/[a-zA-Z0-9]/.test(first)) {
    return false
  }
  if (!/^[a-zA-Z0-9_.:/@-]+$/.test(ref)) {
    return false
  }
  if (ref.includes('..') || ref.includes('//')) {
    return false
  }
  const last = ref[ref.length - 1]
  return last !== ':' && last !== '/' && last !== '@'
}

/**
 * 删除单个镜像。ref 是仓库标签（repo:tag）或镜像 ID—— Engine 接受名字/短 ID/
 * 裸 sha256 作引用。被容器占用时 Engine 回 409，原样透传给上层呈现。
 */
export async function deleteImage(serverId: number, ref: string): Promise<void> {
  await request<{ ok: boolean }>(`/servers/${serverId}/images/${ref}`, { method: 'DELETE' })
}

/** prune 清单的一条：Deleted（本体删掉）与 Untagged（只解引用）二选一。 */
export interface PrunedItem {
  deleted: string
  untagged: string
}

export interface PruneResult {
  items: PrunedItem[]
  /** 回收字节数。 */
  spaceReclaimed: number
}

function toPrune(w: {
  ImagesDeleted?: { Deleted?: string; Untagged?: string }[]
  SpaceReclaimed?: number
}): PruneResult {
  return {
    items: (w.ImagesDeleted ?? []).map((it) => ({ deleted: it.Deleted ?? '', untagged: it.Untagged ?? '' })),
    spaceReclaimed: w.SpaceReclaimed ?? 0,
  }
}

export async function pruneImages(serverId: number): Promise<PruneResult> {
  const res = await postJSON<{
    ImagesDeleted?: { Deleted?: string; Untagged?: string }[]
    SpaceReclaimed?: number
  }>(`/servers/${serverId}/images/prune`, {})
  return toPrune(res)
}
