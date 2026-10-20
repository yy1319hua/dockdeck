/*
 * 受管服务器清单。
 *
 * 注意：后端的 ServerSummary 没有 json tag，线上字段是 Go 的导出名（大写开头），
 * 所以这里有两套类型——Wire 照抄线上形态，Server 是前端用的 camelCase，
 * 转换只发生在 toServer 一处。将来后端若补了 json tag，改这里就够了。
 */
import { postJSON, request } from './client'

export interface ServerWire {
  ID: number
  Name: string
  BaseURL: string
  Arch: string
  DockVer: string
  Enabled: boolean
  LastSeen: string
  LastError: string
}

export interface Server {
  id: number
  name: string
  baseUrl: string
  arch: string
  dockVer: string
  enabled: boolean
  lastSeen: string
  lastError: string
}

export function toServer(w: ServerWire): Server {
  return {
    id: w.ID,
    name: w.Name,
    baseUrl: w.BaseURL,
    arch: w.Arch,
    dockVer: w.DockVer,
    enabled: w.Enabled,
    lastSeen: w.LastSeen,
    lastError: w.LastError,
  }
}

export async function fetchServers(): Promise<Server[]> {
  const rows = await request<ServerWire[]>('/servers')
  return (rows ?? []).map(toServer)
}

/** 在线判定窗口：默认采集周期 30s，留三倍余量——抖动一次不该被判离线。 */
export const ONLINE_WINDOW_SEC = 90

/**
 * 在线状态以后端的 last_seen 为准（见 GET /api/v1/servers 的说明）。
 * 空值表示从未通讯过，一律算离线。
 */
export function isOnline(s: Server, now: number = Date.now()): boolean {
  if (!s.lastSeen) {
    return false
  }
  const t = Date.parse(s.lastSeen)
  if (Number.isNaN(t)) {
    return false
  }
  return now - t <= ONLINE_WINDOW_SEC * 1000
}

/** 纳管邀请：明文 join_secret 仅签发响应可见一次，落库只存哈希。 */
export interface Invite {
  joinSecret: string
  /** 有效期（秒）。页面应在到期后停止展示/复制。 */
  expiresIn: number
  warning: string
}

interface InviteWire {
  join_secret: string
  expires_in: number
  warning: string
}

/**
 * 签发一次性纳管邀请（POST /servers/invite，仅浏览器会话可调）。
 *
 * ttl_min 默认 15：够用户在目标机器上粘贴命令，又短到泄露窗口有限。
 * 后端上限 24h，超出会被静默 clamp——这里不重复校验，以响应的 expires_in 为准。
 */
export async function createInvite(ttlMin = 15, nameHint?: string): Promise<Invite> {
  const w = await postJSON<InviteWire>('/servers/invite', {
    ttl_min: ttlMin,
    ...(nameHint ? { name_hint: nameHint } : {}),
  })
  return { joinSecret: w.join_secret, expiresIn: w.expires_in, warning: w.warning }
}
