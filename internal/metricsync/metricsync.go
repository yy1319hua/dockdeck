// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package metricsync 定时从各 Agent 拉取主机/容器指标并落库。
//
// 为什么单独成包而不是写在 cmd/hub：cmd/hub 是 main 包，逻辑放进来就测不了。
// 本包的测试用 internal/fakeagent 起真实 HTTP 链路 + 真实 sqlite，
// 不靠 mock 接口糊弄——否则「采集到底写没写进库」这类问题永远测不出来。
package metricsync

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/yy1319hua/dockdeck/internal/agentcli"
	"github.com/yy1319hua/dockdeck/internal/store"
)

const (
	// DefaultInterval 是默认采集周期。
	DefaultInterval = 30 * time.Second
	// DefaultRetention 与既定的备份轮转口径一致：指标历史保留 14 天。
	DefaultRetention = 14 * 24 * time.Hour
	// perServerTimeout 单台采集超时：一台机器卡住不能拖垮整轮。
	perServerTimeout = 10 * time.Second
	// pruneEvery 至少间隔多久清一次历史。轮次里每次都删一遍没有意义。
	pruneEvery = time.Hour
)

// Poller 是 Hub 侧的指标轮询器。零值不可用，请用 New 构造。
type Poller struct {
	st        *store.Store
	interval  time.Duration
	retention time.Duration
	logf      func(format string, args ...any)
	lastPrune time.Time
}

// New 构造轮询器。logf 为 nil 时落到标准 log（Hub 可换成自己的结构化日志）。
// interval <= 0 用 DefaultInterval；retention <= 0 用 DefaultRetention。
func New(st *store.Store, interval, retention time.Duration, logf func(string, ...any)) *Poller {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	if logf == nil {
		logf = log.Printf
	}
	return &Poller{st: st, interval: interval, retention: retention, logf: logf}
}

// PollOnce 采集一轮，返回成功台数。
//
// 单台失败只记录日志并继续下一台：一台机器离线不该让其余机器的数据也断掉。
func (p *Poller) PollOnce(ctx context.Context) int {
	servers, err := p.st.ListServers(ctx)
	if err != nil {
		p.logf("metricsync: 读取服务器列表失败: %v", err)
		return 0
	}

	ok := 0
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if err := p.pollServer(ctx, s.ID, s.Name); err != nil {
			p.logf("metricsync: 服务器 %d(%s) 采集失败: %v", s.ID, s.Name, err)
			continue
		}
		ok++
	}
	return ok
}

// Run 定时轮询直到 ctx 取消。启动后立刻采一轮——否则要空等一个周期才有首个数据点。
func (p *Poller) Run(ctx context.Context) {
	p.round(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logf("metricsync: 已停止")
			return
		case <-ticker.C:
			p.round(ctx)
		}
	}
}

// round 是「采一轮 + 视情况清历史」。
func (p *Poller) round(ctx context.Context) {
	p.PollOnce(ctx)

	if time.Since(p.lastPrune) < pruneEvery {
		return
	}
	host, ctr, err := p.st.PruneMetrics(ctx, p.retention, time.Now().UTC())
	if err != nil {
		p.logf("metricsync: 清理历史失败: %v", err)
	} else if host+ctr > 0 {
		p.logf("metricsync: 清理过期指标 host=%d container=%d 行", host, ctr)
	}
	p.lastPrune = time.Now()
}

// pollServer 采集单台：拉主机指标 + 容器指标，写入实时快照与历史点。
func (p *Poller) pollServer(ctx context.Context, id int64, name string) error {
	conn, err := p.st.ServerConn(ctx, id)
	if err != nil {
		// 含 ErrServerDisabled（已禁用）与令牌解密失败，两者都不该静默跳过后续处理，
		// 但调用方只需知道「这台没采到」。
		return err
	}

	ctx2, cancel := context.WithTimeout(ctx, perServerTimeout)
	defer cancel()

	cli := agentcli.New(conn.BaseURL, conn.Token, nil)

	m, err := cli.Metrics(ctx2)
	if err != nil {
		return fmt.Errorf("拉取主机指标: %w", err)
	}

	// 容器指标拿不到不算致命：Agent 可能没跑 Docker。
	// 此时保留主机指标、容器留空（nil 编码为 null，前端据此区分「无容器」与「空列表」）。
	containers, cerr := cli.ContainerStats(ctx2)
	if cerr != nil {
		p.logf("metricsync: 服务器 %d(%s) 容器指标不可用，按无容器处理: %v", id, name, cerr)
		containers = nil
	}

	ts := time.Now().UTC()
	if err := p.st.UpsertMetricsLatest(ctx, id, m, containers, ts); err != nil {
		return fmt.Errorf("写实时快照: %w", err)
	}
	if err := p.st.SaveMetricsSnapshot(ctx, id, m, containers, ts); err != nil {
		return fmt.Errorf("写历史点: %w", err)
	}
	return nil
}
