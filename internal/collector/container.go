// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package collector

import "github.com/yy1319hua/dockdeck/internal/protocol"

// ContainerStatsSource 是「每容器资源占用」的数据来源抽象。
//
// S2.4 只定契约与消费端（Agent 端点）；真实实现由 S3.1 的 Docker Engine API
// 客户端提供（GET /containers/{id}/stats?stream=false，语义见
// docs/ARCHITECTURE.md「数据流」——Agent 只转发 Docker API，不读 cgroup 自算）。
// 抽出接口是为了让 Agent 在 docker 源缺席时（S3 之前、引擎不可达、非 root 等）
// 也能完整装配与测试，而不是为一个可选数据源卡住整条链路。
type ContainerStatsSource interface {
	// ContainerStats 返回全部容器的实时资源占用快照。
	// 返回的切片按容器 ID 升序排序，保证输出稳定可断言。
	ContainerStats() ([]protocol.ContainerStat, error)
}

// NilStatsSource 是「无 docker 源」的默认实现：永远返回空列表、无错误。
//
// 语义是「这台 Agent 暂时没有容器指标可报」，而不是故障——调用方拿到空列表
// 就照常返回 200，让 Hub 把容器曲线留空，而不是把机器判成异常。
type NilStatsSource struct{}

// ContainerStats 实现 ContainerStatsSource。
func (NilStatsSource) ContainerStats() ([]protocol.ContainerStat, error) {
	return []protocol.ContainerStat{}, nil
}
