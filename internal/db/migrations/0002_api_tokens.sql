-- 0002_api_tokens.sql —— App / 脚本直连用的 API Token
-- 与 sessions 分属两套身份：cookie 会话走浏览器，API Token 走 Bearer，二者互不信任。
-- 库里只存 token 明文的 SHA-256（同 sessions），明文仅在签发那一次返回给调用方。

PRAGMA foreign_keys = ON;

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,           -- 人类可读标识，如 "ios-app"
    token_hash   TEXT    NOT NULL UNIQUE,           -- SHA-256(明文)，明文不落库
    scopes       TEXT    NOT NULL DEFAULT '',       -- 逗号分隔，如 "read:metrics,write:containers"
    created_by   INTEGER REFERENCES users(id) ON DELETE CASCADE,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    last_used_at TEXT,
    expires_at   TEXT,                              -- NULL = 永不过期（不推荐，UI 会提示）
    revoked_at   TEXT
);

CREATE INDEX idx_tokens_hash ON api_tokens(token_hash);
CREATE INDEX idx_tokens_creator ON api_tokens(created_by);
