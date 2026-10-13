// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 本文件承载 S2.6 的指标入库：host 历史行、每容器历史行、实时快照、滚动保留。
// SQL 就近原则：全部语句写在这里，路由层与轮询器不碰 SQL。

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// DefaultMetricsRetention 是滚动保留默认窗口（架构文档：滚动 7 天）。
const DefaultMetricsRetention = 7 * 24 * time.Hour

// MetricsSnapshotExtra 是 host 历史行 extra 列的结构化负载。
// 只放没有对应列、但排查时需要的低频字段；别把主曲线字段挪进来。
type MetricsSnapshotExtra struct {
	Processes int    `json:"processes"`
	UptimeSec uint64 `json:"uptime_sec"`
}

// rootDisk 取根挂载点的磁盘用量作为 host 级 disk_bytes/disk_total 的口径。
// metrics_snapshots 只有单列容量，多挂载点机器取「/」最贴近用户直觉；
// 没有 / 采集项时退回第一个挂载点（Agent 配置了自定义 mounts 的场景），
// 两者都没有则返回 nil，让列落 NULL——宁缺勿编。
func rootDisk(disks []protocol.DiskStat) (used, total *int64) {
	for i, d := range disks {
		if d.Mount == "/" || i == 0 {
			u, t := int64(d.Used), int64(d.Total)
			return &u, &t
		}
	}
	return nil, nil
}

// sumNet 汇总全部网卡的累计收发字节。速率由 Hub 按两次快照的时间差计算，
// 这里只存累计值，与 Agent 侧口径一致（见 protocol.NetStat 注释）。
func sumNet(nets []protocol.NetStat) (rx, tx int64) {
	for _, n := range nets {
		rx += int64(n.RxBytes)
		tx += int64(n.TxBytes)
	}
	return rx, tx
}

// SaveMetricsSnapshot 把一次完整快照写入历史表：host 一行 + 每容器一行。
// 整体一个事务——半份快照（有 host 没容器）会让曲线出现假掉零。
// ts 用传入时刻（轮询器的采样基准），统一 UTC RFC3339，与 0001 口径一致。
func (s *Store) SaveMetricsSnapshot(ctx context.Context, serverID int64, m *protocol.HostMetrics, containers []protocol.ContainerStat, ts time.Time) error {
	if serverID <= 0 {
		return errors.New("store: serverID 非法")
	}
	if m == nil {
		return errors.New("store: HostMetrics 不能为空")
	}
	tsStr := ts.UTC().Format(time.RFC3339)
	extra, err := json.Marshal(MetricsSnapshotExtra{Processes: m.Processes, UptimeSec: m.UptimeSec})
	if err != nil {
		return fmt.Errorf("store: extra 编码失败: %w", err)
	}
	diskUsed, diskTotal := rootDisk(m.Disks)
	rx, tx := sumNet(m.Network)

	txDB, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer txDB.Rollback()

	if _, err := txDB.ExecContext(ctx,
		`INSERT INTO metrics_snapshots(server_id,ts,cpu_pct,mem_bytes,mem_total,disk_bytes,disk_total,net_rx,net_tx,load1,extra)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		serverID, tsStr, m.CPU.UsagePercent, int64(m.Memory.Used), int64(m.Memory.Total),
		diskUsed, diskTotal, rx, tx, m.Load.One, string(extra)); err != nil {
		return fmt.Errorf("store: 写 host 快照失败: %w", err)
	}
	for _, c := range containers {
		if _, err := txDB.ExecContext(ctx,
			`INSERT INTO container_snapshots(server_id,ts,cid,name,cpu_pct,mem_bytes,mem_limit,mem_pct,net_rx,net_tx,pids)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			serverID, tsStr, c.ID, c.Name, c.CPUPercent, int64(c.MemUsage), int64(c.MemLimit),
			c.MemPercent, int64(c.NetRx), int64(c.NetTx), int64(c.PIDS)); err != nil {
			return fmt.Errorf("store: 写容器快照失败: %w", err)
		}
	}
	return txDB.Commit()
}

// UpsertMetricsLatest 覆盖写实时快照（每台服务器一行）。
// containers 为 nil 时容器列存 NULL——「源缺席」与「空列表」在下一次覆盖前都成立为 NULL，
// 读取方据此知道这台机器暂无容器指标，而不是把 NULL 当零值画图。
func (s *Store) UpsertMetricsLatest(ctx context.Context, serverID int64, m *protocol.HostMetrics, containers []protocol.ContainerStat, ts time.Time) error {
	if m == nil {
		return errors.New("store: HostMetrics 不能为空")
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: metrics 编码失败: %w", err)
	}
	var cb interface{}
	if containers != nil {
		b, err := json.Marshal(containers)
		if err != nil {
			return fmt.Errorf("store: containers 编码失败: %w", err)
		}
		cb = string(b)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO metrics_latest(server_id,ts,metrics,containers) VALUES(?,?,?,?)
		 ON CONFLICT(server_id) DO UPDATE SET ts=excluded.ts, metrics=excluded.metrics, containers=excluded.containers`,
		serverID, ts.UTC().Format(time.RFC3339), string(mb), cb)
	return err
}

// LatestMetrics 读取实时快照。返回值 containers 为 nil 表示该机暂无容器指标。
func (s *Store) LatestMetrics(ctx context.Context, serverID int64) (m *protocol.HostMetrics, containers []protocol.ContainerStat, ts time.Time, err error) {
	var tsStr, mb string
	var cb sql.NullString
	row := s.DB.QueryRowContext(ctx,
		`SELECT ts,metrics,containers FROM metrics_latest WHERE server_id=?`, serverID)
	if err = row.Scan(&tsStr, &mb, &cb); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, time.Time{}, ErrNotFound
		}
		return nil, nil, time.Time{}, err
	}
	m = &protocol.HostMetrics{}
	if err = json.Unmarshal([]byte(mb), m); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("store: metrics 解码失败: %w", err)
	}
	if cb.Valid {
		if err = json.Unmarshal([]byte(cb.String), &containers); err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("store: containers 解码失败: %w", err)
		}
	}
	ts, err = time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("store: ts 解析失败: %w", err)
	}
	return m, containers, ts, nil
}

// PruneMetrics 删除早于保留窗口的历史行（两张表），返回各自删除的行数。
// 只清历史表，metrics_latest 是实时值永远保留。
func (s *Store) PruneMetrics(ctx context.Context, retention time.Duration, now time.Time) (hostRows, containerRows int64, err error) {
	if retention <= 0 {
		return 0, 0, errors.New("store: 保留窗口必须为正")
	}
	cutoff := now.UTC().Add(-retention).Format(time.RFC3339)
	for _, q := range []struct {
		table string
	}{
		{"metrics_snapshots"},
		{"container_snapshots"},
	} {
		res, err := s.DB.ExecContext(ctx,
			`DELETE FROM `+q.table+` WHERE ts < ?`, cutoff)
		if err != nil {
			return 0, 0, fmt.Errorf("store: 清理 %s 失败: %w", q.table, err)
		}
		n, _ := res.RowsAffected()
		if q.table == "metrics_snapshots" {
			hostRows = n
		} else {
			containerRows = n
		}
	}
	return hostRows, containerRows, nil
}
