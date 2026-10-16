/*
 * 设置页服务端态（S7.8）。三块数据 + 一个下载：
 *   1. 保护名单 GET/PUT /settings/protected-containers——Hub 侧安全边界，
 *      保存即时生效（后端 containerGuard 逐请求读 settings，无需重启）；
 *   2. TLS 指纹 GET /settings/tls-fingerprint——内置自签证书的 SHA-256，
 *      供浏览器证书固定时人工比对；
 *   3. 整库备份 GET /admin/backup——VACUUM INTO 一致快照的裸流下载。
 *
 * 与 files.ts 的同款口径，动前先读：字段名沿用后端 json tag（snake_case），
 * camelCase 转换在本层完成；backup 是 NoEnvelope 端点（全表第三个，继容器
 * 日志、文件正文之后）：成功是裸流、失败又是 {error} 信封——不能套
 * client.ts 的 request()，仿 files.ts 的 contentFetch 直 fetch，失败时
 * 解析信封取 code/message。
 *
 * 保护名单的存储格式（S7.8 起）是 JSON 数组文本：正则含逗号（量词 {2,3}）
 * 不被切碎；本层只传字符串数组，序列化是后端的事。
 */
import { ApiError } from './errors'
import { putJSON, request } from './client'

/** 保护名单视图：patterns 为后端确认过的正则数组；usingDefault=名单为空（守卫回落默认名单）。 */
export interface ProtectedView {
  patterns: string[]
  usingDefault: boolean
}

interface ProtectedViewWire {
  patterns?: string[]
  using_default?: boolean
}

function toProtectedView(w: ProtectedViewWire): ProtectedView {
  return { patterns: w.patterns ?? [], usingDefault: w.using_default ?? false }
}

/** GET /settings/protected-containers。 */
export function fetchProtectedContainers(): Promise<ProtectedView> {
  return request<ProtectedViewWire>('/settings/protected-containers').then(toProtectedView)
}

/**
 * PUT /settings/protected-containers：整名单覆盖写。
 * 坏正则 400（code=bad_pattern，message 带「第 N 条」）——文案原样透出，
 * 它是用户改错的直接指引；空数组即清除（回落默认名单）。
 */
export function putProtectedContainers(patterns: string[]): Promise<ProtectedView> {
  return putJSON<ProtectedViewWire>('/settings/protected-containers', { patterns }).then(toProtectedView)
}

/** TLS 指纹视图：fingerprint 为空串表示未使用内置自签证书（none / 外部终止 TLS）。 */
export interface TlsFingerprintView {
  fingerprint: string
  selfSigned: boolean
}

/** GET /settings/tls-fingerprint。 */
export function fetchTlsFingerprint(): Promise<TlsFingerprintView> {
  return request<{ fingerprint?: string; self_signed?: boolean }>('/settings/tls-fingerprint').then((w) => ({
    fingerprint: w.fingerprint ?? '',
    selfSigned: w.self_signed ?? false,
  }))
}

/*
 * 下载整库快照（GET /admin/backup）。裸流直 fetch，理由见文件头。
 * 文件名优先取 Content-Disposition（后端给 dockdeck-<UTC 时间戳>.db），
 * 缺失时前端兜底生成——两种情况下用户拿到的都是可用的 .db 文件。
 * 返回实际使用的文件名，供页面提示。
 */
export async function downloadBackup(): Promise<string> {
  let res: Response
  try {
    res = await fetch('/api/v1/admin/backup', { headers: { Accept: '*/*' } })
  } catch (e) {
    throw new ApiError(0, 'network_error', e instanceof Error ? e.message : '网络请求失败')
  }
  if (!res.ok) {
    // 失败体是 {error} 信封（与成功无信封并存，本端点特有形态）。
    let code = 'backup_error'
    let message = res.statusText
    try {
      const body = (await res.json()) as { error?: { code?: string; message?: string } }
      if (body?.error?.code) code = body.error.code
      if (body?.error?.message) message = body.error.message
    } catch {
      // 非 JSON 失败体：保留 statusText
    }
    throw new ApiError(res.status, code, message)
  }
  const blob = await res.blob()
  const name = filenameFromDisposition(res.headers.get('Content-Disposition')) ?? backupFallbackName()
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(a.href)
  return name
}

/** 从 Content-Disposition 取 filename；解析不出返回 null。 */
function filenameFromDisposition(cd: string | null): string | null {
  if (!cd) {
    return null
  }
  const m = /filename="([^"]+)"/.exec(cd)
  return m ? m[1] : null
}

/** 兜底文件名：UTC 秒级时间戳，与后端命名同风格。 */
function backupFallbackName(): string {
  return `dockdeck-${new Date().toISOString().slice(0, 19).replace(/[:T-]/g, '')}.db`
}
