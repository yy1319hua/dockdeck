// Package apitoken 实现 API Token 的签发、校验与 scope 判定。
// Token 供 App / 脚本以 Authorization: Bearer 直连 /api/*，与浏览器 cookie 会话并存且互不信任。
package apitoken

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// Prefix 让 Token 一眼可辨用途，也便于凭据扫描器识别泄漏。
const Prefix = "ddk_"

const bearerPrefix = "Bearer "

// scope 常量。写不蕴含读：需要两种能力的 App 得同时勾上，避免隐式扩权。
const (
	ScopeReadMetrics     = "read:metrics"
	ScopeReadContainers  = "read:containers"
	ScopeWriteContainers = "write:containers"
	ScopeWriteFiles      = "write:files"
	ScopeReadAudit       = "read:audit"
)

// AllScopes 是签发界面可勾选的全集，也是校验时的合法域。
var AllScopes = []string{
	ScopeReadMetrics,
	ScopeReadContainers,
	ScopeWriteContainers,
	ScopeWriteFiles,
	ScopeReadAudit,
}

// ScopeDesc 是 scope → 中文说明。写在这里而不是 openapi 包：
// openapi 依赖 apitoken，反过来会成环；字典由本包拥有，spec 与 UI 都读它。
var ScopeDesc = map[string]string{
	ScopeReadMetrics:     "读取主机指标与监控历史",
	ScopeReadContainers:  "读取容器/镜像/Compose 清单与日志",
	ScopeWriteContainers: "启停容器、执行 Compose 操作",
	ScopeWriteFiles:      "在白名单根目录内读写文件",
	ScopeReadAudit:       "读取审计日志",
}

// 输入类错误，供 handler 区分 400 与 409，不靠比对错误文案。
var (
	ErrInvalidInput = errors.New("apitoken: 输入不合法")
	ErrNameTaken    = errors.New("apitoken: 名称已存在")
)

// ValidateScopes 去重、排序并拒绝未知 scope。
// 空列表非法：没有权限的 Token 只会让排查变困难。
func ValidateScopes(scopes []string) ([]string, error) {
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !knownScope(s) {
			return nil, fmt.Errorf("%w: 未知 scope %q", ErrInvalidInput, s)
		}
		seen[s] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("%w: 至少需要一个 scope", ErrInvalidInput)
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func knownScope(s string) bool {
	for _, k := range AllScopes {
		if k == s {
			return true
		}
	}
	return false
}

// Token 是一条 API Token 记录。Plaintext 仅在 Issue 返回时有值，此后不可再取。
type Token struct {
	ID         int64
	Name       string
	Scopes     []string
	CreatedBy  int64
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	Plaintext  string
}

// Active 判定是否仍可用。
func (t *Token) Active(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return false
	}
	return true
}

// API 是 Token 的访问器，无状态，可并发使用。
type API struct{ db *sql.DB }

func NewAPI(db *sql.DB) *API { return &API{db: db} }

const tokenCols = `id,name,scopes,created_by,created_at,last_used_at,expires_at,revoked_at`

// Issue 生成一枚 Token：明文 = 前缀 + 32 字节 base64url，库里只存其 SHA-256。
// ttl 为 0 表示不过期。creatorID 必须是已登录用户，由 handler 显式传入。
func (a *API) Issue(ctx context.Context, creatorID int64, name string, scopes []string, ttl time.Duration) (*Token, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: name 不能为空", ErrInvalidInput)
	}
	if len(name) > 64 {
		return nil, fmt.Errorf("%w: name 过长（≤64）", ErrInvalidInput)
	}
	clean, err := ValidateScopes(scopes)
	if err != nil {
		return nil, err
	}
	raw, err := crypto.RandomToken(32)
	if err != nil {
		return nil, err
	}
	plain := Prefix + raw

	var expires any
	var expPtr *time.Time
	if ttl > 0 {
		e := time.Now().UTC().Add(ttl)
		expires = e.Format(time.RFC3339)
		expPtr = &e
	}
	res, err := a.db.ExecContext(ctx,
		`INSERT INTO api_tokens(name,token_hash,scopes,created_by,expires_at) VALUES(?,?,?,?,?)`,
		name, crypto.HashToken(plain), strings.Join(clean, ","), creatorID, expires)
	if err != nil {
		if store.IsUniqueErr(err) {
			return nil, fmt.Errorf("%w: %q", ErrNameTaken, name)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Token{
		ID: id, Name: name, Scopes: clean, CreatedBy: creatorID,
		CreatedAt: time.Now().UTC(), ExpiresAt: expPtr, Plaintext: plain,
	}, nil
}

// Lookup 按明文取可用 Token，并刷新 last_used_at。
// 写库节流：距上次记录不足 60s 不再写，免得每次 API 调用都产生一次写事务。
func (a *API) Lookup(ctx context.Context, plaintext string) (*Token, error) {
	row := a.db.QueryRowContext(ctx,
		`SELECT `+tokenCols+` FROM api_tokens WHERE token_hash=?`, crypto.HashToken(plaintext))
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !t.Active(time.Now().UTC()) {
		return nil, store.ErrNotFound
	}
	if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
		_, _ = a.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=?`,
			time.Now().UTC().Format(time.RFC3339), t.ID)
	}
	return t, nil
}

// List 返回全部 Token（不含哈希，也不含明文）。
func (a *API) List(ctx context.Context) ([]Token, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var (
			t               Token
			csv, createdS   string
			lastU, exp, rev sql.NullString
		)
		if err := rows.Scan(&t.ID, &t.Name, &csv, &t.CreatedBy, &createdS, &lastU, &exp, &rev); err != nil {
			return nil, err
		}
		t.Scopes = splitScopes(csv)
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdS)
		t.LastUsedAt, t.ExpiresAt, t.RevokedAt = nullTime(lastU), nullTime(exp), nullTime(rev)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke 立即作废一枚 Token；返回是否命中（已撤销的再撤销返回 false）。
func (a *API) Revoke(ctx context.Context, id int64) (bool, error) {
	res, err := a.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ExtractBearer 取出 Authorization: Bearer 的值；缺失或格式不符返回 ("", false)。
// 导出供 /ws 握手等非中间件路径复用同一套凭据解析。
func ExtractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, bearerPrefix) {
		return "", false
	}
	v := strings.TrimSpace(h[len(bearerPrefix):])
	return v, v != ""
}

// principal 把 Token 记录变成请求主体。UserID 取 created_by——Token 是
// 「代表创建者行事」的凭据，权限边界由 scope 收紧，而不是挂到一个虚构的匿名账户上。
// 创建者不存在或已禁用时返回 ErrNotFound。
func (a *API) principal(ctx context.Context, t *Token) (*httpx.UserPrincipal, error) {
	var uname string
	err := a.db.QueryRowContext(ctx,
		`SELECT username FROM users WHERE id=? AND disabled=0`, t.CreatedBy).Scan(&uname)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &httpx.UserPrincipal{
		UserID:    t.CreatedBy,
		Username:  uname,
		AuthKind:  httpx.AuthKindToken,
		TokenID:   t.ID,
		TokenName: t.Name,
		Scopes:    t.Scopes,
	}, nil
}

// verifyBearer 解析明文令牌并还原主体；非 0 code 表示应直接返回该状态。
func (a *API) verifyBearer(ctx context.Context, plain string) (*httpx.UserPrincipal, int, string) {
	t, err := a.Lookup(ctx, plain)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, http.StatusUnauthorized, "API Token 无效、已撤销或已过期"
		}
		return nil, http.StatusInternalServerError, "内部错误"
	}
	p, err := a.principal(ctx, t)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, http.StatusForbidden, "Token 创建者已禁用或不存在"
		}
		return nil, http.StatusInternalServerError, "内部错误"
	}
	return p, 0, ""
}

// SessionVerifier 由 auth 包提供：校验 cookie 会话，注入主体后放行；失败时自行写 401。
// scope 对会话没有意义（浏览器登录即全权限），保留参数只为让两类身份共用同一条包装链。
type SessionVerifier func(w http.ResponseWriter, r *http.Request, next http.Handler, scope string)

// Require 返回鉴权包装：Bearer 优先，无 Bearer 时回落到 cookie 会话。
// Bearer 与 cookie 同时存在时只认 Bearer，不做身份合并——否则一枚只读 Token
// 可以借浏览器会话的高权限越权。身份不成立 → 401；scope 不足 → 403。
func Require(session SessionVerifier, a *API, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			plain, hasBearer := ExtractBearer(r)
			if !hasBearer {
				if session == nil {
					httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "需登录")
					return
				}
				session(w, r, next, scope)
				return
			}
			p, code, msg := a.verifyBearer(r.Context(), plain)
			if code != 0 {
				httpx.WriteError(w, code, "token_invalid", msg)
				return
			}
			if scope != "" && !p.HasScope(scope) {
				httpx.WriteError(w, http.StatusForbidden, "scope_denied",
					fmt.Sprintf("该 Token 缺少所需权限 %q", scope))
				return
			}
			next.ServeHTTP(w, r.WithContext(httpx.WithUser(r.Context(), *p)))
		})
	}
}

// TryBearer 解析 Bearer 并注入主体；无 Bearer 时原样放行，供「可选认证」端点使用。
// 它从不读 cookie，因此不会与会话中间件互相冒充。带了坏 Token 则明确拒绝，
// 不降级成匿名——否则一次拼错的 Header 会把请求悄悄送进未鉴权分支。
func (a *API) TryBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain, ok := ExtractBearer(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		p, code, msg := a.verifyBearer(r.Context(), plain)
		if code != 0 {
			httpx.WriteError(w, code, "token_invalid", msg)
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithUser(r.Context(), *p)))
	})
}

func scanToken(row *sql.Row) (*Token, error) {
	var (
		t               Token
		csv, createdS   string
		lastU, exp, rev sql.NullString
	)
	if err := row.Scan(&t.ID, &t.Name, &csv, &t.CreatedBy, &createdS, &lastU, &exp, &rev); err != nil {
		return nil, err
	}
	t.Scopes = splitScopes(csv)
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdS)
	t.LastUsedAt, t.ExpiresAt, t.RevokedAt = nullTime(lastU), nullTime(exp), nullTime(rev)
	return &t, nil
}

func splitScopes(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func nullTime(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s.String)
	if err != nil {
		return nil
	}
	tt := t.UTC()
	return &tt
}
