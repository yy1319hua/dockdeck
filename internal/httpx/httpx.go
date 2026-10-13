// Package httpx 提供 Hub 的统一 JSON 响应信封、错误码、请求 ID 与安全响应头。
package httpx

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// ctxKey 是包内上下文键类型，避免与其它包碰撞。
type ctxKey string

const (
	keyRequestID ctxKey = "dd.requestID"
	keyClientIP  ctxKey = "dd.clientIP"
	keyUser      ctxKey = "dd.user"
)

// ---- 请求上下文取值 ----

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, keyClientIP, ip)
}
func ClientIP(r *http.Request) string {
	if v, ok := r.Context().Value(keyClientIP).(string); ok && v != "" {
		return v
	}
	// 回退：从 RemoteAddr 去掉端口。
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type UserPrincipal struct {
	UserID   int64
	Username string
	Session  string
}

func WithUser(ctx context.Context, u UserPrincipal) context.Context {
	return context.WithValue(ctx, keyUser, u)
}
func CurrentUser(r *http.Request) (UserPrincipal, bool) {
	v, ok := r.Context().Value(keyUser).(UserPrincipal)
	return v, ok
}

// ---- 响应信封 ----

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Envelope struct {
	Data  interface{} `json:"data,omitempty"`
	Error *ErrorBody  `json:"error,omitempty"`
}

// WriteJSON 输出 { "data": ... }。
func WriteJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{Data: data})
}

// WriteError 输出 { "error": {code,message} }。
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{Error: &ErrorBody{Code: code, Message: message}})
}

// DecodeJSON 严格解析请求体到 dst，拒绝未知字段，限制 1MiB。
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ---- 安全响应头中间件 ----

// SecurityHeaders 统一注入安全头；对 JSON API 与 SPA 均适用。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if RequestID(r.Context()) != "" {
			h.Set("X-Request-Id", RequestID(r.Context()))
		}
		next.ServeHTTP(w, r)
	})
}

// NormalizePath 去掉多余斜杠与尾斜杠，供路由匹配。
func NormalizePath(p string) string {
	p = "/" + strings.Trim(p, "/")
	if p == "/" {
		return "/"
	}
	return p
}
