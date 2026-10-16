/*
 * 卷与网络（S7.9）。
 *
 * 字段口径：后端 protocol.VolumeSummary / NetworkSummary 是 Engine API
 * GET /volumes、GET /networks 的原始投影（大写 json tag：Name/Driver/
 * UsageData/Containers……），与 ImageSummary 同一种世界——Wire 与前端
 * camelCase 之间由 toVolume / toNetwork 一处映射。
 *
 * 两条**只读**端点，无写方法：卷删除即删数据、prune 的 dangling 判定由
 * Engine 做出且误判不可逆，面板不提供清理入口（需要清理走宿主机 docker CLI，
 * 有人工判断在场）。
 *
 * 三个已在后端钉死的形态，前端照此呈现（别自行"优化"）：
 *   * 卷的 UsageData 可能为 null——语义是「Engine 没统计」，**不是**「0 个容器
 *     在用」。混同这两者会让用户误判卷可删，故 null 一律显示「—」；
 *     Size 为 -1 同样是「未统计」，不是 0 字节。
 *   * 卷的 Mountpoint 不在线上——后端 DTO 就不含该字段：宿主机绝对路径不外泄，
 *     与文件模块「对外不回宿主机绝对路径」同口径。前端也不需要它。
 *   * 网络的 Containers 是「容器 ID → 条目」的映射，清单里就带回来了——
 *     故「谁接入了这个网络」不需要额外请求；卷没有这个福利，只有 RefCount 计数。
 */
import { request } from './client'

/** Engine 的卷用量统计（UsageData）。 */
export interface VolumeUsageWire {
  Size?: number
  RefCount?: number
}

export interface VolumeWire {
  Name?: string
  Driver?: string
  Scope?: string
  Labels?: Record<string, string> | null
  Options?: Record<string, string> | null
  CreatedAt?: string
  /** 可能为 null：Engine 未做统计（部分驱动如此）。 */
  UsageData?: VolumeUsageWire | null
}

export interface VolumeUsage {
  /** 字节数；-1 表示 Engine 未统计。 */
  size: number
  refCount: number
}

export interface Volume {
  name: string
  driver: string
  scope: string
  labels: Record<string, string>
  /** RFC3339 文本（可能带纳秒与偏移）。 */
  createdAt: string
  /** null = Engine 没给统计。不是 0，也不是空卷。 */
  usage: VolumeUsage | null
}

export function toVolume(w: VolumeWire): Volume {
  const u = w.UsageData
  return {
    name: w.Name ?? '',
    driver: w.Driver ?? '',
    scope: w.Scope ?? '',
    labels: w.Labels ?? {},
    createdAt: w.CreatedAt ?? '',
    usage: u ? { size: u.Size ?? -1, refCount: u.RefCount ?? 0 } : null,
  }
}

/** 网络里的一条接入容器（Engine 的 Containers 映射值）。 */
export interface NetworkContainerWire {
  Name?: string
  EndpointID?: string
  MacAddress?: string
  IPv4Address?: string
  IPv6Address?: string
}

export interface NetworkIPAMConfigWire {
  Subnet?: string
  IPRange?: string
  Gateway?: string
}

export interface NetworkWire {
  Id?: string
  Name?: string
  Driver?: string
  Scope?: string
  Created?: string
  Internal?: boolean
  Attachable?: boolean
  Ingress?: boolean
  EnableIPv6?: boolean
  Labels?: Record<string, string> | null
  IPAM?: { Driver?: string; Config?: NetworkIPAMConfigWire[] | null } | null
  /** 键是容器 ID；无容器接入时为 null 或空对象。 */
  Containers?: Record<string, NetworkContainerWire> | null
}

export interface NetworkContainer {
  /** 容器 ID（映射的键）。 */
  id: string
  name: string
  ipv4: string
  ipv6: string
  mac: string
}

export interface Network {
  id: string
  name: string
  driver: string
  scope: string
  created: string
  /** 内部网络：不与宿主机外部通信。 */
  internal: boolean
  /** 允许手工接入（docker network connect）。 */
  attachable: boolean
  /** swarm ingress 网络。 */
  ingress: boolean
  ipv6: boolean
  subnets: string[]
  gateway: string
  /** 接入的容器名单（Engine 在清单里就给全了，零额外请求）。 */
  containers: NetworkContainer[]
}

export function toNetwork(w: NetworkWire): Network {
  const cfg = w.IPAM?.Config ?? []
  const map = w.Containers ?? {}
  return {
    id: w.Id ?? '',
    name: w.Name ?? '',
    driver: w.Driver ?? '',
    scope: w.Scope ?? '',
    created: w.Created ?? '',
    internal: !!w.Internal,
    attachable: !!w.Attachable,
    ingress: !!w.Ingress,
    ipv6: !!w.EnableIPv6,
    subnets: cfg.map((c) => c.Subnet ?? '').filter((s) => s !== ''),
    gateway: cfg.find((c) => c.Gateway)?.Gateway ?? '',
    containers: Object.entries(map).map(([id, c]) => ({
      id,
      name: c.Name ?? '',
      ipv4: c.IPv4Address ?? '',
      ipv6: c.IPv6Address ?? '',
      mac: c.MacAddress ?? '',
    })),
  }
}

/** 数据卷清单（只读，无 filters——本页不做清理，不需要 dangling 预演）。 */
export async function fetchVolumes(serverId: number): Promise<Volume[]> {
  const rows = await request<VolumeWire[]>(`/servers/${serverId}/volumes`)
  return (rows ?? []).map(toVolume)
}

/** 容器网络清单（只读）。每项自带接入容器名单。 */
export async function fetchNetworks(serverId: number): Promise<Network[]> {
  const rows = await request<NetworkWire[]>(`/servers/${serverId}/networks`)
  return (rows ?? []).map(toNetwork)
}
