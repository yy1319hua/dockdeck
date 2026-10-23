// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// engineStatsSource（v0.2.2 #13）：ContainerStatsSource 的真实现。
// 数据来自 Engine /containers/{id}/stats?stream=false（经 dockercli），逐容器
// 取快照后算 CPU/内存占用，映射为 protocol.ContainerStat 上行 Hub。
//
// CPU% 口径与 docker stats 一致：跨拍差分 (Δcpu / Δsystem) × online_cpus × 100。
// stream=false 的 precpu_stats 在部分引擎/存储驱动下为零，不可靠——因此本源
// 自己缓存上一拍计数（按容器 ID），首拍或计数回卷时返回 0，下一拍起正常。
// 停止/消失的容器跳过（docker stats 也只报运行中的）；单容器失败不拖垮整拍。

package agenthttp

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// cpuSample 是一次 stats 快照里算 CPU% 需要的最小集合。
type cpuSample struct {
	total  uint64
	system uint64
	online int64
	at     time.Time
}

// engineStatsSource 实现 collector.ContainerStatsSource（接口在 collector 包，
// Agent 组装处在 cmd/agent/main.go——为免 cmd↔internal 反向依赖只在此实现）。
type engineStatsSource struct {
	dc *dockercli.Client

	mu     sync.Mutex
	prev   map[string]cpuSample // key: 容器 ID
	prevAt time.Time
}

// NewEngineStatsSource 构造真实现。Engine 不可达时 Collect 侧返回空列表
// （containerStatsHandler 的降级语义），构造不需要引擎在线。
func NewEngineStatsSource(socket string) *engineStatsSource {
	return &engineStatsSource{dc: dockercli.New(socket), prev: map[string]cpuSample{}}
}

// ContainerStats 实现 collector.ContainerStatsSource。
// sorted 输出与接口注释一致（按 ID 升序）。
func (s *engineStatsSource) ContainerStats() ([]protocol.ContainerStat, error) {
	// List 与 stats 采集解耦：列容器名要快，stats 采样各自独立超时。早先一个全局 8s
	// ctx 串行采集，容器一多就吃满整拍预算，尾部容器被 continue 跳过——表现就是
	// 「容器占用显示不全」（截图只见前几个，且没有「还有 N 个未列出」提示）。
	listCtx, listCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer listCancel()
	sums, err := s.dc.List(listCtx, false) // 只列运行中：停止的容器 docker stats 也不报
	if err != nil {
		return nil, err
	}

	now := time.Now()
	out := make([]protocol.ContainerStat, len(sums))
	next := make(map[string]cpuSample, len(sums))

	// 并发采集：单容器 stats?stream=false 要约 1–2s，串行时 N 个容器就是 N 秒量级。
	// 每个容器独立超时 + 限流并发（护住 Engine），单点失败（退出/删除/超时）只跳过自己。
	const (
		perContainerTimeout = 6 * time.Second
		maxConcurrency      = 5
	)
	var (
		wg sync.WaitGroup
		mu sync.Mutex // 保护 next map 的并发写与 out 槽位
	)
	sem := make(chan struct{}, maxConcurrency)
	for i, c := range sums {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c dockercli.ContainerSummary) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, ccancel := context.WithTimeout(context.Background(), perContainerTimeout)
			defer ccancel()
			raw, err := s.dc.Stats(cctx, c.ID)
			if err != nil {
				return // 单容器瞬时失败只跳过，不影响其它容器与整拍
			}
			mu.Lock()
			out[i] = s.statFor(c, raw, now, next) // statFor 写 next map，须持 mu
			mu.Unlock()
		}(i, c)
	}
	wg.Wait()

	s.mu.Lock()
	s.prev = next
	s.prevAt = now
	s.mu.Unlock()

	// 压紧被跳过的零值槽，再按 ID 升序（接口要求只含成功项、按 ID 排序）。
	compact := out[:0]
	for _, st := range out {
		if st.ID != "" {
			compact = append(compact, st)
		}
	}
	sort.Slice(compact, func(i, j int) bool { return compact[i].ID < compact[j].ID })
	return compact, nil
}

// statFor 由一份原始 stats 算出 ContainerStat，并把这拍的 CPU 计数写入 next
// 供下一拍差分。CPU% 首拍为 0（无上一拍），与 docker stats 首拍行为一致。
func (s *engineStatsSource) statFor(c dockercli.ContainerSummary, raw dockercli.StatsJSON, now time.Time, next map[string]cpuSample) protocol.ContainerStat {
	cur := cpuSample{
		total:  raw.CPUStats.CPUUsage.TotalUsage,
		system: raw.CPUStats.SystemCPUUsage,
		online: raw.CPUStats.OnlineCPUs,
		at:     now,
	}
	if cur.online <= 0 { // cgroup v1 老引擎没有 online_cpus：按 percpu 长度推
		cur.online = int64(len(raw.CPUStats.CPUUsage.Percpu))
	}
	if cur.online <= 0 {
		cur.online = 1
	}
	next[c.ID] = cur

	var cpuPct float64
	s.mu.Lock()
	last, ok := s.prev[c.ID]
	s.mu.Unlock()
	if ok && cur.total > last.total && cur.system > last.system {
		dtCPU := float64(cur.total - last.total)
		dtSys := float64(cur.system - last.system)
		cpuPct = (dtCPU / dtSys) * float64(cur.online) * 100
	}

	var rx, tx uint64
	for _, n := range raw.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}

	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	return protocol.ContainerStat{
		ID:   c.ID,
		Name: name,
		// CPUPercent 单核百分比（docker stats 口径），可超过 100%。
		CPUPercent: cpuPct,
		// CPUCores 占用核数：与主机侧 busy_cores 同量纲，可直接对照。
		CPUCores: cpuPct / 100,
		// CPUPercentNormalized 整机归一口径：与主机侧 usage_percent 同量纲，
		// 可与同屏的主机值直接比较（不分母是可见核数，兜底已保证 >=1）。
		CPUPercentNormalized: cpuPct / float64(cur.online),
		MemUsage:             raw.MemoryStats.Usage,
		MemLimit:             raw.MemoryStats.Limit,
		MemPercent:           memPct(raw.MemoryStats.Usage, raw.MemoryStats.Limit),
		NetRx:                rx,
		NetTx:                tx,
		PIDS:                 raw.PIDsStats.Current,
	}
}

// memPct 与 docker stats 同口径：无上限（cgroup v2 无 memory.max）时不给百分比。
func memPct(usage, limit uint64) float64 {
	if limit == 0 || usage > limit {
		return 0
	}
	return float64(usage) / float64(limit) * 100
}
