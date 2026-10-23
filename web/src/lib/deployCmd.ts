/*
 * Agent 部署命令生成：纯函数，面板两处共用——「添加节点」(AddNode.vue) 与
 * 「服务器部署配置」(DeployConfigDialog.vue)。抽成独立模块，避免两套模板各写各的、
 * 改一处漏另一处（那样面板产出的 compose 就会不一致，违背「面板获取的即正确」）。
 *
 * 逻辑与 AddNode 原 currentCommand 完全一致：镜像加速替换、pid:host、日志轮转、
 * 反向长连接（v0.2.6 起唯一形态）都在此一处定义。
 */

export interface DeployOptions {
  dir: string
  tag: string
  /** 加速地址预设值；'__custom__' 表示取 customMirror。 */
  mirror: string
  /** 选了「自定义…」时生效的加速域名。 */
  customMirror: string
  fsRoots: string
  composeDirs: string
  selfContainer: string
  diskMounts: string
  logSize: string
  logFiles: string
}

export type DeployTab = 'docker' | 'compose'

function activeDir(o: DeployOptions): string {
  return o.dir || '/opt/dockdeck'
}
function activeFsRoots(o: DeployOptions): string {
  return o.fsRoots || activeDir(o)
}
function activeComposeDirs(o: DeployOptions): string {
  return o.composeDirs || activeDir(o)
}
function activeTag(o: DeployOptions): string {
  return o.tag || 'latest'
}
function activeSelf(o: DeployOptions): string {
  return o.selfContainer || 'dockdeck-agent'
}
function activeMirror(o: DeployOptions): string {
  const raw = o.mirror === '__custom__' ? o.customMirror : o.mirror
  return raw.trim().replace(/\/+$/, '')
}
function agentImage(o: DeployOptions): string {
  const m = activeMirror(o)
  return m
    ? `${m}/yy1319hua/dockdeck-agent:${activeTag(o)}`
    : `ghcr.io/yy1319hua/dockdeck-agent:${activeTag(o)}`
}

/**
 * 生成 Agent 部署命令。secret 是 DD_AGENT_TOKEN 用的令牌（添加节点时是一次性
 * join_secret；重新部署时是服务器原长期令牌——两者都走同一份模板）。
 * tab 决定输出 docker run 还是 compose 片段。hubUrl 取 window.location.origin，
 * 页面不硬编码任何域名。
 */
export function buildDeployCmd(
  o: DeployOptions,
  secret: string,
  tab: DeployTab,
  hubUrl: string,
): string {
  if (tab === 'docker') {
    const env: string[] = [
      `-e DD_HUB_URL=${hubUrl}`,
      `-e DD_AGENT_TOKEN=${secret}`,
      `-e DD_AGENT_FS_ROOTS=${activeFsRoots(o)}`,
      `-e DD_AGENT_COMPOSE_DIRS=${activeComposeDirs(o)}`,
      `-e DD_AGENT_SELF_CONTAINER=${activeSelf(o)}`,
    ]
    if (o.diskMounts) {
      env.push(`-e DD_AGENT_DISK_MOUNTS=${o.diskMounts}`)
    }
    const lines = [
      `docker run -d --name ${activeSelf(o)} --restart unless-stopped --network host \\`,
      '  -v /var/run/docker.sock:/var/run/docker.sock:ro \\',
      '  --pid host \\',
      `  -v ${activeDir(o)}:${activeDir(o)} \\`,
      ...env.map((x) => `  ${x} \\`),
    ]
    if (o.logSize && o.logFiles) {
      lines.push('  --log-driver json-file \\')
      lines.push(`  --log-opt max-size=${o.logSize} \\`)
      lines.push(`  --log-opt max-file=${o.logFiles} \\`)
    }
    lines.push(`  ${agentImage(o)}`)
    return lines.join('\n')
  }
  const cmdLines = [
    '      - --docker-socket',
    '      - /var/run/docker.sock',
    '      - --self-container',
    `      - ${activeSelf(o)}`,
    '      - --compose-dirs',
    `      - ${activeComposeDirs(o)}`,
    '      - --fs-root',
    `      - ${activeFsRoots(o)}`,
  ]
  const envLines = [
    `      DD_HUB_URL: ${hubUrl}`,
    `      DD_AGENT_TOKEN: ${secret}`,
    `      DD_AGENT_SELF_CONTAINER: ${activeSelf(o)}`,
    `      DD_AGENT_FS_ROOTS: ${activeFsRoots(o)}`,
    `      DD_AGENT_COMPOSE_DIRS: ${activeComposeDirs(o)}`,
  ]
  if (o.diskMounts) {
    envLines.push(`      DD_AGENT_DISK_MOUNTS: ${o.diskMounts}`)
  }
  const lines = [
    'services:',
    `  ${activeSelf(o)}:`,
    `    image: ${agentImage(o)}`,
    `    container_name: ${activeSelf(o)}`,
    '    network_mode: host',
    '    pid: host',
    '    restart: unless-stopped',
    '    command:',
    ...cmdLines,
    '    volumes:',
    '      - /var/run/docker.sock:/var/run/docker.sock:ro',
    `      - ${activeDir(o)}:${activeDir(o)}`,
    '    environment:',
    ...envLines,
  ]
  if (o.logSize && o.logFiles) {
    lines.push(
      '    logging:',
      '      driver: json-file',
      '      options:',
      `        max-size: ${o.logSize}`,
      `        max-file: '${o.logFiles}'`,
    )
  }
  return lines.join('\n')
}

/** AddNode 页的默认部署参数；重新部署弹窗也用同一组默认值（与「添加节点」一致），
 *  保证面板两处产出的 compose 完全一致。 */
export const DEFAULT_DEPLOY_OPTIONS: DeployOptions = {
  dir: '/opt/dockdeck',
  tag: 'latest',
  mirror: '',
  customMirror: '',
  fsRoots: '',
  composeDirs: '',
  selfContainer: 'dockdeck-agent',
  diskMounts: '',
  logSize: '10m',
  logFiles: '3',
}
