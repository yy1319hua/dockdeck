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
  /** 容器名（compose ps 的 Name/Names 首段；老版本 compose 可能不给）。 */
  container?: string
  /** 容器短 ID（compose ps 的 ID）。 */
  containerId?: string
}

export interface ComposeProject {
  /**
   * 白名单内的唯一标识（前端拿它做路由段与列表 key）。
   * 目录名；同目录多份 compose 时为「目录名-角色后缀」（如 dockdeck-agent）；
   * 再同名则后端追加 -2、-3 序号，保证每个项目都能被解析到。
   */
  name: string
  /** 项目目录绝对路径（只读展示）。 */
  path: string
  /** compose 文件绝对路径（只读展示）。 */
  configFile: string
  status: ComposeStatus
  services: ComposeService[]
  /**
   * compose 自己认的项目名（compose 文件顶层 name:，无则目录名）。
   *
   * 与 name 可能不同：name 是面板路由用的「目录名-角色后缀」，而 compose
   * 执行时以文件里的 name: 为准。两者不一致时面板看到的与实际操作的
   * 不是同一个对象，故单独展示出来供核对。
   */
  composeName?: string
  /** 发现来源：白名单目录扫描 / 容器 label 反推，可同时命中两个。 */
  sources?: ComposeSource[]
  /**
   * 同名冲突：同一个 compose 项目名出现在多个目录下。
   *
   * compose 按项目名管理容器，故这几份文件在 compose 眼里是同一个项目，
   * 操作其一实际作用到另一组容器。冲突项目的写操作被后端拒绝。
   */
  conflict?: boolean
  /** 冲突的其它 compose 文件绝对路径。 */
  conflictPaths?: string[]
}

/** compose 项目的发现来源。 */
export type ComposeSource = 'whitelist' | 'label'

const COMPOSE_SOURCES: readonly string[] = ['whitelist', 'label']

interface ComposeProjectWire {
  name: string
  path: string
  config_file: string
  status: string
  compose_name?: string
  sources?: string[]
  conflict?: boolean
  conflict_paths?: string[]
  services?: {
    name: string
    state: string
    image: string
    container?: string
    container_id?: string
  }[]
}

/** 后端给的来源值只认这两个；其余一律忽略（未知来源不该让前端崩）。 */
function toSource(s: string): ComposeSource | null {
  return COMPOSE_SOURCES.includes(s) ? (s as ComposeSource) : null
}

function toProject(w: ComposeProjectWire): ComposeProject {
  return {
    name: w.name,
    path: w.path,
    configFile: w.config_file,
    status: toStatus(w.status),
    composeName: w.compose_name,
    sources: (w.sources ?? []).map(toSource).filter((s): s is ComposeSource => s !== null),
    conflict: w.conflict === true,
    conflictPaths: w.conflict_paths ?? [],
    services: (w.services ?? []).map((s) => ({
      name: s.name,
      state: s.state,
      image: s.image,
      container: s.container,
      containerId: s.container_id,
    })),
  }
}

/** 列举服务器上白名单目录内的 compose 项目（空清单为 [] 非 null）。 */
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

/* ---------- 写前备份（S7.11） ---------- */

/**
 * 一条写前备份。**只含文件名/大小/时间**——后端不回宿主机绝对路径（与文件
 * 管理模块同口径），前端也不需要路径：定位靠「项目名 + 文件名」。
 */
export interface ComposeBackup {
  name: string
  size: number
  /** RFC3339 文本（UTC），仅用于展示；清单顺序由后端给出（最新在前）。 */
  modTime: string
}

function toBackup(w: { name?: string; size?: number; mod_time?: string }): ComposeBackup {
  return { name: w.name ?? '', size: w.size ?? 0, modTime: w.mod_time ?? '' }
}

/** 项目的写前备份清单（只读）。 */
export async function fetchComposeBackups(serverId: number, project: string): Promise<ComposeBackup[]> {
  const res = await request<{ backups?: { name?: string; size?: number; mod_time?: string }[] }>(
    `/servers/${serverId}/compose/${encodeURIComponent(project)}/backups`,
  )
  return (res.backups ?? []).map(toBackup)
}

/**
 * 读备份正文（预览与 diff 用）。
 *
 * 备份名含 RFC3339 的 `+` 与 `:`（如 `+08:00`），故**走查询串并转义**——
 * 与上面「项目名 encodeURIComponent 进路径段」是两套处理，别混：
 * 路径段的编码差异 S7.5 踩过一次，这里不再留第二次。
 */
export async function fetchComposeBackupContent(
  serverId: number,
  project: string,
  name: string,
): Promise<string> {
  const res = await request<{ name?: string; content?: string }>(
    `/servers/${serverId}/compose/${encodeURIComponent(project)}/backups/content?file=${encodeURIComponent(name)}`,
  )
  return res.content ?? ''
}

/**
 * 用备份覆盖当前 compose 文件（恢复）。
 *
 * 恢复本身是一次覆盖写，后端会先把「恢复前的当前内容」备份成新的
 * `.bak.<RFC3339>` 再覆盖——否则回滚是单向门：恢复到旧版本后就回不去了。
 * 返回那份新备份的文件名（仅文件名，不含目录）。
 *
 * 注意：改 YAML 不会直接改动运行中的容器，需再执行 up 才生效——这条写进
 * 页面的确认文案，别让人以为点上就生效了。
 */
export async function restoreComposeBackup(
  serverId: number,
  project: string,
  name: string,
): Promise<string> {
  const res = await postJSON<{ backup?: string }>(
    `/servers/${serverId}/compose/${encodeURIComponent(project)}/backups/restore`,
    { file: name },
  )
  return res.backup ?? ''
}
