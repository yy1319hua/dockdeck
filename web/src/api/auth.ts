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
