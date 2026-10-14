// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// flushGate 是一个「Flush 之前数据不上线」的假 ResponseWriter，
// 模拟真实连接的缓冲语义。httptest.NewRecorder 不缓冲，缺了 Flush
// 透传它也照单全收——拿它测流式透传等于没测，必须自己造一个。
type flushGate struct {
	header  http.Header
	flushed bool
}

func (g *flushGate) Header() http.Header {
	if g.header == nil {
		g.header = http.Header{}
	}
	return g.header
}
func (g *flushGate) Write(b []byte) (int, error) { return len(b), nil }
func (g *flushGate) WriteHeader(int)             {}
func (g *flushGate) Flush()                      { g.flushed = true }

// TestStatusRecorderForwardsFlush 保证审计包装不吞流式推送：
// 日志 follow 经 Audit 包一层后，handler 必须仍拿得到 http.Flusher，
// 且 Flush 真的到达底层 writer——否则浏览器看到的是「一次性吐完」
// 而非滚动，而用 recorder 跑的 e2e 抓不到这个退化。
func TestStatusRecorderForwardsFlush(t *testing.T) {
	g := &flushGate{}
	rec := &StatusRecorder{ResponseWriter: g}
	if rec.Unwrap() != http.ResponseWriter(g) {
		t.Error("Unwrap 应透出底层 writer（ResponseController 依赖）")
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("StatusRecorder 应保持 http.Flusher 能力（流式端点依赖）")
			return
		}
		if _, err := w.Write([]byte("frame-1")); err != nil {
			t.Errorf("Write 应透传: %v", err)
		}
		flusher.Flush()
		if _, err := w.Write([]byte("frame-2")); err != nil {
			t.Errorf("Write 应透传: %v", err)
		}
	})
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if !g.flushed {
		t.Error("Flush 未被透传：流式端点会退化成一次性吐完")
	}
	if rec.Status != http.StatusOK {
		t.Errorf("隐式 200 应被记录，得 %d", rec.Status)
	}
	if rec.Bytes != len("frame-1")+len("frame-2") {
		t.Errorf("字节数应累计，得 %d", rec.Bytes)
	}
}

// TestServerIDFromPath 覆盖审计的 server_id 取材：{id} 合法 → 取值；
// 无 {id} 的端点（登录等）与畸形 ID → 0（不落 server_id，审计不因
// 此失败——请求本身早被 handler 拒了）。
func TestServerIDFromPath(t *testing.T) {
	cases := []struct {
		id   string // SetPathValue 的值；空表示不设置
		want int64
	}{
		{"7", 7},
		{"", 0},    // 非服务器级端点没有 {id}
		{"abc", 0}, // 畸形
		{"0", 0},   // 非正数
		{"-3", 0},  // 负数
		{"9223372036854775807", 9223372036854775807}, // int64 上限
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/servers/x/containers", nil)
		if c.id != "" {
			req.SetPathValue("id", c.id)
		}
		if got := serverIDFromPath(req); got != c.want {
			t.Errorf("id=%q 应得 %d，got %d", c.id, c.want, got)
		}
	}
}

// TestRequestCtxAdoptsIdempotencyKey 覆盖幂等键 X-DD-Request-Id 的采纳规则
// （ARCHITECTURE.md「写操作全部幂等键落审计」的入口）：
//   - 规范头优先，旧头 X-Request-Id 兜底（入站兼容）；
//   - 带值但形态非法 → 400 且不放行——悄悄换掉比拒掉更危险，
//     客户端的重试关联会无声断掉；
//   - 缺省现场生成，形态合法；
//   - 响应头回显规范名（SecurityHeaders 与 RequestCtx 同序装配，
//     与 main.go 真实链路一致）。
func TestRequestCtxAdoptsIdempotencyKey(t *testing.T) {
	build := func() (http.Handler, *string) {
		var seen string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = httpx.RequestID(r.Context())
		})
		return RequestCtx(httpx.SecurityHeaders(next)), &seen
	}

	call := func(h http.Handler, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("规范头采纳并回显", func(t *testing.T) {
		h, seen := build()
		rec := call(h, map[string]string{protocol.HeaderRequestID: "e2e-key-42"})
		if *seen != "e2e-key-42" {
			t.Errorf("ctx 应采纳规范头，得 %q", *seen)
		}
		if got := rec.Header().Get(protocol.HeaderRequestID); got != "e2e-key-42" {
			t.Errorf("响应头应回显规范名，得 %q", got)
		}
		if got := rec.Header().Get("X-Request-Id"); got != "e2e-key-42" {
			t.Errorf("旧名响应头过渡期内应同步回显，得 %q", got)
		}
	})

	t.Run("旧头兜底", func(t *testing.T) {
		h, seen := build()
		rec := call(h, map[string]string{"X-Request-Id": "legacy-7"})
		if *seen != "legacy-7" {
			t.Errorf("ctx 应兜底采纳旧头，得 %q", *seen)
		}
		if got := rec.Header().Get(protocol.HeaderRequestID); got != "legacy-7" {
			t.Errorf("响应头应回显规范名，得 %q", got)
		}
	})

	t.Run("规范头优先于旧头", func(t *testing.T) {
		h, seen := build()
		call(h, map[string]string{
			protocol.HeaderRequestID: "canonical-1",
			"X-Request-Id":           "legacy-2",
		})
		if *seen != "canonical-1" {
			t.Errorf("两个头都在时应取规范头，得 %q", *seen)
		}
	})

	// 非法值一律 400 且不碰 next——该拒的请求不该放行，
	// 更不该被静默替换成合法值（那会让客户端的重试关联无声断掉）。
	for _, bad := range []string{"bad key", "bad!", "键", "a\r\nX-Evil: 1", strings.Repeat("x", 65)} {
		t.Run("非法值拒绝 "+strconv.Quote(bad), func(t *testing.T) {
			h, seen := build()
			rec := call(h, map[string]string{protocol.HeaderRequestID: bad})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("非法键应 400，得 %d", rec.Code)
			}
			if *seen != "" {
				t.Errorf("非法键不应放行到 next，ctx 却拿了 %q", *seen)
			}
		})
	}

	t.Run("缺省现场生成", func(t *testing.T) {
		h, seen := build()
		rec := call(h, nil)
		if *seen == "" {
			t.Fatal("缺省应生成幂等键")
		}
		if !protocol.ValidRequestID(*seen) {
			t.Errorf("生成的键应合法，得 %q", *seen)
		}
		if got := rec.Header().Get(protocol.HeaderRequestID); got != *seen {
			t.Errorf("响应头应回显生成的键，得 %q｜want %q", got, *seen)
		}
	})
}
