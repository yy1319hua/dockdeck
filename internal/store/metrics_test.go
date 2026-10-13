// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// newMetricsStore 建一个临时库并塞进一台服务器（满足外键），返回 Store 与其 ID。
func newMetricsStore(t *testing.T) (*Store, int64) {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	s := &Store{DB: d, Key: make([]byte, 32)}
	res, err := d.Exec(`INSERT INTO servers(name,base_url,token_cipher) VALUES("srv1","https://t:9527","x")`)
	if err != nil {
		t.Fatalf("insert server: %v", err)
	}
	id, _ := res.LastInsertId()
	return s, id
}

// sampleHost 构造一次可断言的快照：根盘取「/」、网络两张卡求和。
func sampleHost() *protocol.HostMetrics {
	return &protocol.HostMetrics{
		CollectedAt: time.Unix(1700000000, 0).UTC(),
		CPU:         protocol.CPUStat{Cores: 2, UsagePercent: 12.5},
		Memory:      protocol.MemoryStat{Total: 4096, Used: 1024},
		Disks: []protocol.DiskStat{
			{Mount: "/boot", Total: 100, Used: 10},
			{Mount: "/", Total: 8000, Used: 4000},
		},
		Network: []protocol.NetStat{
			{Interface: "lo", RxBytes: 5, TxBytes: 5},
			{Interface: "eth0", RxBytes: 95, TxBytes: 195},
		},
		Load:      protocol.LoadStat{One: 0.25},
		Processes: 42,
		UptimeSec: 100000,
	}
}

func sampleContainers() []protocol.ContainerStat {
	return []protocol.ContainerStat{
		{ID: "c1", Name: "web", CPUPercent: 3.5, MemUsage: 100, MemLimit: 500, MemPercent: 20, NetRx: 7, NetTx: 9, PIDS: 3},
		{ID: "c2", Name: "db", CPUPercent: 1, MemUsage: 200, MemLimit: 800, MemPercent: 25, NetRx: 8, NetTx: 10, PIDS: 5},
	}
}

// TestSaveAndLatest_Full 验证主链路：历史行落库、实时快照覆盖写、读回一致。
func TestSaveAndLatest_Full(t *testing.T) {
	s, id := newMetricsStore(t)
	ctx := context.Background()
	ts := time.Now()

	if err := s.SaveMetricsSnapshot(ctx, id, sampleHost(), sampleContainers(), ts); err != nil {
		t.Fatalf("SaveMetricsSnapshot: %v", err)
	}
	if err := s.UpsertMetricsLatest(ctx, id, sampleHost(), sampleContainers(), ts); err != nil {
		t.Fatalf("UpsertMetricsLatest: %v", err)
	}

	// host 历史行：根盘口径取「/」而不是首盘，网络是两卡累计和。
	var cpu float64
	var mem, memTotal, diskUsed, diskTotal, rx, tx int64
	var load float64
	var extra string
	err := s.DB.QueryRow(`SELECT cpu_pct,mem_bytes,mem_total,disk_bytes,disk_total,net_rx,net_tx,load1,extra
		FROM metrics_snapshots WHERE server_id=?`, id).
		Scan(&cpu, &mem, &memTotal, &diskUsed, &diskTotal, &rx, &tx, &load, &extra)
	if err != nil {
		t.Fatalf("查 host 行: %v", err)
	}
	if cpu != 12.5 || mem != 1024 || memTotal != 4096 || load != 0.25 {
		t.Errorf("host 值不符：cpu=%v mem=%v memTotal=%v load=%v", cpu, mem, memTotal, load)
	}
	if diskUsed != 4000 || diskTotal != 8000 {
		t.Errorf("磁盘口径应取「/」，got used=%d total=%d", diskUsed, diskTotal)
	}
	if rx != 100 || tx != 200 {
		t.Errorf("网络应为两卡求和，got rx=%d tx=%d", rx, tx)
	}
	if extra == "" {
		t.Error("extra 应携带 processes/uptime")
	}

	// 容器历史行：每个容器一行，limit 定格当时值。
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(1) FROM container_snapshots WHERE server_id=?`, id).Scan(&n); err != nil || n != 2 {
		t.Fatalf("容器行数 = %d err=%v，want 2", n, err)
	}

	// 实时快照读回。
	m, containers, gotTs, err := s.LatestMetrics(ctx, id)
	if err != nil {
		t.Fatalf("LatestMetrics: %v", err)
	}
	if m.CPU.UsagePercent != 12.5 || len(containers) != 2 || containers[1].Name != "db" {
		t.Errorf("实时快照读回不符：m=%+v containers=%+v", m, containers)
	}
	if gotTs.Unix() != ts.UTC().Unix() {
		t.Errorf("ts 应为写入时刻，got %v want %v", gotTs, ts)
	}
}

// TestUpsertLatest_Overwrite 验证实时快照按 server_id 覆盖而非追加。
func TestUpsertLatest_Overwrite(t *testing.T) {
	s, id := newMetricsStore(t)
	ctx := context.Background()
	h1, h2 := sampleHost(), sampleHost()
	h2.CPU.UsagePercent = 88.8
	for _, p := range []*protocol.HostMetrics{h1, h2} {
		if err := s.UpsertMetricsLatest(ctx, id, p, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	m, containers, _, err := s.LatestMetrics(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.CPU.UsagePercent != 88.8 {
		t.Errorf("应读到最新值，got %v", m.CPU.UsagePercent)
	}
	// containers=nil 落 NULL：源缺席≠零值，读回应为 nil 而不是空切片被误用。
	if containers != nil {
		t.Errorf("源缺席时 containers 应为 nil，got %+v", containers)
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(1) FROM metrics_latest`).Scan(&n)
	if n != 1 {
		t.Errorf("实时快照应只有一行，got %d", n)
	}
}

// TestLatestMetrics_NotFound 验证无记录返回 ErrNotFound，调用方据此返回 404。
func TestLatestMetrics_NotFound(t *testing.T) {
	s, id := newMetricsStore(t)
	if _, _, _, err := s.LatestMetrics(context.Background(), id+999); err != ErrNotFound {
		t.Fatalf("应返回 ErrNotFound，got %v", err)
	}
}

// TestPruneMetrics 验证滚动保留：只清过期历史行，实时快照不动。
func TestPruneMetrics(t *testing.T) {
	s, id := newMetricsStore(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	fresh := now.Add(-time.Hour)

	if err := s.SaveMetricsSnapshot(ctx, id, sampleHost(), sampleContainers(), old); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMetricsSnapshot(ctx, id, sampleHost(), nil, fresh); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMetricsLatest(ctx, id, sampleHost(), nil, now); err != nil {
		t.Fatal(err)
	}

	hostRows, containerRows, err := s.PruneMetrics(ctx, DefaultMetricsRetention, now)
	if err != nil {
		t.Fatalf("PruneMetrics: %v", err)
	}
	if hostRows != 1 || containerRows != 2 {
		t.Errorf("应删 1 行 host + 2 行容器，got %d/%d", hostRows, containerRows)
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(1) FROM metrics_snapshots`).Scan(&n)
	if n != 1 {
		t.Errorf("fresh 行应保留，剩 %d", n)
	}
	s.DB.QueryRow(`SELECT COUNT(1) FROM container_snapshots`).Scan(&n)
	if n != 0 {
		t.Errorf("过期容器行应清空，剩 %d", n)
	}
	if _, _, _, err := s.LatestMetrics(ctx, id); err != nil {
		t.Errorf("实时快照不应被清理: %v", err)
	}
}

// TestSaveMetricsSnapshot_RejectsNil 验证防御性输入检查。
func TestSaveMetricsSnapshot_RejectsNil(t *testing.T) {
	s, id := newMetricsStore(t)
	if err := s.SaveMetricsSnapshot(context.Background(), id, nil, nil, time.Now()); err == nil {
		t.Fatal("nil HostMetrics 应报错")
	}
	if _, _, _, err := s.LatestMetrics(context.Background(), 0); err == nil || err == sql.ErrNoRows {
		t.Fatalf("非法 serverID 不应命中 ErrNotFound 分支，got %v", err)
	}
}
