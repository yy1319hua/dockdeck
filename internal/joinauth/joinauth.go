// Package joinauth 实现 Hub 的纳管邀请（join_secret）鉴权中间件。
// 它是 route.Table 的第三种鉴权模式 AuthJoin：Agent 启动自注册时，唯一的凭据就是
// 上线前带外拿到的一次性 join_secret——此刻它还没有（也不该有）任何 scoped API Token，
// 因此不能复用 apitoken.Require。本包只负责「认得这把 secret」，认领/幂等策略在 handler。
package joinauth

import (
	"context"
	"net/http"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// ctxKey 包内私有，避免与其它上下文键碰撞。
type ctxKey struct{}

func withSecret(ctx context.Context, secret string) context.Context {
	return context.WithValue(ctx, ctxKey{}, secret)
}

// Secret 取出中间件校验过的 join_secret 明文，供 enroll handler 建服务器行用。
func Secret(r *http.Request) (string, bool) {
	v, ok := r.Context().Value(ctxKey{}).(string)
	return v, ok && v != ""
}

// Verify 返回 AuthJoin 包装：要求 Authorization: Bearer <join_secret>，且该串
// 确是 Hub 签发过的邀请；否则 401。认得即放行并把明文注入上下文，不在此判过期/认领。
func Verify(st *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			plain, ok := bearer(r)
			if !ok {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "需 join_secret（Authorization: Bearer）")
				return
			}
			known, err := st.KnownJoinSecret(r.Context(), plain)
			if err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, "internal", "内部错误")
				return
			}
			if !known {
				httpx.WriteError(w, http.StatusUnauthorized, "invalid_join_secret", "join_secret 无法识别")
				return
			}
			next.ServeHTTP(w, r.WithContext(withSecret(r.Context(), plain)))
		})
	}
}

func bearer(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	v := strings.TrimSpace(h[len(prefix):])
	return v, v != ""
}
