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
	for _, d := range disks {
		if d.Mount == "/" {
			u, t := int64(d.Used), int64(d.Total)
			return &u, &t
		}
	}
	// 没有「/」：退回第一个挂载点（Agent 配置了自定义 mounts 的场景）。
	if len(disks) > 0 {
		u, t := int64(disks[0].Used), int64(disks[0].Total)
		return &u, &t
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

// ---- S2.7：监控查询 API 的数据访问 ----

// MaxOverviewPoints 限定 overview 单次返回的点数上限。
// 历史表按 30s 采样滚动 7 天，全量约 2 万点，一次吐给前端既浪费也卡渲染；
// 调用方需要更细就分页拉。上限常量写死而非配置——这是口径不是参数。
const MaxOverviewPoints = 1440

// HostPoint 是 host 历史曲线上的一个点，列与 metrics_snapshots 一一对应。
type HostPoint struct {
	Ts        time.Time `json:"ts"`
	CPUPct    float64   `json:"cpu_pct"`
	Mem       int64     `json:"mem_bytes"`
	MemTotal  int64     `json:"mem_total"`
	Disk      int64     `json:"disk_bytes"`
	DiskTotal int64     `json:"disk_total"`
	NetRx     int64     `json:"net_rx"`
	NetTx     int64     `json:"net_tx"`
	Load1     float64   `json:"load1"`
}

// ContainerPoint 是每容器历史曲线上的一个点。
type ContainerPoint struct {
	Ts       time.Time `json:"ts"`
	CID      string    `json:"cid"`
	Name     string    `json:"name"`
	CPUPct   float64   `json:"cpu_pct"`
	Mem      int64     `json:"mem_bytes"`
	MemLimit int64     `json:"mem_limit"`
	MemPct   float64   `json:"mem_pct"`
	NetRx    int64     `json:"net_rx"`
	NetTx    int64     `json:"net_tx"`
	PIDs     int64     `json:"pids"`
}

// ServerExists 判断服务器是否存在。禁用的服务器也返回 true——
// 它的指标只是不再更新，已入库的历史仍应可查。
func (s *Store) ServerExists(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, nil
	}
	var one int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM servers WHERE id=?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// MetricsHostRange 返回 [from,to] 内 host 历史点，升序，最多 limit 点（截最新的）。
// limit<=0 或超过上限时按 MaxOverviewPoints 收敛，防止一次查询拖死 SQLite。
func (s *Store) MetricsHostRange(ctx context.Context, serverID int64, from, to time.Time, limit int) ([]HostPoint, error) {
	if limit <= 0 || limit > MaxOverviewPoints {
		limit = MaxOverviewPoints
	}
	// 子查询取最新 N 点再升序排回：曲线要的是时间正向序列。
	rows, err := s.DB.QueryContext(ctx,
		`SELECT ts,cpu_pct,mem_bytes,mem_total,disk_bytes,disk_total,net_rx,net_tx,load1 FROM (
		   SELECT ts,cpu_pct,mem_bytes,mem_total,disk_bytes,disk_total,net_rx,net_tx,load1
		   FROM metrics_snapshots WHERE server_id=? AND ts>=? AND ts<=?
		   ORDER BY ts DESC LIMIT ?
		 ) ORDER BY ts ASC`,
		serverID, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostPoint
	for rows.Next() {
		var tsStr string
		var p HostPoint
		var disk, diskTotal sql.NullInt64
		if err := rows.Scan(&tsStr, &p.CPUPct, &p.Mem, &p.MemTotal, &disk, &diskTotal, &p.NetRx, &p.NetTx, &p.Load1); err != nil {
			return nil, err
		}
		if p.Ts, err = time.Parse(time.RFC3339, tsStr); err != nil {
			return nil, fmt.Errorf("store: ts 解析失败: %w", err)
		}
		// 可空列落 NULL 时以 0 入点：曲线断点靠相邻 ts 间隔判断，0 值不会伪装成有效数据。
		p.Disk, p.DiskTotal = disk.Int64, diskTotal.Int64
		out = append(out, p)
	}
	return out, rows.Err()
}

// MetricsContainerRange 返回 [from,to] 内全部容器的历史点，升序。
// limit 作用于行数（不按容器均分）：上限兜底是目的，公平分配是曲线层的职责。
func (s *Store) MetricsContainerRange(ctx context.Context, serverID int64, from, to time.Time, limit int) ([]ContainerPoint, error) {
	if limit <= 0 || limit > MaxOverviewPoints {
		limit = MaxOverviewPoints
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT ts,cid,name,cpu_pct,mem_bytes,mem_limit,mem_pct,net_rx,net_tx,pids FROM (
		   SELECT ts,cid,name,cpu_pct,mem_bytes,mem_limit,mem_pct,net_rx,net_tx,pids
		   FROM container_snapshots WHERE server_id=? AND ts>=? AND ts<=?
		   ORDER BY ts DESC LIMIT ?
		 ) ORDER BY ts ASC, cid ASC`,
		serverID, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContainerPoint
	for rows.Next() {
		var tsStr string
		var p ContainerPoint
		if err := rows.Scan(&tsStr, &p.CID, &p.Name, &p.CPUPct, &p.Mem, &p.MemLimit, &p.MemPct, &p.NetRx, &p.NetTx, &p.PIDs); err != nil {
			return nil, err
		}
		if p.Ts, err = time.Parse(time.RFC3339, tsStr); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
