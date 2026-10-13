// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import "time"

// ContainerInfo 是容器清单里的一条，对应 Agent 侧 Docker Engine API 的只读投影。
//
// 字段语义以真实引擎为准，S3 完成后会拿 ARM 宿主机上的真实返回值逐字段核对并回填
// docs/ARCHITECTURE.md 的「接口契约」。在此之前，本结构的字段只作占位，
// 不允许据此推断引擎行为。
type ContainerInfo struct {
	// ID 是容器完整 ID。
	ID string `json:"id"`
	// Name 不带前导斜杠，与 docker ps 的 NAMES 列一致。
	Name    string `json:"name"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	// State 是引擎给出的状态词（running/exited/paused 等）。
	State string `json:"state"`
	// Status 是人类可读的状态串（如 Up 3 hours），仅用于展示，不用于判断。
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	Labels    map[string]string `json:"labels,omitempty"`
	Ports     []PortBinding     `json:"ports,omitempty"`
}

// PortBinding 是一条端口映射。
//
// 端口用字符串而非整数：引擎可能返回 "3000/tcp" 这类带协议的复合值，
// 用整数会在解析阶段就丢信息。
type PortBinding struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      string `json:"host_port"`
	ContainerPort string `json:"container_port"`
	Protocol      string `json:"protocol"`
}

// ContainerStat 是单个容器的实时资源占用。
//
// 与 HostMetrics 一样只报当前值；MemUsage/MemLimit 单位 byte，
// CPUPercent/MemPercent 是 0–100。MemLimit 为 0 表示未设内存上限，
// 此时 MemPercent 无意义，UI 不应展示百分比。
type ContainerStat struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpu_percent"`
	MemUsage   uint64  `json:"mem_usage"`
	MemLimit   uint64  `json:"mem_limit"`
	MemPercent float64 `json:"mem_percent"`
	NetRx      uint64  `json:"net_rx"`
	NetTx      uint64  `json:"net_tx"`
	// PIDS 是容器内进程数。
	PIDS uint64 `json:"pids"`
}
