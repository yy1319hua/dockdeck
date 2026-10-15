/*
 * 统一错误语义。
 *
 * 后端一律用 httpx.WriteError 输出 { "error": { code, message } }；
 * code 取自 internal/protocol.Code*（bad_request / unauthorized / forbidden /
 * not_found / conflict / rate_limited / too_large …），前端据此判定处置方式，
 * 不要用 HTTP 状态码硬套语义。
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

/** 凭证缺失或失效——通常意味着会话过期，应引导重新登录。 */
export function isUnauthorized(e: unknown): e is ApiError {
  return e instanceof ApiError && e.status === 401
}

/** 凭证有效但权限不足——不是登录问题，不该把用户踢到登录页。 */
export function isForbidden(e: unknown): e is ApiError {
  return e instanceof ApiError && e.status === 403
}

/** 触发限流——提示重试即可，不必登出。 */
export function isRateLimited(e: unknown): e is ApiError {
  return e instanceof ApiError && (e.status === 429 || e.code === 'rate_limited')
}

/** 把任意异常转成可展示的 { code, message }。 */
export function toErrorInfo(e: unknown): { code: string; message: string } {
  if (e instanceof ApiError) {
    return { code: e.code, message: e.message }
  }
  if (e instanceof Error) {
    return { code: 'client_error', message: e.message }
  }
  return { code: 'unknown', message: '未知错误' }
}
