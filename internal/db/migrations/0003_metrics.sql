-- 0003_metrics.sql —— S2.6 指标入库补齐
-- 0001 已有 host 级 metrics_snapshots（extra 列保留给「不做结构化聚合」的杂项），
-- 本迁移补两块：
--   1) container_snapshots：每容器 cpu/mem 历史序列。独立成表而非塞 extra JSON，
--      是因为 S7 曲线要按容器 ID/名字过滤查询，结构化列才能走索引。
--      mem_limit 同步存进每行：容器重建后 limit 可能变化，历史行各自定格当时的值，
--      避免「用今天的 limit 回算昨天的百分比」这种口径漂移。
--   2) metrics_latest：每台服务器一行的实时快照（轮询器只更新、UI 首屏只读），
--      30s 采样 + 7 天滚动的场景下，最新值查询不该扫历史表。

CREATE TABLE container_snapshots (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    ts        TEXT    NOT NULL,
    cid       TEXT    NOT NULL, -- 容器完整 ID
    name      TEXT    NOT NULL, -- 无前导斜杠，与 docker ps 一致
    cpu_pct   REAL,
    mem_bytes INTEGER,
    mem_limit INTEGER,
    mem_pct   REAL,
    net_rx    INTEGER,
    net_tx    INTEGER,
    pids      INTEGER,
    state     TEXT
);
CREATE INDEX idx_container_snap_server_ts ON container_snapshots(server_id, ts);
CREATE INDEX idx_container_snap_cid_ts    ON container_snapshots(server_id, cid, ts);

CREATE TABLE metrics_latest (
    server_id INTEGER PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
    ts        TEXT NOT NULL,
    metrics   TEXT NOT NULL, -- protocol.HostMetrics JSON 原文；结构演进靠版本字段兜底
    containers TEXT    -- protocol.ContainerStat JSON 数组；源缺席时为 NULL
);
