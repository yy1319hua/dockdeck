/*
 * Compose 项目与 YAML 文件（S7.5 消费 S4.2/S4.3/S4.7 已落地的六个端点）。
 *
 * 字段口径：Wire 用后端 protocol 的 snake_case json tag（config_file/backup_file），
 * 与前端 camelCase 之间由 toProject 一处映射（与 toImage/toContainer 同款）。
 *
 * 路径口径：项目名是 URL 路径段，走 encodeURIComponent——这里与镜像 delete 的
 * {ref...} **相反**：compose 路由是普通路径段，Go 1.22 ServeMux 会解码 %XX，
 * 目录名含空格/中文也能正确到达 Agent；而镜像尾通配符遇 %2F 匹配失败、必须
 * 原样拼接。两处差异由路由形态决定，不是疏漏，别为「一致性」互相抄。
 *
 * 动作白名单：up/down/restart/pull 与 protocol.ValidComposeAction 一致，
 * 前端只提供这四个按钮，不给用户输入动词的入口（绝不把字符串透传给 shell）。
 *
 * 日志通道（GET .../logs 裸流）本期不做（S7.5 拍板范围不含日志），
 * 若要补，仿 containers.ts 的 NoEnvelope 直 fetch，不要套 request()。
 */
import { post, postJSON, request } from './client'

/** 后端 ComposeProject.status 的四个取值（dockercli 实测口径）。 */
export type ComposeStatus = 'running' | 'partial' | 'stopped' | 'unknown'

const COMPOSE_STATUSES: readonly string[] = ['running', 'partial', 'stopped', 'unknown']

function toStatus(s: string | undefined): ComposeStatus {
  return s && COMPOSE_STATUSES.includes(s) ? (s as ComposeStatus) : 'unknown'
}

/** 项目里的一个服务实例（docker compose ps 的一行）。 */
export interface ComposeService {
  name: string
  state: string
  image: string
}

export interface ComposeProject {
  /** 目录名（白名单内标识；跨目录重名时后端按首个匹配解析，后者不可达）。 */
  name: string
  /** 项目目录绝对路径（只读展示）。 */
  path: string
  /** compose 文件绝对路径（只读展示）。 */
  configFile: string
  status: ComposeStatus
  services: ComposeService[]
}

interface ComposeProjectWire {
  name: string
  path: string
  config_file: string
  status: string
  services?: { name: string; state: string; image: string }[]
}

function toProject(w: ComposeProjectWire): ComposeProject {
  return {
    name: w.name,
    path: w.path,
    configFile: w.config_file,
    status: toStatus(w.status),
    services: (w.services ?? []).map((s) => ({ name: s.name, state: s.state, image: s.image })),
  }
}

/** 列举主机上白名单目录内的 compose 项目（空清单为 [] 非 null）。 */
export async function fetchComposeProjects(serverId: number): Promise<ComposeProject[]> {
  const rows = await request<ComposeProjectWire[]>(`/servers/${serverId}/compose`)
  return (rows ?? []).map(toProject)
}

/** 写动作白名单：与 protocol.ValidComposeAction 一致。 */
export type ComposeActionName = 'up' | 'down' | 'restart' | 'pull'

export interface ComposeActionResult {
  /** compose 原生输出（stdout+stderr 合流）——业务成败写在这里，不在状态码。 */
  output: string
}

/** 执行一个写动作。端点不读请求体，走无体 POST；输出原样透传给上层展示。 */
export async function composeAction(
  serverId: number,
  project: string,
  action: ComposeActionName,
): Promise<ComposeActionResult> {
  const res = await post<{ output?: string }>(
    `/servers/${serverId}/compose/${encodeURIComponent(project)}/${action}`,
  )
  return { output: res.output ?? '' }
}

/** compose 文件当前内容与绝对路径（只读；写走下面的 diff 预览 + PUT）。 */
export interface ComposeFile {
  configFile: string
  content: string
}

export async function fetchComposeFile(serverId: number, project: string): Promise<ComposeFile> {
  const res = await request<{ config_file: string; content: string }>(
    `/servers/${serverId}/compose/${encodeURIComponent(project)}/file`,
  )
  return { configFile: res.config_file, content: res.content }
}

/**
 * 两段式写入的第一段：算 diff——不落盘、不产生备份。
 * 返回 unified diff 纯文本，着色（+/- 行）由展示层负责。
 */
export async function diffComposeFile(serverId: number, project: string, content: string): Promise<string> {
  const res = await postJSON<{ diff?: string }>(
    `/servers/${serverId}/compose/${encodeURIComponent(project)}/diff`,
    { content },
  )
  return res.diff ?? ''
}

/**
 * 两段式写入的第二段：覆盖写。Agent 侧写前备份 <file>.bak.<RFC3339>，
 * 备份落盘成功才动原文件；返回备份文件路径。
 * 注意：备份的「列表/读取/恢复」后端无端点（S7.5 拍板拆 S7.10 待立项），
 * 本期只把该路径展示给用户，回滚需人工上宿主机。
 */
export async function putComposeFile(serverId: number, project: string, content: string): Promise<string> {
  const res = await request<{ backup_file: string }>(
    `/servers/${serverId}/compose/${encodeURIComponent(project)}/file`,
    {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content }),
    },
  )
  return res.backup_file
}
