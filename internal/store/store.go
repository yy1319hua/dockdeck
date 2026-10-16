// Package store 提供基于 *sql.DB 的领域访问方法，SQL 就近于此，路由层不碰语句。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
)

// ErrNotFound 表示目标不存在；调用方据此返回 404。
var ErrNotFound = errors.New("store: 记录不存在")

// Store 包裹数据库句柄与主密钥。
type Store struct {
	DB  *sql.DB
	Key []byte // 32 字节主密钥，用于服务器 token 加解密
}

// ---- 用户 ----

type User struct {
	ID          int64
	Username    string
	PwHash      string
	Disabled    bool
	FailedCount int
	LockedUntil *time.Time
	LastLogin   *time.Time
}

func (s *Store) CreateUser(ctx context.Context, username, password string) (int64, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return 0, errors.New("store: 用户名不能为空")
	}
	if len(password) < 8 {
		return 0, errors.New("store: 口令至少 8 位")
	}
	hash, err := crypto.HashPassword(password, crypto.DefaultArgon)
	if err != nil {
		return 0, err
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO users(username,pw_hash) VALUES(?,?)`, username, hash)
	if err != nil {
		if isUniqueErr(err) {
			return 0, errors.New("store: 用户名已存在")
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetUserByName(ctx context.Context, username string) (*User, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id,username,pw_hash,disabled,failed_count,locked_until,last_login
		 FROM users WHERE username=?`, username)
	return scanUser(row)
}

func (s *Store) HasAnyUser(ctx context.Context) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&n)
	return n > 0, err
}

func (s *Store) UpdatePassword(ctx context.Context, userID int64, password string) error {
	if len(password) < 8 {
		return errors.New("store: 口令至少 8 位")
	}
	hash, err := crypto.HashPassword(password, crypto.DefaultArgon)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		`UPDATE users SET pw_hash=?, failed_count=0, locked_until=NULL WHERE id=?`, hash, userID)
	return err
}

// MarkLoginFailure 递增失败计数；达到阈值时原子地设置 locked_until。
// 返回锁定后的解锁时刻（若本次触发锁定），否则返回 nil。
func (s *Store) MarkLoginFailure(ctx context.Context, userID int64, maxFails int, lockDur time.Duration) (*time.Time, error) {
	now := time.Now().UTC()
	// 先自增，再依据自增后的值判断是否达到阈值。
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE users SET failed_count=failed_count+1 WHERE id=?`, userID); err != nil {
		return nil, err
	}
	var cnt int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT failed_count FROM users WHERE id=?`, userID).Scan(&cnt); err != nil {
		return nil, err
	}
	if maxFails > 0 && cnt >= maxFails {
		until := now.Add(lockDur)
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE users SET locked_until=? WHERE id=?`,
			until.Format(time.RFC3339), userID); err != nil {
			return nil, err
		}
		return &until, nil
	}
	return nil, nil
}

func (s *Store) RecordLoginSuccess(ctx context.Context, userID int64) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE users SET failed_count=0, locked_until=NULL, last_login=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), userID)
	return err
}

func scanUser(row *sql.Row) (*User, error) {
	var (
		u           User
		disabled    int
		lockedUntil sql.NullString
		lastLogin   sql.NullString
	)
	err := row.Scan(&u.ID, &u.Username, &u.PwHash, &disabled, &u.FailedCount, &lockedUntil, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	if lockedUntil.Valid {
		if t, e := time.Parse(time.RFC3339, lockedUntil.String); e == nil {
			tt := t
			u.LockedUntil = &tt
		}
	}
	if lastLogin.Valid {
		if t, e := time.Parse(time.RFC3339, lastLogin.String); e == nil {
			tt := t
			u.LastLogin = &tt
		}
	}
	return &u, nil
}

// ---- 会话 ----

type Session struct {
	ID          int64
	UserID      int64
	TokenHash   string
	ExpiresAt   time.Time
	AbsoluteExp time.Time
	IP          string
	UserAgent   string
}

// CreateSession 生成随机令牌，库中只存其 SHA-256；返回明文令牌（仅此一次可见）。
func (s *Store) CreateSession(ctx context.Context, userID int64, idleTTL, absTTL time.Duration, ip, ua string) (token string, sessID int64, err error) {
	token, err = crypto.RandomToken(32)
	if err != nil {
		return "", 0, err
	}
	now := time.Now().UTC()
	exp := now.Add(idleTTL)
	abs := now.Add(absTTL)
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO sessions(user_id,token_hash,expires_at,absolute_exp,ip,user_agent)
		 VALUES(?,?,?,?,?,?)`,
		userID, crypto.HashToken(token),
		exp.Format(time.RFC3339), abs.Format(time.RFC3339), ip, ua)
	if err != nil {
		return "", 0, err
	}
	id, _ := res.LastInsertId()
	return token, id, nil
}

// LookupSession 以明文令牌查有效会话（未过期、未撤销），并滑动更新 last_seen/expires_at。
func (s *Store) LookupSession(ctx context.Context, token string, idleTTL time.Duration) (*Session, error) {
	hash := crypto.HashToken(token)
	row := s.DB.QueryRowContext(ctx,
		`SELECT id,user_id,token_hash,expires_at,absolute_exp,ip,user_agent
		 FROM sessions
		 WHERE token_hash=? AND revoked_at IS NULL AND absolute_exp > ?`,
		hash, time.Now().UTC().Format(time.RFC3339))
	var (
		sess       Session
		expS, absS string
	)
	err := row.Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &expS, &absS, &sess.IP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.ExpiresAt, _ = time.Parse(time.RFC3339, expS)
	sess.AbsoluteExp, _ = time.Parse(time.RFC3339, absS)
	if time.Now().UTC().After(sess.ExpiresAt) {
		return nil, ErrNotFound // 空闲超时
	}
	newExp := time.Now().UTC().Add(idleTTL)
	if newExp.After(sess.AbsoluteExp) {
		newExp = sess.AbsoluteExp
	}
	_, _ = s.DB.ExecContext(ctx,
		`UPDATE sessions SET last_seen=?, expires_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), newExp.Format(time.RFC3339), sess.ID)
	return &sess, nil
}

func (s *Store) RevokeSession(ctx context.Context, token string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE sessions SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), crypto.HashToken(token))
	return err
}

// RevokeAllForUser 撤销某用户全部会话（改密/禁用时调用）。
func (s *Store) RevokeAllForUser(ctx context.Context, userID int64) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), userID)
	return err
}

// ---- 审计 ----

type AuditEntry struct {
	// ID/Ts 仅读路径（ListAuditLogs 的游标分页与展示）使用；写路径
	// 两者都留空——id 由自增列产生，ts 由表 DEFAULT 产生。
	ID        int64
	Ts        string
	UserID    int64
	Username  string
	ServerID  int64
	Action    string
	Target    string
	Detail    string
	Result    string // ok | error
	Error     string
	IP        string
	RequestID string
}

func (s *Store) WriteAudit(ctx context.Context, e AuditEntry) error {
	if e.Action == "" {
		return errors.New("store: 审计 action 不能为空")
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO audit_logs(user_id,username,server_id,action,target,detail,result,error,ip,request_id)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		nullInt64(e.UserID), e.Username, nullInt64(e.ServerID),
		e.Action, e.Target, e.Detail, e.Result, e.Error, e.IP, e.RequestID)
	return err
}

// ---- 审计查询（S7.7）----

// AuditFilter 审计查询过滤条件。零值字段=不限。
// From/To 是秒级 RFC3339 UTC 字符串：表里 ts 就是该格式的 TEXT，
// 字典序即时间序，直接比较（调用方负责把用户输入归一成同格式——
// 带毫秒或时区偏移的 RFC3339 与秒级 Z 后缀混比会错位）。
type AuditFilter struct {
	From     string
	To       string
	ServerID int64  // 0=不限
	Action   string // 前缀匹配（SQL LIKE 'x%'），如 "container" 命中 container.*
	Result   string // ok | error；空=不限
	BeforeID int64  // 游标：只取 id 严格更小的行（倒序翻页）
	Limit    int    // <=0 取默认 100
}

// ListAuditLogs 倒序（id 降序=最新在前）查审计行，第二个返回值表示是否
// 还有更多（此时调用方应把本页最后一条的 id 作为下游游标）。
//
// limit+1 探测法：多查一条判断有无更多，比 COUNT(*) 少一次全表扫描。
func (s *Store) ListAuditLogs(ctx context.Context, f AuditFilter) ([]AuditEntry, bool, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	where := []string{"1=1"}
	args := make([]any, 0, 8)
	if f.From != "" {
		where = append(where, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, "ts <= ?")
		args = append(args, f.To)
	}
	if f.ServerID > 0 {
		where = append(where, "server_id = ?")
		args = append(args, f.ServerID)
	}
	if f.Action != "" {
		where = append(where, "action LIKE ?")
		args = append(args, f.Action+"%")
	}
	if f.Result != "" {
		where = append(where, "result = ?")
		args = append(args, f.Result)
	}
	if f.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, f.BeforeID)
	}
	// user_id/server_id 可空（ON DELETE SET NULL）、TEXT 列可空——
	// COALESCE 成 0/空串，扫进 int64/string 不需要 sql.Null* 中转。
	q := fmt.Sprintf(
		`SELECT id, COALESCE(ts,''), COALESCE(user_id,0), COALESCE(username,''), COALESCE(server_id,0),
		        action, COALESCE(target,''), COALESCE(detail,''), result, COALESCE(error,''),
		        COALESCE(ip,''), COALESCE(request_id,'')
		   FROM audit_logs WHERE %s ORDER BY id DESC LIMIT ?`,
		strings.Join(where, " AND "))
	args = append(args, limit+1)

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Ts, &e.UserID, &e.Username, &e.ServerID,
			&e.Action, &e.Target, &e.Detail, &e.Result, &e.Error, &e.IP, &e.RequestID); err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// ---- 设置 ----

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339))
	return err
}

// ---- helpers ----

func nullInt64(v int64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint")
}

// IsUniqueErr 把 SQLite 的唯一约束冲突暴露给上层，让它们能回「名称已存在」
// 而不是笼统的内部错误。
func IsUniqueErr(err error) bool { return isUniqueErr(err) }
