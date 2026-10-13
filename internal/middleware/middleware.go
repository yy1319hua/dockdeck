// Package middleware 提供 Hub 的通用请求中间件：请求上下文、限流、审计。
package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// StatusRecorder 记录状态码与字节数，供审计使用。
type StatusRecorder struct {
	http.ResponseWriter
	Status int
	Bytes  int
}

func (r *StatusRecorder) WriteHeader(code int) {
	r.Status = code
	r.ResponseWriter.WriteHeader(code)
}
func (r *StatusRecorder) Write(b []byte) (int, error) {
	if r.Status == 0 {
		r.Status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.Bytes += n
	return n, err
}

// RequestCtx 为每个请求注入 request id 与 client ip。
func RequestCtx(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			if v, err := crypto.RandomToken(8); err == nil {
				id = v
			}
		}
		ctx := httpx.WithRequestID(r.Context(), id)
		ctx = httpx.WithClientIP(ctx, httpx.ClientIP(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RateLimit 是按客户端 IP 的令牌桶，仅用于登录等敏感入口。
type RateLimit struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   time.Duration // 每次 refill 间隔补充 1 个
	lastGC   time.Time
}

type bucket struct {
	tokens   float64
	lastFill time.Time
}

// NewRateLimit：capacity 为突发上限，refillEvery 为补一个令牌的时间。
func NewRateLimit(capacity int, refillEvery time.Duration) *RateLimit {
	return &RateLimit{
		buckets:  make(map[string]*bucket),
		capacity: float64(capacity),
		refill:   refillEvery,
		lastGC:   time.Now(),
	}
}

func (rl *RateLimit) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.gcLocked()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.capacity, lastFill: time.Now()}
		rl.buckets[key] = b
	}
	// 补充令牌
	elapsed := time.Since(b.lastFill)
	add := float64(elapsed) / float64(rl.refill)
	if add > 0 {
		b.tokens += add
		if b.tokens > rl.capacity {
			b.tokens = rl.capacity
		}
		b.lastFill = time.Now()
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens -= 1
	return true
}

// gc 清理长期空闲的桶，防止内存无限增长。
func (rl *RateLimit) gcLocked() {
	if time.Since(rl.lastGC) < time.Minute {
		return
	}
	cutoff := time.Now().Add(-10 * time.Minute)
	for k, b := range rl.buckets {
		if b.lastFill.Before(cutoff) {
			delete(rl.buckets, k)
		}
	}
	rl.lastGC = time.Now()
}

// LoginRateLimit 用客户端 IP 限流，超限返回 429。
func (rl *RateLimit) LoginRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(httpx.ClientIP(r)) {
			httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "尝试过于频繁，请稍后再试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Audit 返回一个 handler 包装：执行内部逻辑后，无论成败都落一条审计。
// action 由调用方显式给定，避免从 URL 反推语义出错。
func Audit(st *store.Store, action string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &StatusRecorder{ResponseWriter: w}
		fn(rec, r)
		res := "ok"
		var errStr string
		if rec.Status >= 400 {
			res = "error"
			errStr = http.StatusText(rec.Status)
		}
		p, _ := httpx.CurrentUser(r)
		_ = st.WriteAudit(r.Context(), store.AuditEntry{
			UserID: p.UserID, Username: p.Username, Action: action,
			Result: res, Error: errStr, Target: r.URL.Path,
			IP: httpx.ClientIP(r), RequestID: httpx.RequestID(r.Context()),
		})
	}
}
