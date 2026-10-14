// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yy1319hua/dockdeck/internal/crypto"
)

// ErrServerDisabled 表示服务器存在但已被管理员禁用。
// 实时转发一律拒绝：禁用服务器的历史指标仍可查（ServerExists 对禁用放行），
// 但向一台已下线的 Agent 发实时请求没有意义，且常是配置滞后导致的误操作。
var ErrServerDisabled = errors.New("store: 服务器已禁用")

// ServerConn 是 Hub 实时调用一台 Agent 所需的连接信息。
// Token 是解密后的共享凭证，只进内存——不写日志、不进错误信息、不进响应体。
type ServerConn struct {
	ID      int64
	Name    string
	BaseURL string
	Token   string
}

// ServerConn 按 id 读取连接信息并解密 Agent 令牌。
//
// 错误分类（调用方据此映射 HTTP 状态码）：
//   - ErrNotFound       服务器不存在 → 404
//   - ErrServerDisabled 已禁用 → 403
//   - 其它 error        库故障或令牌解密失败 → 500
//
// 解密失败不静默跳过：那会把「认证失败」伪装成「网络不通」，排障时骗人；
// 宁可 500 让管理员看见密文损坏。
func (s *Store) ServerConn(ctx context.Context, id int64) (*ServerConn, error) {
	if id <= 0 {
		return nil, ErrNotFound
	}
	var (
		c      ServerConn
		cipher string
		on     int
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, name, base_url, token_cipher, enabled FROM servers WHERE id=?`, id,
	).Scan(&c.ID, &c.Name, &c.BaseURL, &cipher, &on)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if on == 0 {
		return nil, ErrServerDisabled
	}
	plain, err := crypto.OpenSecret(s.Key, cipher)
	if err != nil {
		return nil, fmt.Errorf("store: 服务器 %q 的 Agent 令牌解密失败: %w", c.Name, err)
	}
	c.Token = string(plain)
	return &c, nil
}
