// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package collector

import (
	"errors"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// TestNilStatsSource_Empty 覆盖默认源语义：空列表 + 无错误。
// Agent 在 docker 源缺席时靠它保持「有响应、无数据」而不是 5xx。
func TestNilStatsSource_Empty(t *testing.T) {
	stats, err := NilStatsSource{}.ContainerStats()
	if err != nil {
		t.Fatalf("NilSource 不应返回错误：%v", err)
	}
	if stats == nil || len(stats) != 0 {
		t.Errorf("应返回非 nil 空切片，got %#v", stats)
	}
}

// fakeSource 用于验证接口契约：实现即可被消费，错误可透传。
type fakeSource struct {
	stats []protocol.ContainerStat
	err   error
}

func (f fakeSource) ContainerStats() ([]protocol.ContainerStat, error) { return f.stats, f.err }

// TestContainerStatsSource_ErrorPassthrough 确认接口实现方返回的错误
// 能原样到达调用方——由调用方（Agent 端点）决定降级策略，接口层不吞。
func TestContainerStatsSource_ErrorPassthrough(t *testing.T) {
	want := errors.New("engine unreachable")
	_, err := fakeSource{err: want}.ContainerStats()
	if !errors.Is(err, want) {
		t.Fatalf("错误应透传，got %v", err)
	}
	stats, err := fakeSource{stats: []protocol.ContainerStat{{ID: "abc", Name: "web"}}}.ContainerStats()
	if err != nil || len(stats) != 1 || stats[0].ID != "abc" {
		t.Fatalf("数据应透传，got %#v err=%v", stats, err)
	}
}
