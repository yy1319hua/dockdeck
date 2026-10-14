// Package middleware 提供 Hub 的通用请求中间件：请求上下文、限流、审计。
package middleware

import (
	"net/http"
	"strconv"
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

// Flush 透传底层 writer 的 Flush。流式端点（日志 follow）套审计时这条
// 不能少：StatusRecorder 挡在中间，不做透传的话 handler 里的
// http.Flusher 断言失败，逐块推送被吞——浏览器看到的就不是滚动而是
// 「一次性吐完」，且 e2e 用 recorder 跑还测不出来（recorder 不缓冲）。
func (r *StatusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找底层 writer 的 optional interface
// （Flush/Hijack 等），Go 1.20+ 的标准透出方式。
func (r *StatusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

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
//
// 服务器级端点（/api/v1/servers/{id}/...）的 {id} 自动进 server_id——
// 这是全 Hub 的路径参数约定（route 登记表所有服务器级端点都用 {id}）。
// 解析不了记 0：审计不因一个畸形 ID 而失败，那条请求本身早被 handler
// 以 400 拒掉，失败记录比 server_id 更值得留。
//
// 读与写都审计（S3.4 起容器全线如此）：「谁看过哪台机器」与
// 「谁动了哪台机器」同样是安全审计对象。流式端点（日志 follow）的
// 审计行落在流结束时——连接多久，记录就多久后落库。
//
// 摆放位置在鉴权链之内（apitoken.Require 通过后才执行）：匿名 401 与
// scope 拒绝属鉴权层事件，由登录/Token 流程自行审计，不在此留行——
// 否则每枚探针机器人都会在审计表里留痕，真正的操作记录反而难找。
func Audit(st *store.Store, action string, fn http.HandlerFunc) http.HandlerFunc {
	return audit(st, func(*http.Request) string { return action }, fn)
}

// AuditFunc 同 Audit，但审计动作按请求推导——用于一个 handler 承载多种
// 语义的端点（POST .../containers/{cid}/{action}：restart 与 kill 的
// 风险等级天差地别，审计里必须分开可查，不能混成一个 container.action）。
func AuditFunc(st *store.Store, actionOf func(*http.Request) string, fn http.HandlerFunc) http.HandlerFunc {
	if actionOf == nil {
		panic("middleware: AuditFunc 的 actionOf 不能为 nil")
	}
	return audit(st, actionOf, fn)
}

func audit(st *store.Store, actionOf func(*http.Request) string, fn http.HandlerFunc) http.HandlerFunc {
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
			UserID: p.UserID, Username: p.Username, ServerID: serverIDFromPath(r),
			Action: actionOf(r),
			Result: res, Error: errStr, Target: r.URL.Path,
			IP: httpx.ClientIP(r), RequestID: httpx.RequestID(r.Context()),
		})
	}
}

// serverIDFromPath 从 {id} 路径参数取服务器 ID，约定见 Audit 注释。
func serverIDFromPath(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
