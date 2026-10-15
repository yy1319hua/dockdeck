// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
)

// 纳管邀请相关错误，供 handler 映射 HTTP 状态码。
var (
	// ErrInviteNotFound 该 join_secret 从未被签发过。
	ErrInviteNotFound = errors.New("store: 纳管邀请不存在")
	// ErrInviteExpired 邀请未认领但已过期，或已认领但其服务器已不存在（皆不可再用）。
	ErrInviteExpired = errors.New("store: 纳管邀请已过期或不可用")
	// ErrServerNameTaken 同名主机已存在（servers.name UNIQUE）。
	ErrServerNameTaken = errors.New("store: 服务器名称已存在")
)

type invite struct {
	id        int64
	expiresAt time.Time
	claimedAt sql.NullString
	serverID  sql.NullInt64
}

func parseRFC3339(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// CreateInvite 生成一枚一次性 join_secret，库中只存其 SHA-256。
// 明文仅此一次由调用方返回给管理员，事后无从取回。
func (s *Store) CreateInvite(ctx context.Context, nameHint string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	plain, err := crypto.RandomToken(32)
	if err != nil {
		return "", err
	}
	exp := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO server_invites(join_hash, name_hint, expires_at) VALUES(?,?,?)`,
		crypto.HashToken(plain), strings.TrimSpace(nameHint), exp); err != nil {
		return "", err
	}
	return plain, nil
}

// KnownJoinSecret 判断某串是否曾是 Hub 签发过的 join_secret，供 AuthJoin 中间件快速识别。
// 只验「认识」，不判过期/认领——那由 EnrollServer 依策略处理（过期未认领 410、已认领幂等返回）。
func (s *Store) KnownJoinSecret(ctx context.Context, plain string) (bool, error) {
	if strings.TrimSpace(plain) == "" {
		return false, nil
	}
	var one int
	err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM server_invites WHERE join_hash=?`, crypto.HashToken(plain)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) lookupInvite(ctx context.Context, plain string) (*invite, error) {
	var (
		inv  invite
		expS string
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, expires_at, claimed_at, server_id FROM server_invites WHERE join_hash=?`,
		crypto.HashToken(plain)).Scan(&inv.id, &expS, &inv.claimedAt, &inv.serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inv.expiresAt = parseRFC3339(expS)
	return &inv, nil
}

// EnrollServer 认领 join_secret 并写入 servers 行；以同一 secret 作为该 Agent 长期共享令牌
// （token_cipher = SealSecret(join_secret)，与 ServerConn 解密口径一致）。
//
// 幂等：已认领的 secret 再次调用，返回既有服务器 id 并刷新 base_url——Agent 重启或被调度
// 重建后无需重新部署即可自愈上线（base_url 变了也一并更正）。
func (s *Store) EnrollServer(ctx context.Context, plain, name, baseURL string) (int64, error) {
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	if name == "" || baseURL == "" {
		return 0, errors.New("store: name 与 base_url 均不能为空")
	}
	inv, err := s.lookupInvite(ctx, plain)
	if err != nil {
		return 0, err
	}
	if inv == nil {
		return 0, ErrInviteNotFound
	}
	if inv.claimedAt.Valid {
		if inv.serverID.Valid {
			// 幂等重放：刷新可达地址，返回既有 id。
			if _, err := s.DB.ExecContext(ctx,
				`UPDATE servers SET base_url=? WHERE id=?`, baseURL, inv.serverID.Int64); err != nil {
				return 0, err
			}
			return inv.serverID.Int64, nil
		}
		// 已认领但其服务器已不存在（极少见）：该邀请不可再用。
		return 0, ErrInviteExpired
	}
	// 未认领：过期即拒。
	if time.Now().UTC().After(inv.expiresAt) {
		return 0, ErrInviteExpired
	}
	// 认领：建 server + 标记 invite 已消费，同一事务保证「一行一 invite」。
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	cipher, err := crypto.SealSecret(s.Key, []byte(plain))
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO servers(name, base_url, token_cipher, enabled) VALUES(?,?,?,1)`,
		name, baseURL, cipher)
	if err != nil {
		if IsUniqueErr(err) {
			return 0, ErrServerNameTaken
		}
		return 0, err
	}
	sid, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		`UPDATE server_invites SET claimed_at=?, server_id=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), sid, inv.id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return sid, nil
}
