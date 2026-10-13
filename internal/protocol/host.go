// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import "time"

// HostMetrics 是 Agent 对一台宿主机采集一次得到的完整快照。
//
// Agent 侧不落地历史（除最近一次快照），历史序列由 Hub 轮询写入 metrics_snapshots，
// 因此本结构代表「某一时刻」，不代表区间。
type HostMetrics struct {
	// CollectedAt 是采集时刻。Hub 用它做时间差推算速率，也用于判断 Agent 是否还活着。
	CollectedAt time.Time  `json:"collected_at"`
	CPU         CPUStat    `json:"cpu"`
	Memory      MemoryStat `json:"memory"`
	// Disks 是按配置的挂载点逐个采集的结果，未配置时为空切片而非 nil，
	// 这样前端不需要区分「没配」和「采到 0 个」。
	Disks []DiskStat `json:"disks"`
	// Network 逐网卡。回环与未启用的网卡是否包含由 Agent 配置决定。
	Network   []NetStat `json:"network"`
	Load      LoadStat  `json:"load"`
	Processes int       `json:"processes"`
	UptimeSec uint64    `json:"uptime_sec"`
}

// CPUStat 是 CPU 占用情况。
//
// User/System/IOWait/Steal/Idle 是自开机以来的累计 jiffies（原始值，便于事后核对口径）；
// UsagePercent 由 Agent 对 /proc/stat 做两次采样取差值算出，是 UI 实际展示的那个数。
type CPUStat struct {
	Cores int `json:"cores"`
	// UsagePercent 取值 0–100，含 user/system/iowait/steal。
	UsagePercent float64 `json:"usage_percent"`
	User         uint64  `json:"user"`
	System       uint64  `json:"system"`
	IOWait       uint64  `json:"iowait"`
	Steal        uint64  `json:"steal"`
	Idle         uint64  `json:"idle"`
}

// MemoryStat 是内存与 swap 用量，单位 byte。
//
// Used = Total − Available：MemAvailable 已经把可回收的 page cache 算进去，
// 比 Total − Free 更贴近真实占用，后者会把缓存误算成已用。
type MemoryStat struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
	Free      uint64 `json:"free"`
	Buffers   uint64 `json:"buffers"`
	Cached    uint64 `json:"cached"`
	SwapTotal uint64 `json:"swap_total"`
	SwapUsed  uint64 `json:"swap_used"`
}

// DiskStat 是单个挂载点的磁盘用量，容量单位 byte。
type DiskStat struct {
	Mount string `json:"mount"`
	// FSType 取自 statfs，如 ext4、overlay。
	FSType      string  `json:"fs_type"`
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Free        uint64  `json:"free"`
	UsedPercent float64 `json:"used_percent"`
}

// NetStat 是单张网卡的网络流量。
//
// 全部字段都是开机以来的累计值，不是速率。速率由 Hub 拿两次快照按时间差计算——
// 放在 Hub 侧算，Agent 就不必保存上一次的采样，重装后速率也能立刻恢复正确。
type NetStat struct {
	Interface string `json:"interface"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	TxPackets uint64 `json:"tx_packets"`
	RxErrors  uint64 `json:"rx_errors"`
	TxErrors  uint64 `json:"tx_errors"`
}

// LoadStat 是系统平均负载，分别对应 1/5/15 分钟。
type LoadStat struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// AgentInfo 是 Agent 自身的静态信息，由 /info 返回，Hub 在纳管时读取一次。
//
// 它变化极慢，因此与高频的 HostMetrics 分开，避免每条快照都重复带一遍主机名和内核版本。
type AgentInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kernel   string `json:"kernel"`
	// AgentVersion 是构建时注入的版本串，未注入时为空。
	AgentVersion string `json:"agent_version"`
	// ProtocolVersion 恒为 protocol.ProtocolVersion，Hub 据此判断是否兼容。
	ProtocolVersion string    `json:"protocol_version"`
	Cores           int       `json:"cores"`
	MemoryTotal     uint64    `json:"memory_total"`
	BootTime        time.Time `json:"boot_time"`
}
