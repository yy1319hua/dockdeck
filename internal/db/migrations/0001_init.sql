-- 0001_init.sql —— DockDeck 基础表
-- 说明：所有时间以 UTC RFC3339 文本存储（strftime 生成），避免时区歧义。
-- 口令仅存 argon2id 编码串；会话仅存 token 的 SHA-256；服务器 token 以主密钥 AES-GCM 密文存储。

PRAGMA foreign_keys = ON;

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    pw_hash       TEXT    NOT NULL,
    disabled      INTEGER NOT NULL DEFAULT 0,
    failed_count  INTEGER NOT NULL DEFAULT 0,
    locked_until  TEXT,
    last_login    TEXT,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   TEXT    NOT NULL UNIQUE,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    last_seen    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    expires_at   TEXT    NOT NULL,
    absolute_exp TEXT    NOT NULL,
    ip           TEXT,
    user_agent   TEXT,
    revoked_at   TEXT
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_exp  ON sessions(expires_at);

CREATE TABLE servers (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL UNIQUE,
    base_url      TEXT    NOT NULL,
    token_cipher  TEXT    NOT NULL,
    arch          TEXT,
    docker_ver    TEXT,
    enabled       INTEGER NOT NULL DEFAULT 1,
    last_seen     TEXT,
    last_error    TEXT,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE audit_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    username   TEXT,
    server_id  INTEGER REFERENCES servers(id) ON DELETE SET NULL,
    action     TEXT    NOT NULL,
    target     TEXT,
    detail     TEXT,
    result     TEXT    NOT NULL,
    error      TEXT,
    ip         TEXT,
    request_id TEXT
);
CREATE INDEX idx_audit_ts     ON audit_logs(ts);
CREATE INDEX idx_audit_user   ON audit_logs(user_id);
CREATE INDEX idx_audit_server ON audit_logs(server_id);
CREATE INDEX idx_audit_action ON audit_logs(action);

CREATE TABLE metrics_snapshots (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    ts        TEXT    NOT NULL,
    cpu_pct   REAL,
    mem_bytes INTEGER,
    mem_total INTEGER,
    disk_bytes INTEGER,
    disk_total INTEGER,
    net_rx    INTEGER,
    net_tx    INTEGER,
    load1     REAL,
    extra     TEXT
);
CREATE INDEX idx_metrics_server_ts ON metrics_snapshots(server_id, ts);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
