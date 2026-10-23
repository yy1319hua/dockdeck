// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// Type 区分本机（local）与远端 Agent（agent）：本机由 Hub 进程内服务，
	// 固定走 loopback HTTP，不参与反向长连接寻址。
	Type  ServerType
	Token string
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
		`SELECT id, name, base_url, type, token_cipher, enabled FROM servers WHERE id=?`, id,
	).Scan(&c.ID, &c.Name, &c.BaseURL, &c.Type, &cipher, &on)
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

// ServerType 是受管服务器的来源类型。
//
//   - ServerTypeAgent：远端 Agent（自注册/enroll，默认）
//   - ServerTypeLocal：Hub 本机（Hub 启动时自动注册，进程内 agent 服务）
//
// 类型只由 Hub 代码写入，不接受外部输入；前端据此渲染「本机」徽标并禁用删除。
type ServerType string

const (
	ServerTypeAgent ServerType = "agent"
	ServerTypeLocal ServerType = "local"
)

// ServerSummary 是受管服务器在列表接口的公开投影：只含管理所需字段，
// 绝不携带 Agent 令牌（token_cipher / 解密后的 Token 一律不入此结构）。
type ServerSummary struct {
	ID        int64
	Name      string
	BaseURL   string
	Type      ServerType
	Arch      string
	DockVer   string
	Enabled   bool
	LastSeen  string
	LastError string
}

// ListServers 返回全部受管服务器（含已禁用），供管理列表使用。
// 与 ServerConn 不同，这里不解密 Agent 令牌、也不触达任何秘密字段。
func (s *Store) ListServers(ctx context.Context) ([]ServerSummary, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, base_url, type, arch, docker_ver, enabled, last_seen, last_error FROM servers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ServerSummary, 0)
	for rows.Next() {
		var (
			ss   ServerSummary
			arch sql.NullString
			dv   sql.NullString
			on   int
			seen sql.NullString
			errs sql.NullString
		)
		if err := rows.Scan(&ss.ID, &ss.Name, &ss.BaseURL, &ss.Type, &arch, &dv, &on, &seen, &errs); err != nil {
			return nil, err
		}
		ss.Arch = arch.String
		ss.DockVer = dv.String
		ss.Enabled = on != 0
		ss.LastSeen = seen.String
		ss.LastError = errs.String
		out = append(out, ss)
	}
	return out, rows.Err()
}

// TouchServer 记录一次与 Agent 的通讯结果：最后通讯时间 + 最近一次错误。
//
// 在线状态以 last_seen 为准（见 GET /api/v1/servers 的说明），所以无论采集成败
// 都要更新——只在成功时写的话，一台机器掉线后 last_seen 会永远停在最后一次成功，
// 前端就永远把它显示成在线。errText 为空表示本次成功，此时清空 last_error。
func (s *Store) TouchServer(ctx context.Context, id int64, ts time.Time, errText string) error {
	// 无错误时写 NULL 而不是空串：空串会被前端当成「有一条空的错误信息」。
	var lastErr any
	if errText != "" {
		lastErr = errText
	}
	_, err := s.DB.ExecContext(ctx,
		`UPDATE servers SET last_seen=?, last_error=? WHERE id=?`,
		ts.UTC().Format(time.RFC3339), lastErr, id)
	return err
}

// ServerConnByToken 按明文 Agent 令牌反查服务器，供反向长连接握手使用。
//
// 为什么遍历而不建索引：token_cipher 是密文，索引值也只能是密文，帮不上忙。
// 受管服务器是个位到几十台，O(n) 解密完全可接受。
//
// 语义与 ServerConn 对齐：不存在 → ErrNotFound；禁用 → ErrServerDisabled；
// 单条密文损坏跳过（不能因为一台机器的密文坏了就拒绝所有 Agent 连接）。
func (s *Store) ServerConnByToken(ctx context.Context, token string) (*ServerConn, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, base_url, token_cipher, enabled FROM servers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var found *ServerConn
	for rows.Next() {
		var (
			c      ServerConn
			cipher string
			on     int
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.BaseURL, &cipher, &on); err != nil {
			return nil, err
		}
		plain, err := crypto.OpenSecret(s.Key, cipher)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(plain, []byte(token)) != 1 {
			continue
		}
		if on == 0 {
			return nil, ErrServerDisabled
		}
		c.Token = string(plain)
		found = &c
		// 不 break：命中位置不该通过响应时间泄漏。
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// DeleteServer 物理删除一台受管服务器，返回其名称（供调用方记日志/响应体）。
//
// 级联语义（db.Open 的 DSN 已开 foreign_keys(ON)，删除即生效）：
//   - metrics_snapshots / metrics_latest / container_snapshots：ON DELETE CASCADE，
//     历史指标一并清空；
//   - audit_logs.server_id：ON DELETE SET NULL，审计记录留存、归属置空；
//   - server_invites.server_id：ON DELETE SET NULL，该邀请自此落入 EnrollServer
//     「已认领但其服务器已不存在」分支——旧 Agent 持旧 secret 重放被 410 正确拒绝。
//
// 删除同时释放 servers.name 唯一约束：同名节点可立即重新 enroll
// （解 409 撞名卡死正是删除功能的核心用途之一）。
//
// 不存在返回 ErrNotFound（handler 映射 404）。先查名再删，两步之间
// 极小概率的并发删除由 RowsAffected 兜底。
func (s *Store) DeleteServer(ctx context.Context, id int64) (string, error) {
	if id <= 0 {
		return "", ErrNotFound
	}
	var name string
	if err := s.DB.QueryRowContext(ctx, `SELECT name FROM servers WHERE id=?`, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM servers WHERE id=?`, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return name, nil
}

// MaxServerNameLen 是服务器显示名的字节上限（UTF-8，一个汉字 3 字节）。
const MaxServerNameLen = 64

// RenameServer 改服务器显示名（#86）。名字是 Hub 侧展示与审计记录用的标识，
// 与 Agent 连接无关——改它不动反向链路，也不需要通知 Agent。
// 校验：去首尾空白后非空、≤ MaxServerNameLen 字节；重名回 ErrServerNameTaken。
// 不存在回 ErrNotFound（RowsAffected 兜底并发删除）。
func (s *Store) RenameServer(ctx context.Context, id int64, name string) (string, error) {
	if id <= 0 {
		return "", ErrNotFound
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("store: 服务器名不能为空")
	}
	if len(name) > MaxServerNameLen {
		return "", fmt.Errorf("store: 服务器名最长 %d 字节", MaxServerNameLen)
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE servers SET name=? WHERE id=?`, name, id)
	if err != nil {
		if isUniqueErr(err) {
			return "", ErrServerNameTaken
		}
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return name, nil
}
