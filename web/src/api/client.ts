/*
 * 极薄的请求层：只做三件事——拼基址、拆信封、把失败统一成 ApiError。
 *
 * 零依赖（不用 axios）：项目不引重型框架，且需求只到「同源 JSON + 会话 cookie」。
 * 同源说明：生产由 Hub 同时提供 SPA 与 /api；开发期由 vite dev server 反代，
 * 浏览器看到的仍是同源，故 cookie 天然带上——**不要**改 credentials，也不要带 Bearer
 * （ARCHITECTURE：会话不识别 Bearer 头，Token 不能访问 UI 专属端点）。
 */
import { ApiError } from './errors'

const BASE = '/api/v1'

interface ErrorPart {
  code: string
  message: string
}

interface ResponseBody<T> {
  data?: T
  error?: ErrorPart
}

function parse<T>(text: string): ResponseBody<T> | null {
  if (!text) {
    return null
  }
  try {
    return JSON.parse(text) as ResponseBody<T>
  } catch {
    // 非 JSON 响应（例如网关插进来的 HTML）：按「拿不到结构体」处理，不是致命错误。
    return null
  }
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response
  try {
    res = await fetch(BASE + path, {
      ...init,
      headers: { Accept: 'application/json', ...(init?.headers ?? {}) },
    })
  } catch (e) {
    // 网络层失败没有状态码：status 记为 0，供上层与 401/403 区分。
    throw new ApiError(0, 'network_error', e instanceof Error ? e.message : '网络请求失败')
  }

  const body = parse<T>(await res.text())

  if (!res.ok) {
    throw new ApiError(res.status, body?.error?.code ?? 'http_error', body?.error?.message ?? res.statusText)
  }

  return (body?.data ?? null) as T
}

export function postJSON<T>(path: string, payload: unknown): Promise<T> {
  return request<T>(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  })
}

/** JSON 请求体的 PUT（S7.8 设置页保护名单覆盖写用）。与 postJSON 同口径，只换方法。 */
export function putJSON<T>(path: string, payload: unknown): Promise<T> {
  return request<T>(path, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  })
}

export function post<T>(path: string): Promise<T> {
  return request<T>(path, { method: 'POST' })
}
