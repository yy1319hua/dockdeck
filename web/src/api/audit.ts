/*
 * 审计日志查询（S7.7）。消费后端 GET /api/v1/audit_logs（S7.7 后端同期落地）。
 *
 * 三处口径，动前先读：
 * 1. **倒序 + before_id 游标分页**：后端按 id 倒序（最新在前），响应 next_before=0
 *    表示没有更多，否则原样作为下一次请求的 before_id。limit 缺省 100、上限 1000，
 *    本层默认显式发 100（不依赖后端缺省值，调参只改一处）。
 * 2. **过滤四维**：from/to（RFC3339，后端缺省最近 7 天窗口）、server_id、
 *    action 前缀（如 container 命中 container.* 全部）、result（ok|error）。
 *    时间参数由本层 isoSeconds 归一成秒级 UTC——后端表 ts 是秒级 TEXT 按字典序
 *    比较，带毫秒/偏移的串直接透传会错位（归一在后端也会做，这里先做一遍让
 *    请求 URL 干净可复制）。
 * 3. **查询端点自身不写审计行**（后端刻意不包 Audit 中间件）：刷新/翻页/导出
 *    都不会在审计表里留痕，页面可以放心提供手动刷新按钮，无需防连点节流。
 */
import { request } from './client'

/** 审计行：protocol.AuditLog 的线上形态（snake_case tag）。 */
export interface AuditLog {
  id: number
  /** RFC3339 秒级 UTC（后端表 DEFAULT 口径）。 */
  ts: string
  userId: number
  username: string
  /** 0 = 与具体服务器无关的动作（token/auth 类），UI 显示「—」。 */
  serverId: number
  /** 「资源.动作」命名，如 container.remove / auth.login。 */
  action: string
  target: string
  detail: string
  /** ok | error。 */
  result: string
  error: string
  ip: string
  requestId: string
}

interface AuditLogWire {
  id?: number
  ts?: string
  user_id?: number
  username?: string
  server_id?: number
  action?: string
  target?: string
  detail?: string
  result?: string
  error?: string
  ip?: string
  request_id?: string
}

function toAuditLog(w: AuditLogWire): AuditLog {
  return {
    id: w.id ?? 0,
    ts: w.ts ?? '',
    userId: w.user_id ?? 0,
    username: w.username ?? '',
    serverId: w.server_id ?? 0,
    action: w.action ?? '',
    target: w.target ?? '',
    detail: w.detail ?? '',
    result: w.result ?? '',
    error: w.error ?? '',
    ip: w.ip ?? '',
    requestId: w.request_id ?? '',
  }
}

/** 查询过滤条件；undefined/空串的维度后端按「不限」处理。 */
export interface AuditFilter {
  /** RFC3339（含）。页面 datetime-local 值经 isoSeconds 转换后传入。 */
  from?: string
  to?: string
  serverId?: number
  /** 动作前缀，如 container。 */
  action?: string
  result?: string
}

export interface AuditPage {
  logs: AuditLog[]
  /** 0 = 没有更多；否则作为下一次请求的 before_id。 */
  nextBefore: number
}

/** Date → 秒级 RFC3339（去毫秒）：与后端表 ts 口径一致。 */
export function isoSeconds(d: Date): string {
  return d.toISOString().replace(/\.\d+Z$/, 'Z')
}

/** 每页条数默认值：与后端 auditDefaultLimit 一致（改需前后端一起发版）。 */
export const AUDIT_PAGE_LIMIT = 100

/**
 * 查询一页审计日志（GET /audit_logs）。
 * beforeId > 0 时只返回 id 更小的行（翻页）；limit 显式发送，不吃后端缺省。
 */
export async function fetchAuditLogs(
  filter: AuditFilter,
  beforeId = 0,
  limit: number = AUDIT_PAGE_LIMIT,
): Promise<AuditPage> {
  const q = new URLSearchParams()
  if (filter.from) q.set('from', filter.from)
  if (filter.to) q.set('to', filter.to)
  if (filter.serverId && filter.serverId > 0) q.set('server_id', String(filter.serverId))
  if (filter.action) q.set('action', filter.action)
  if (filter.result) q.set('result', filter.result)
  if (beforeId > 0) q.set('before_id', String(beforeId))
  q.set('limit', String(limit))
  const res = await request<{ logs?: AuditLogWire[]; next_before?: number }>(
    `/audit_logs?${q.toString()}`,
  )
  return {
    logs: (res.logs ?? []).map(toAuditLog),
    nextBefore: res.next_before ?? 0,
  }
}
