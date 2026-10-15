// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package metricsync

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/fakeagent"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// 测试走真链路：fakeagent 起真实 TLS + 真实验签，store 是真的 sqlite。
// 不 mock agentcli——「采集到底写没写进库」正是本包要保证的事，mock 掉就测不出来了。

func newStore(t *testing.T) *store.Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return &store.Store{DB: d, Key: bytes.Repeat([]byte("K"), 32)}
}

// seedServer 起一个假 Agent 并在 servers 表种一行，返回 id 与假 Agent。
func seedServer(t *testing.T, st *store.Store, name string, enabled bool) (int64, *fakeagent.Fake) {
	t.Helper()
	token := "agent-sekret"
	fa := fakeagent.New(token)
	as := fa.Server()
	t.Cleanup(as.Close)

	cipher, err := crypto.SealSecret(st.Key, []byte(token))
	if err != nil {
		t.Fatalf("封存令牌: %v", err)
	}
	on := 0
	if enabled {
		on = 1
	}
	res, err := st.DB.ExecContext(context.Background(),
		`INSERT INTO servers(name, base_url, token_cipher, enabled) VALUES(?,?,?,?)`,
		name, as.URL, cipher, on)
	if err != nil {
		t.Fatalf("种服务器行: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取 id: %v", err)
	}
	return id, fa
}

func sampleMetrics() *protocol.HostMetrics {
	return &protocol.HostMetrics{
		CollectedAt: time.Now().UTC(),
		CPU:         protocol.CPUStat{Cores: 4, UsagePercent: 42.5},
		Memory:      protocol.MemoryStat{Total: 8 << 30, Used: 3 << 30, Available: 5 << 30},
		Load:        protocol.LoadStat{One: 0.75, Five: 0.6, Fifteen: 0.5},
		Processes:   123,
	}
}

func TestPollOnceWritesLatestAndSnapshot(t *testing.T) {
	st := newStore(t)
	id, fa := seedServer(t, st, "node1", true)
	fa.SetMetrics(sampleMetrics())

	p := New(st, time.Second, 24*time.Hour, t.Logf)
	if n := p.PollOnce(context.Background()); n != 1 {
		t.Fatalf("应采到 1 台，实际 %d", n)
	}

	m, _, ts, err := st.LatestMetrics(context.Background(), id)
	if err != nil {
		t.Fatalf("读实时快照: %v（说明轮询器根本没写库）", err)
	}
	if m == nil || m.CPU.UsagePercent != 42.5 {
		t.Fatalf("实时快照内容不对: %+v", m)
	}
	if ts.IsZero() {
		t.Fatal("快照时间不应为零值")
	}

	// 历史曲线也要有：points 为空的话 S7 的图表就没有数据源。
	pts, err := st.MetricsHostRange(context.Background(), id, time.Now().Add(-time.Hour), time.Now().Add(time.Second), 10)
	if err != nil {
		t.Fatalf("读历史: %v", err)
	}
	if len(pts) == 0 {
		t.Fatal("轮询应同时写入历史点，实际为空")
	}
}

func TestPollOnceSkipsDisabledServer(t *testing.T) {
	st := newStore(t)
	id, fa := seedServer(t, st, "offline", false)
	fa.SetMetrics(sampleMetrics())

	p := New(st, time.Second, 24*time.Hour, t.Logf)
	if n := p.PollOnce(context.Background()); n != 0 {
		t.Fatalf("禁用的机器不该被采集，成功台数应为 0，实际 %d", n)
	}

	if _, _, _, err := st.LatestMetrics(context.Background(), id); err == nil {
		t.Fatal("禁用的机器不应留下指标快照")
	}
}

// 一台不可达不应连累其余机器——否则一次网络抖动就会让所有图表断线。
func TestPollOnceIsolatesFailures(t *testing.T) {
	st := newStore(t)
	good, fa := seedServer(t, st, "good", true)
	fa.SetMetrics(sampleMetrics())

	// 坏的一台：地址指向没人监听的端口。
	bad, _ := seedServer(t, st, "bad", true)
	if _, err := st.DB.ExecContext(context.Background(),
		`UPDATE servers SET base_url=? WHERE id=?`, "https://127.0.0.1:1", bad); err != nil {
		t.Fatalf("改坏地址: %v", err)
	}

	p := New(st, 50*time.Millisecond, 24*time.Hour, t.Logf)
	if n := p.PollOnce(context.Background()); n != 1 {
		t.Fatalf("只有好的那台该成功，实际 %d", n)
	}

	if _, _, _, err := st.LatestMetrics(context.Background(), good); err != nil {
		t.Fatalf("好的那台应照常入库: %v", err)
	}
	if _, _, _, err := st.LatestMetrics(context.Background(), bad); err == nil {
		t.Fatal("坏的那台不该有快照")
	}
}

// Run 必须能被 ctx 取消收住——否则 Hub 关掉了采集 goroutine 还在跑。
func TestRunStopsOnContextCancel(t *testing.T) {
	st := newStore(t)
	_, fa := seedServer(t, st, "node1", true)
	fa.SetMetrics(sampleMetrics())

	p := New(st, 20*time.Millisecond, 24*time.Hour, t.Logf)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	// 先确认它确实在跑（否则下面的「已停止」可能是根本没启动）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, _, err := st.LatestMetrics(context.Background(), 1); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run 启动后一直没采到数据")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 仍未退出")
	}
}

// 在线状态以 servers.last_seen 为准，前端据此算在线率——
// 这两个用例守的是「字段真的被写」，而不是「代码看起来会写」。
func serverState(t *testing.T, st *store.Store, id int64) (seen string, lastErr string) {
	t.Helper()
	var s, e sql.NullString
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT last_seen, last_error FROM servers WHERE id=?`, id).Scan(&s, &e); err != nil {
		t.Fatalf("读服务器状态: %v", err)
	}
	return s.String, e.String
}

func TestPollRecordsLastSeenOnSuccess(t *testing.T) {
	st := newStore(t)
	id, fa := seedServer(t, st, "node1", true)
	fa.SetMetrics(sampleMetrics())

	p := New(st, time.Second, 24*time.Hour, t.Logf)
	p.PollOnce(context.Background())

	seen, lastErr := serverState(t, st, id)
	if seen == "" {
		t.Fatal("采集成功后 last_seen 必须有值——否则前端会把它判成离线")
	}
	if lastErr != "" {
		t.Fatalf("采集成功后 last_error 应被清空，实际: %q", lastErr)
	}
}

func TestPollRecordsLastErrorOnFailure(t *testing.T) {
	st := newStore(t)
	id, _ := seedServer(t, st, "bad", true)
	if _, err := st.DB.ExecContext(context.Background(),
		`UPDATE servers SET base_url=? WHERE id=?`, "https://127.0.0.1:1", id); err != nil {
		t.Fatalf("改坏地址: %v", err)
	}

	p := New(st, 50*time.Millisecond, 24*time.Hour, t.Logf)
	p.PollOnce(context.Background())

	seen, lastErr := serverState(t, st, id)
	if seen == "" {
		t.Fatal("失败也要更新 last_seen：在线判定看的是最后一次通讯，不是最后一次成功")
	}
	if lastErr == "" {
		t.Fatal("采集失败应把原因写进 last_error，否则排障无从下手")
	}
}
