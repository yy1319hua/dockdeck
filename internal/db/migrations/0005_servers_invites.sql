-- 0005_servers_invites.sql —— Agent 纳管邀请表（GAP-1）
-- 语义：管理员经 POST /api/v1/servers/invite 签发一枚一次性 join_secret（库中只存其 SHA-256）；
-- Agent 容器启动时以 Authorization: Bearer <join_secret> 调 /api/v1/servers/enroll 认领，
-- 认领即写入 servers 行，并以**同一** secret 作为该 Agent 的长期共享令牌（token_cipher 密文存储）。
-- 幂等：已认领的 secret 再次 enroll 返回既有服务器 id 并刷新 base_url，故 Agent 重启安全。

CREATE TABLE server_invites (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    join_hash   TEXT    NOT NULL UNIQUE,      -- join_secret 的 SHA-256；明文不落库
    name_hint   TEXT,                          -- 签发时备注，可空
    expires_at  TEXT    NOT NULL,              -- RFC3339 UTC；未认领过期即失效
    claimed_at  TEXT,                          -- 认领时刻；非空表示已消费
    server_id   INTEGER REFERENCES servers(id) ON DELETE SET NULL,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX idx_invites_expiry ON server_invites(expires_at);
