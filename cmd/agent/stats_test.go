// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// mkStats 造一份 Engine stats 快照。
func mkStats(total, system uint64, online int64, mem, limit uint64) dockercli.StatsJSON {
	var s dockercli.StatsJSON
	s.CPUStats.CPUUsage.TotalUsage = total
	s.CPUStats.SystemCPUUsage = system
	s.CPUStats.OnlineCPUs = online
	s.MemoryStats.Usage = mem
	s.MemoryStats.Limit = limit
	s.PIDsStats.Current = 7
	s.Networks = map[string]dockercli.StatsNetworks{}
	s.Networks["eth0"] = struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	}{RxBytes: 100, TxBytes: 50}
	return s
}

// TestEngineStatsSource_TwoBeatCPUPercent 回归 v0.2.2 #13：CPU% 用跨拍差分，
// 与 docker stats 同口径；首拍 0，次拍 (Δcpu/Δsys)×cpus×100。
func TestEngineStatsSource_TwoBeatCPUPercent(t *testing.T) {
	s := newEngineStatsSource("/nonexistent.sock") // 不联网，只测 statFor 数学
	sum := dockercli.ContainerSummary{ID: "abc", Names: []string{"/web"}}

	now := time.Now()
	next := map[string]cpuSample{}
	first := s.statFor(sum, mkStats(1000, 100000, 2, 30<<20, 1<<30), now, next)
	if first.CPUPercent != 0 {
		t.Errorf("首拍无上一拍应 0，得 %v", first.CPUPercent)
	}
	if first.Name != "web" || first.MemUsage != 30<<20 || first.MemPercent == 0 || first.PIDS != 7 {
		t.Errorf("首拍字段映射不符: %+v", first)
	}

	s.mu.Lock()
	s.prev = next
	s.prevAt = now
	s.mu.Unlock()

	// Δcpu=400, Δsys=1000, 2 cpus → 80%
	next2 := map[string]cpuSample{}
	second := s.statFor(sum, mkStats(1400, 101000, 2, 60<<20, 1<<30), now.Add(time.Second), next2)
	if want := (400.0 / 1000.0) * 2 * 100; second.CPUPercent != want {
		t.Errorf("次拍 CPU%% = %v, want %v", second.CPUPercent, want)
	}
}

// TestEngineStatsSource_CounterRollback 计数回卷（容器重启）不应算出负/爆值。
func TestEngineStatsSource_CounterRollback(t *testing.T) {
	s := newEngineStatsSource("/nonexistent.sock")
	sum := dockercli.ContainerSummary{ID: "abc", Names: []string{"/web"}}
	now := time.Now()
	next := map[string]cpuSample{}
	_ = s.statFor(sum, mkStats(5000, 100000, 1, 1, 0), now, next)
	s.mu.Lock()
	s.prev = next
	s.mu.Unlock()
	again := s.statFor(sum, mkStats(300, 50000, 1, 1, 0), now.Add(time.Second), map[string]cpuSample{})
	if again.CPUPercent != 0 {
		t.Errorf("回卷拍应 0，得 %v", again.CPUPercent)
	}
}

// 接口满足性：编译期钉住 engineStatsSource 实现 collector.ContainerStatsSource。
var _ interface {
	ContainerStats() ([]protocol.ContainerStat, error)
} = (*engineStatsSource)(nil)
