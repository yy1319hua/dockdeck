/*
 * Hub 构建元数据（GET /system/version，仅会话）。
 *
 * 设置页「版本信息」分区消费；拉不到不挡其他设置——调用方各自容错
 * （旧版 Hub 没有这个端点时会 404，页面显示「获取失败」而非整页报错）。
 */
import { request } from './client'

export interface BuildInfo {
  version: string
  commit: string
  date: string
}

export async function fetchBuildInfo(): Promise<BuildInfo> {
  return request<BuildInfo>('/system/version')
}
