// Package auth 实现 Hub 的账号密码登录、会话中间件与首启建号。
package auth

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// Options 是认证策略参数。
type Options struct {
	CookieName string
	IdleTTL    time.Duration
	AbsTTL     time.Duration
	Secure     bool // TLS 模式下 true
	MaxFails   int
	LockDur    time.Duration
}

// DefaultOptions 空闲 8h / 绝对 72h，5 次失败锁 15 分钟。
func DefaultOptions() Options {
	return Options{
		CookieName: "dd_session",
		IdleTTL:    8 * time.Hour,
		AbsTTL:     72 * time.Hour,
		Secure:     true,
		MaxFails:   5,
		LockDur:    15 * time.Minute,
	}
}

// Manager 持有 store 与策略。
type Manager struct {
	st  *store.Store
	opt Options
}

func NewManager(st *store.Store, opt Options) *Manager {
	return &Manager{st: st, opt: opt}
}

// ---- HTTP 处理器 ----

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login 校验口令，成功则签发会话 cookie。
func (m *Manager) Login(w http.ResponseWriter, r *http.Request) {
	var body loginReq
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "请求体解析失败")
		return
	}
	u, err := m.st.GetUserByName(r.Context(), body.Username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 不回显用户名是否存在，统一失败信息。
			m.audit(r, 0, body.Username, "auth.login", "error", "user_not_found")
			httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "用户名或口令错误")
			return
		}
		m.audit(r, 0, body.Username, "auth.login", "error", "internal")
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "内部错误")
		return
	}

	if u.LockedUntil != nil && time.Now().Before(*u.LockedUntil) {
		m.audit(r, u.ID, u.Username, "auth.login", "error", "locked")
		httpx.WriteError(w, http.StatusTooManyRequests, "account_locked", "账户暂时锁定，请稍后再试")
		return
	}
	if u.Disabled {
		m.audit(r, u.ID, u.Username, "auth.login", "error", "disabled")
		httpx.WriteError(w, http.StatusForbidden, "account_disabled", "账户已禁用")
		return
	}

	ok, err := crypto.VerifyPassword(body.Password, u.PwHash)
	if err != nil || !ok {
		if err == nil { // 口令不匹配
			_, _ = m.st.MarkLoginFailure(r.Context(), u.ID, m.opt.MaxFails, m.opt.LockDur)
		}
		m.audit(r, u.ID, u.Username, "auth.login", "error", "bad_password")
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "用户名或口令错误")
		return
	}

	if err := m.st.RecordLoginSuccess(r.Context(), u.ID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	token, _, err := m.st.CreateSession(r.Context(), u.ID, m.opt.IdleTTL, m.opt.AbsTTL,
		httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "会话创建失败")
		return
	}
	m.setCookie(w, token)
	m.audit(r, u.ID, u.Username, "auth.login", "ok", "")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"username": u.Username})
}

// Logout 撤销当前会话并清 cookie。
func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(m.opt.CookieName); err == nil && c.Value != "" {
		_ = m.st.RevokeSession(r.Context(), c.Value)
		if p, ok := httpx.CurrentUser(r); ok {
			m.audit(r, p.UserID, p.Username, "auth.logout", "ok", "")
		}
		m.clearCookie(w)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type meResp struct {
	Username      string `json:"username"`
	SetupRequired bool   `json:"setup_required"`
}

// Me 返回当前登录用户；未登录时若系统尚无账户，报告 setup_required 以便前端引导建号。
func (m *Manager) Me(w http.ResponseWriter, r *http.Request) {
	if p, ok := httpx.CurrentUser(r); ok {
		httpx.WriteJSON(w, http.StatusOK, meResp{Username: p.Username})
		return
	}
	anyUser, _ := m.st.HasAnyUser(r.Context())
	httpx.WriteJSON(w, http.StatusOK, meResp{SetupRequired: !anyUser})
}

type setupReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Setup 首启建号：仅当系统无任何用户时允许，成功即登录。
func (m *Manager) Setup(w http.ResponseWriter, r *http.Request) {
	anyUser, err := m.st.HasAnyUser(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	if anyUser {
		httpx.WriteError(w, http.StatusConflict, "already_initialized", "系统已初始化，请用登录接口")
		return
	}
	var body setupReq
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "请求体解析失败")
		return
	}
	if len(body.Password) < 8 {
		httpx.WriteError(w, http.StatusBadRequest, "weak_password", "口令至少 8 位")
		return
	}
	id, err := m.st.CreateUser(r.Context(), body.Username, body.Password)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	token, _, err := m.st.CreateSession(r.Context(), id, m.opt.IdleTTL, m.opt.AbsTTL,
		httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "会话创建失败")
		return
	}
	m.setCookie(w, token)
	m.audit(r, id, body.Username, "auth.setup", "ok", "")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"username": body.Username})
}

type changePwReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// ChangePassword 校验旧口令后改密，并撤销该用户全部会话（含当前）。
func (m *Manager) ChangePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := httpx.CurrentUser(r)
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "需登录")
		return
	}
	var body changePwReq
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "请求体解析失败")
		return
	}
	u, err := m.st.GetUserByName(r.Context(), p.Username)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	okp, err := crypto.VerifyPassword(body.OldPassword, u.PwHash)
	if err != nil || !okp {
		m.audit(r, p.UserID, p.Username, "auth.change_password", "error", "bad_old_password")
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "旧口令错误")
		return
	}
	if len(body.NewPassword) < 8 {
		httpx.WriteError(w, http.StatusBadRequest, "weak_password", "新口令至少 8 位")
		return
	}
	if err := m.st.UpdatePassword(r.Context(), u.ID, body.NewPassword); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "改密失败")
		return
	}
	// 改密后全端失效，强制重新登录。
	_ = m.st.RevokeAllForUser(r.Context(), u.ID)
	m.clearCookie(w)
	m.audit(r, p.UserID, p.Username, "auth.change_password", "ok", "")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "relogin_required": true})
}

// ---- 会话中间件 ----

// TryAuth 尝试解析会话并注入 principal；无论成功与否都放行（供 /me 等可选认证端点）。
func (m *Manager) TryAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(m.opt.CookieName)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		sess, err := m.st.LookupSession(r.Context(), c.Value, m.opt.IdleTTL)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		uname := m.usernameByID(r, sess.UserID)
		if uname == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := httpx.WithUser(r.Context(), httpx.UserPrincipal{
			UserID: sess.UserID, Username: uname, Session: c.Value,
			AuthKind: httpx.AuthKindSession,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAuth 校验 cookie 会话，注入 UserPrincipal；失败返回 401。
func (m *Manager) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.ServeSession(w, r, next, "")
	})
}

// ServeSession 校验 cookie 会话后放行，失败自行写 401。
// 签名刻意与 apitoken.SessionVerifier 一致——Go 的函数类型按签名结构化匹配，
// 这样两个包不必互相引用（会话不该知道 Token 的存在）。
// scope 参数对会话无意义：浏览器登录即全权限。
func (m *Manager) ServeSession(w http.ResponseWriter, r *http.Request, next http.Handler, scope string) {
	c, err := r.Cookie(m.opt.CookieName)
	if err != nil || c.Value == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "需登录")
		return
	}
	sess, err := m.st.LookupSession(r.Context(), c.Value, m.opt.IdleTTL)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, http.StatusUnauthorized, "session_invalid", "会话无效或已过期")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	uname := m.usernameByID(r, sess.UserID)
	if uname == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "session_invalid", "会话无效")
		return
	}
	ctx := httpx.WithUser(r.Context(), httpx.UserPrincipal{
		UserID: sess.UserID, Username: uname, Session: c.Value,
		AuthKind: httpx.AuthKindSession,
	})
	next.ServeHTTP(w, r.WithContext(ctx))
}

func (m *Manager) usernameByID(r *http.Request, id int64) string {
	var name string
	err := m.st.DB.QueryRowContext(r.Context(),
		`SELECT username FROM users WHERE id=? AND disabled=0`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	return name
}

// ---- cookie / audit helpers ----

func (m *Manager) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.opt.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.opt.Secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(m.opt.AbsTTL.Seconds()),
	})
}

func (m *Manager) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: m.opt.CookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: m.opt.Secure, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

func (m *Manager) audit(r *http.Request, uid int64, uname, action, result, errStr string) {
	_ = m.st.WriteAudit(r.Context(), store.AuditEntry{
		UserID: uid, Username: uname, Action: action, Result: result,
		Error: errStr, IP: httpx.ClientIP(r), RequestID: httpx.RequestID(r.Context()),
	})
}
