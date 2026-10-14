// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
