/*
 * API Token 管理（S7.8 设置页第二分区）。端点自 S1.10 起齐全（仅会话），
 * 本期补前端消费。三条安全口径，动前先读：
 *
 *   1. **明文只在签发响应里出现一次**：库里只有哈希，列表/详情都不返回
 *      明文。页面必须在签发当时就把 plaintext 展示给用户（带复制按钮与
 *      「仅此一次」警示），关掉即无处可查。
 *   2. **本组端点仅浏览器会话**（后端 route.AuthSession）：泄漏的 Token
 *      不能自签更多 Token，否则权限收不回来。前端直连即可（cookie 同源
 *      带上），不要带 Bearer。
 *   3. ttl 白名单与后端 ttlChoices 一致（7d|30d|90d|1y|never）：
 *      前端只给这几个选项，不让用户输任意时长——把过期策略变成摆设的
 *      入口不开。
 */
import { postJSON, request } from './client'

/** 一枚 Token 的元数据（线上形态的 camelCase 化）。 */
export interface ApiToken {
  id: number
  name: string
  scopes: string[]
  /** RFC3339；签发时间必有。 */
  createdAt: string
  /** 最后使用时间；从未使用为空串。 */
  lastUsedAt: string
  /** 过期时间；永不过期（ttl=never）为空串。 */
  expiresAt: string
  /** 撤销时间；未撤销为空串。 */
  revokedAt: string
  /** 后端按当前时间判定的可用性（未撤销且未过期）。 */
  active: boolean
}

interface TokenViewWire {
  id: number
  name: string
  scopes: string[]
  created_at: string
  last_used_at?: string
  expires_at?: string
  revoked_at?: string
  active: boolean
}

function toToken(w: TokenViewWire): ApiToken {
  return {
    id: w.id,
    name: w.name,
    scopes: w.scopes ?? [],
    createdAt: w.created_at,
    lastUsedAt: w.last_used_at ?? '',
    expiresAt: w.expires_at ?? '',
    revokedAt: w.revoked_at ?? '',
    active: w.active,
  }
}

/** GET /api-tokens：全部 Token（含已撤销），按 id 倒序（后端序）。 */
export function fetchTokens(): Promise<ApiToken[]> {
  return request<TokenViewWire[]>('/api-tokens').then((ws) => (ws ?? []).map(toToken))
}

/** scope 字典的一项：scope 名与中文说明（后端单点维护，UI 不复制文案）。 */
export interface ScopeItem {
  scope: string
  description: string
}

/** GET /scopes：公开端点，不含本机信息，只是权限名词表。 */
export function fetchScopes(): Promise<ScopeItem[]> {
  return request<{ scope: string; description: string }[]>('/scopes').then((ws) => ws ?? [])
}

/** ttl 白名单（与后端 ttlChoices 的键一致）。空串不在选项里——UI 总给显式值。 */
export const TOKEN_TTLS = ['7d', '30d', '90d', '1y', 'never'] as const

export type TokenTtl = (typeof TOKEN_TTLS)[number]

export interface CreateTokenInput {
  name: string
  scopes: string[]
  ttl: TokenTtl
}

/** 签发结果：token 是元数据，plaintext 仅此一次可见，warning 是后端给的警示文案。 */
export interface CreateTokenResult {
  token: ApiToken
  plaintext: string
  warning: string
}

/**
 * POST /api-tokens。失败按后端 code 原样抛 ApiError，页面据此提示：
 *   bad_request  —— 名称缺失 / scope 未知或为空 / 含未知字段；
 *   bad_ttl      —— ttl 不在白名单（UI 不该让它发生，挡在表单层）；
 *   name_taken   —— 名称被占用（409）。
 */
export function createToken(input: CreateTokenInput): Promise<CreateTokenResult> {
  return postJSON<{ token: TokenViewWire; plaintext: string; warning?: string }>('/api-tokens', input).then(
    (w) => ({ token: toToken(w.token), plaintext: w.plaintext, warning: w.warning ?? '' }),
  )
}

/** DELETE /api-tokens/{id}：作废一枚 Token。幂等否定——已撤销的再撤返回 404。 */
export function revokeToken(id: number): Promise<void> {
  return request<{ revoked?: number }>(`/api-tokens/${id}`, { method: 'DELETE' }).then(() => undefined)
}
