/*
 * 会话相关接口。字段名沿用后端 json tag（snake_case）——这是线格式，不要重命名，
 * 转换到 camelCase 的事交给上层的 store/组件，好让契约改动时只需要改一处。
 */
import { post, postJSON, request } from './client'

/** GET /api/v1/auth/me：未登录也返回 200，靠 setup_required 区分「该建号」与「该登录」。 */
export interface MeResponse {
  username: string
  setup_required: boolean
}

export interface Credentials {
  username: string
  password: string
}

export function fetchMe(): Promise<MeResponse> {
  return request<MeResponse>('/auth/me')
}

export function login(cred: Credentials): Promise<null> {
  return postJSON<null>('/auth/login', cred)
}

export function setup(cred: Credentials): Promise<null> {
  return postJSON<null>('/auth/setup', cred)
}

export function logout(): Promise<null> {
  return post<null>('/auth/logout')
}

/**
 * POST /auth/password：改密。**成功后后端撤销该用户全部会话（含当前）**，
 * 响应 relogin_required=true 即要求前端立刻登出并回登录页——抱着旧 cookie
 * 继续跑只会在下一个请求 401。
 *
 * 失败按后端 code 原样抛 ApiError，页面据此提示：
 *   invalid_credentials —— 旧口令错误（401）；
 *   weak_password       —— 新口令不足 8 位（400）。
 */
export function changePassword(oldPassword: string, newPassword: string): Promise<{ reloginRequired: boolean }> {
  return postJSON<{ ok?: boolean; relogin_required?: boolean }>('/auth/password', {
    old_password: oldPassword,
    new_password: newPassword,
  }).then((w) => ({ reloginRequired: w.relogin_required ?? false }))
}
