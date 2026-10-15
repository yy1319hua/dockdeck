/*
 * 指标接口。字段名沿用后端 json tag（snake_case），转换留给上层——
 * 契约一变只改一处，不用在组件里翻。
 */
import { request } from './client'

/** 单容器的实时占用（与 protocol.ContainerStat 一一对应）。 */
export interface ContainerStatWire {
  id: string
  name: string
  cpu_percent: number
  mem_usage: number
  mem_limit: number
  mem_percent: number
  net_rx: number
  net_tx: number
  pids: number
}

/** GET /servers/{id}/metrics 的响应：实时快照。 */
export interface MetricsLatestWire {
  server_id: number
  /** 快照落库时刻（RFC3339）。 */
  ts: string
  metrics: HostMetricsWire | null
  /** 无容器指标时为 null——后端刻意区分「无容器指标」与「空列表」。 */
  containers: ContainerStatWire[] | null
}

export interface HostMetricsWire {
  collected_at: string
  cpu: {
    cores: number
    usage_percent: number
    user: number
    system: number
    iowait: number
    steal: number
    idle: number
  }
  memory: {
    total: number
    used: number
    available: number
    free: number
    buffers: number
    cached: number
    swap_total: number
    swap_used: number
  }
  disks: Array<{
    mount: string
    fs_type: string
    total: number
    used: number
    free: number
    used_percent: number
  }>
  network: Array<{
    interface: string
    rx_bytes: number
    tx_bytes: number
    rx_packets: number
    tx_packets: number
    rx_errors: number
    tx_errors: number
  }>
  load: { one: number; five: number; fifteen: number }
  processes: number
  uptime_sec: number
}

/** 历史曲线上的一个点（metrics_snapshots 的列一一对应）。 */
export interface HostPointWire {
  ts: string
  cpu_pct: number
  mem_bytes: number
  mem_total: number
  disk_bytes: number
  disk_total: number
  net_rx: number
  net_tx: number
  load1: number
}

/** 容器历史点（container_snapshots 的列一一对应）。 */
export interface ContainerPointWire {
  ts: string
  cid: string
  name: string
  cpu_pct: number
  mem_bytes: number
  mem_limit: number
  mem_pct: number
  net_rx: number
  net_tx: number
  pids: number
}

export interface OverviewWire {
  server_id: number
  from: string
  to: string
  points: HostPointWire[]
  containers: ContainerPointWire[]
}

export function fetchLatest(serverId: number): Promise<MetricsLatestWire> {
  return request<MetricsLatestWire>(`/servers/${serverId}/metrics`)
}

/** 历史曲线。不传 from/to 时后端默认最近 1 小时、720 点。 */
export function fetchOverview(
  serverId: number,
  from?: string,
  to?: string,
  limit = 720,
): Promise<OverviewWire> {
  const q = new URLSearchParams({ limit: String(limit) })
  if (from) q.set('from', from)
  if (to) q.set('to', to)
  return request<OverviewWire>(`/servers/${serverId}/metrics/overview?${q.toString()}`)
}

/** 可切换的历史区间。后端窗口上限 8 天，故最大档取 7 天。 */
export const RANGES = [
  { key: '1h', label: '1 小时', dur: 60 * 60 * 1000 },
  { key: '6h', label: '6 小时', dur: 6 * 60 * 60 * 1000 },
  { key: '24h', label: '24 小时', dur: 24 * 60 * 60 * 1000 },
  { key: '7d', label: '7 天', dur: 7 * 24 * 60 * 60 * 1000 },
] as const

export type RangeKey = (typeof RANGES)[number]['key']

export function rangeWindow(key: RangeKey): { from: string; to: string } {
  const r = RANGES.find((x) => x.key === key) ?? RANGES[0]
  const to = new Date()
  const from = new Date(to.getTime() - r.dur)
  return { from: from.toISOString(), to: to.toISOString() }
}
