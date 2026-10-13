// handlers.go：API Token 的管理端点与 scope 字典端点。
// Token 管理端点一律只认浏览器会话（route.AuthSession）——若允许 Token 调用它们，
// 一枚泄漏的 Token 就能自行签发更多 Token，权限再也收不回来。
package apitoken

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/httpx"
)

// ttlChoices 把可选有效期固定成白名单，免得调用方塞任意时长、把过期策略变成摆设。
var ttlChoices = map[string]time.Duration{
	"":      0,
	"7d":    7 * 24 * time.Hour,
	"30d":   30 * 24 * time.Hour,
	"90d":   90 * 24 * time.Hour,
	"1y":    365 * 24 * time.Hour,
	"never": 0,
}

// TTLParam 把请求里的 ttl 字符串换成时长；第二个返回值为 false 表示取值不在白名单内。
func TTLParam(s string) (time.Duration, bool) {
	d, ok := ttlChoices[strings.TrimSpace(s)]
	return d, ok
}

type tokenView struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
	Active     bool     `json:"active"`
}

func view(t Token) tokenView {
	return tokenView{
		ID: t.ID, Name: t.Name, Scopes: t.Scopes,
		CreatedAt:  fmtTime(t.CreatedAt),
		LastUsedAt: fmtTimeP(t.LastUsedAt),
		ExpiresAt:  fmtTimeP(t.ExpiresAt),
		RevokedAt:  fmtTimeP(t.RevokedAt),
		Active:     t.Active(time.Now().UTC()),
	}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func fmtTimeP(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ListHandler 返回全部 Token（不含明文，也不含哈希）。
func (a *API) ListHandler(w http.ResponseWriter, r *http.Request) {
	ts, err := a.List(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	out := make([]tokenView, 0, len(ts))
	for _, t := range ts {
		out = append(out, view(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type createReq struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	TTL    string   `json:"ttl"`
}

type createResp struct {
	Token     tokenView `json:"token"`
	Plaintext string    `json:"plaintext"`
	Warning   string    `json:"warning"`
}

// CreateHandler 签发一枚 Token。明文只在这一次响应里出现——库里只有哈希，事后无处可查。
func (a *API) CreateHandler(w http.ResponseWriter, r *http.Request) {
	p, ok := httpx.CurrentUser(r)
	if !ok || p.ViaToken() {
		// 走到这里说明挂载方式违背了「仅会话」约定。宁可 500 也不拿 0 当创建者，
		// 否则会出现一枚 created_by 指向不存在用户的 Token。
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "该端点要求浏览器会话主体")
		return
	}
	var body createReq
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "请求体解析失败")
		return
	}
	ttl, ok := TTLParam(body.TTL)
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "bad_ttl", "ttl 只接受 7d / 30d / 90d / 1y / never")
		return
	}
	t, err := a.Issue(r.Context(), p.UserID, body.Name, body.Scopes, ttl)
	if err != nil {
		switch {
		case errors.Is(err, ErrNameTaken):
			httpx.WriteError(w, http.StatusConflict, "name_taken", "该名称已被占用")
		case errors.Is(err, ErrInvalidInput):
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		default:
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "签发失败")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, createResp{
		Token:     view(*t),
		Plaintext: t.Plaintext,
		Warning:   "明文仅此一次可见，请立即保存；本端点不会再次返回它。",
	})
}

// RevokeHandler 作废一枚 Token，路径参数 {id}。
func (a *API) RevokeHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "id 非法")
		return
	}
	hit, err := a.Revoke(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	if !hit {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "Token 不存在或已撤销")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

type scopeItem struct {
	Scope       string `json:"scope"`
	Description string `json:"description"`
}

// ScopesHandler 输出 scope 字典：App 与 UI 都从这里取全集与中文说明，
// 新增 scope 时不必在三处文案里同步。
func ScopesHandler(w http.ResponseWriter, r *http.Request) {
	out := make([]scopeItem, 0, len(AllScopes))
	for _, s := range AllScopes {
		out = append(out, scopeItem{Scope: s, Description: ScopeDesc[s]})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
