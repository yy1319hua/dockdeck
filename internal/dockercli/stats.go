// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器资源统计（v0.2.2 #13）：Engine /containers/{id}/stats 的薄读取层。
// 与容器/镜像一样只做 Engine API 转发——CPU/内存百分比的算术放在
// cmd/agent 的采集源里，这里只负责取原始 JSON。

package dockercli

import (
	"context"
	"fmt"
)

// StatsCPURaw 是 Engine stats 里的单段 CPU 计数（cpu_stats / precpu_stats 共用）。
type StatsCPURaw struct {
	CPUUsage struct {
		TotalUsage uint64   `json:"total_usage"`
		Percpu     []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemCPUUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs     int64  `json:"online_cpus"`
}

// StatsNetworks 是 stats 响应里 per-network 计数，取各网卡之和。
type StatsNetworks struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// StatsJSON 是 Engine stats 响应的本项目所需子集（v0.2.2 #13）。
// 字段名与 Engine 一字不差；没用的字段（blkio 等）不进结构体。
type StatsJSON struct {
	CPUStats    StatsCPURaw `json:"cpu_stats"`
	PreCPUStats StatsCPURaw `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
	} `json:"memory_stats"`
	Networks  map[string]StatsNetworks `json:"networks"`
	PIDsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

// Stats 取单个容器的实时统计（GET /containers/{id}/stats?stream=false）。
// stream=false 是单次快照：Engine 返回当前计数并尽量带上上一拍（precpu_stats），
// 但 precpu 不保证非零——跨拍差分由调用方的缓存负责，这里不猜。
// 容器已删除/已停止返回错误（404），调用方按「跳过该容器」处理。
func (c *Client) Stats(ctx context.Context, id string) (StatsJSON, error) {
	var out StatsJSON
	if id == "" {
		return out, fmt.Errorf("dockercli: 容器 ID 不能为空")
	}
	if err := c.getJSON(ctx, "/containers/"+id+"/stats?stream=false", &out); err != nil {
		return out, err
	}
	return out, nil
}
