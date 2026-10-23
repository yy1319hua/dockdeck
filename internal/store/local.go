// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/crypto"
)

// ErrServerLocal 表示试图删除 Hub 本机（servers.type=local）。
//
// 本机由 Hub 进程内 Agent 服务：删掉它下次启动会被重新注册，而级联会先清空
// 本机全部历史指标——没有收益只有损失，故在库层直接拒绝（handler 映射 409）。
var ErrServerLocal = errors.New("store: 本机不可删除")

// Hub 本机在服务器列表里的默认显示名；若该名已被远端服务器占用则退回后者。
const (
	LocalServerName         = "本机"
	localServerNameFallback = "本机 (Hub)"
)

// EnsureLocalServer 幂等注册 Hub 本机服务器（type=local），返回其 id 与可用令牌。
//
// 幂等键是 type='local' 而不是 name：用户可能改过显示名，默认名也可能与一台远端
// Agent 重名。已有本机行时复用其 id（即「固定 server_id」），只刷新 base_url 与
// 启用状态；令牌稳定复用（仅密文损坏时重建），保证 Hub 重启后进程内 Agent 与调用
// 方无需换令牌。首次注册生成 32 字节随机令牌并以主密钥加密入库。
//
// 令牌只用于 Hub↔本机 Agent 的 loopback 调用，不出现在列表接口；调用方如无需
// 可直接丢弃返回值。
func (s *Store) EnsureLocalServer(ctx context.Context, baseURL string) (id int64, token string, err error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return 0, "", errors.New("store: 本机 base_url 不能为空")
	}

	var cipher string
	err = s.DB.QueryRowContext(ctx,
		`SELECT id, token_cipher FROM servers WHERE type=? ORDER BY id LIMIT 1`,
		string(ServerTypeLocal)).Scan(&id, &cipher)
	switch {
	case err == nil:
		// 已注册：id 不动，只刷新可达地址与启用状态。
		// 名字不动——可能是用户改过的，也可能是首次注册时的冲突回退名。
		if _, uerr := s.DB.ExecContext(ctx,
			`UPDATE servers SET base_url=?, enabled=1, last_error=NULL WHERE id=?`,
			baseURL, id); uerr != nil {
			return 0, "", uerr
		}
		if plain, derr := crypto.OpenSecret(s.Key, cipher); derr == nil {
			return id, string(plain), nil
		}
		// 密文损坏（主密钥轮换/库被手工改动）：重建令牌，避免本机永久不可用。
		plain, nc, gerr := s.newLocalToken()
		if gerr != nil {
			return 0, "", gerr
		}
		if _, uerr := s.DB.ExecContext(ctx, `UPDATE servers SET token_cipher=? WHERE id=?`, nc, id); uerr != nil {
			return 0, "", uerr
		}
		return id, plain, nil
	case errors.Is(err, sql.ErrNoRows):
		// 首次注册，落到下面插入。
	default:
		return 0, "", err
	}

	plain, cipher, err := s.newLocalToken()
	if err != nil {
		return 0, "", err
	}
	name := LocalServerName
	var taken int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM servers WHERE name=?`, name).Scan(&taken); err != nil {
		return 0, "", err
	}
	if taken > 0 {
		name = localServerNameFallback
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO servers(name, base_url, token_cipher, type, enabled) VALUES(?,?,?,?,1)`,
		name, baseURL, cipher, string(ServerTypeLocal))
	if err != nil {
		if IsUniqueErr(err) {
			return 0, "", ErrServerNameTaken
		}
		return 0, "", err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	return id, plain, nil
}

// newLocalToken 生成本机 Agent 的共享令牌及其密文形式。
func (s *Store) newLocalToken() (plain, cipher string, err error) {
	tok, err := crypto.RandomToken(32)
	if err != nil {
		return "", "", fmt.Errorf("store: 生成本机 Agent 令牌失败: %w", err)
	}
	c, err := crypto.SealSecret(s.Key, []byte(tok))
	if err != nil {
		return "", "", fmt.Errorf("store: 加密本机 Agent 令牌失败: %w", err)
	}
	return tok, c, nil
}
