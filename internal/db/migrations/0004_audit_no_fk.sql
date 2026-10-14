-- 0004_audit_no_fk.sql —— audit_logs 去掉外键（S3.6 审计补全覆盖时暴露）
--
-- 症结：audit_logs.server_id 原本 REFERENCES servers(id)。「打向不存在
-- 服务器的请求」——探测、扫段、手滑打错 ID，Handler 照常 404——在
-- WriteAudit 时被外键拒收，而中间件只 _ = 忽略错误，行被静默丢掉。
-- 「失败也留痕」的口径恰恰在这类行上破功，而它们本是最该留的。
--
-- 判断：审计表是历史流水，不是活数据。server_id 记录「请求打向哪」，
-- user_id 记录「谁打的」——目标实体可以从未存在（探测），也可以后来
-- 被删（ON DELETE SET NULL 会静默改写历史）。审计行的存在不该依赖
-- 活表里有对应行。
--
-- user_id 的外键一并去掉：登录失败的自审计行 uid=0，今天靠
-- nullInt64(0)→NULL 兜底才没撞外键——兜底不该是唯一防线，哪天
-- 主体解析改回 0 语义或删用户功能上线，同样的静默丢行会重演。
-- 对照：metrics_latest / container_snapshots 的外键保留——那些是活
-- 数据，引用了不存在的服务器本身就是 bug，该在写入时就炸。
--
-- 重建走 SQLite 表重建（本迁移只删约束、不造违约数据，foreign_keys=ON
-- 下可安全执行）：建新表 → 带 id 拷贝 → 删旧表 → 改名 → 重建四个索引。
-- AUTOINCREMENT 经 sqlite_sequence 延续（有回归测试盯着）。

CREATE TABLE audit_logs_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    user_id    INTEGER,
    username   TEXT,
    server_id  INTEGER,
    action     TEXT    NOT NULL,
    target     TEXT,
    detail     TEXT,
    result     TEXT    NOT NULL,
    error      TEXT,
    ip         TEXT,
    request_id TEXT
);

INSERT INTO audit_logs_new(id, ts, user_id, username, server_id, action, target, detail, result, error, ip, request_id)
    SELECT id, ts, user_id, username, server_id, action, target, detail, result, error, ip, request_id
      FROM audit_logs;

DROP TABLE audit_logs;
ALTER TABLE audit_logs_new RENAME TO audit_logs;

CREATE INDEX idx_audit_ts     ON audit_logs(ts);
CREATE INDEX idx_audit_user   ON audit_logs(user_id);
CREATE INDEX idx_audit_server ON audit_logs(server_id);
CREATE INDEX idx_audit_action ON audit_logs(action);
