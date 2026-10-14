// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentcli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// TestClient_LogsLocalRejects 验证本地校验先行：非法 ID、非法 tail、
// 非法 since/until 在本地就拒，请求不出门（与 Agent 侧同款规则）。
func TestClient_LogsLocalRejects(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := quickClient(srv)

	calls := []struct {
		name string
		fn   func() error
	}{
		{"bad-id", func() error {
			_, err := c.Logs(context.Background(), "..%2Fetc", protocol.LogsOptions{})
			return err
		}},
		{"bad-tail", func() error {
			_, err := c.Logs(context.Background(), "c1", protocol.LogsOptions{Tail: "10;rm"})
			return err
		}},
		{"bad-tail-negative", func() error {
			_, err := c.Logs(context.Background(), "c1", protocol.LogsOptions{Tail: "-5"})
			return err
		}},
		{"bad-since", func() error {
			_, err := c.Logs(context.Background(), "c1", protocol.LogsOptions{Since: "2026-13-01T00:00:00Z"})
			return err
		}},
		{"bad-until", func() error {
			_, err := c.Logs(context.Background(), "c1", protocol.LogsOptions{Until: "yesterday"})
			return err
		}},
	}
	for _, ck := range calls {
		if err := ck.fn(); err == nil {
			t.Errorf("%s 应被本地拒绝", ck.name)
		}
	}
	if hit {
		t.Error("本地校验失败的请求不应发出任何 HTTP 请求")
	}
}

// TestClient_LogsQueryShape 验证查询串形状：参数经 url.Values 编码
// （since 的 "+08:00" 必须转义，否则会被解析成空格丢了时区），
// 签名路径与 Agent 收到的字字一致。
func TestClient_LogsQueryShape(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := quickClient(srv).Logs(context.Background(), "c1", protocol.LogsOptions{
		Follow: true, Tail: "100", Since: "2024-01-01T00:00:00+08:00",
		Until: "2024-01-02T00:00:00Z", Timestamps: true, Export: true,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	_ = s.Body.Close()
	want := "/api/v1/containers/c1/logs?export=1&follow=1&since=2024-01-01T00%3A00%3A00%2B08%3A00&tail=100&timestamps=1&until=2024-01-02T00%3A00%3A00Z"
	if gotPath != want {
		t.Errorf("查询串形状不符：\ngot  %s\nwant %s", gotPath, want)
	}
}

// TestClient_LogsRawPassthrough 验证裸流原样透传：一个字节不改，
// 内容类型与 export 文件名从 Agent 响应头带出。
func TestClient_LogsRawPassthrough(t *testing.T) {
	raw := "line1\nline2\nbinary\x00bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="c1.log"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	s, err := quickClient(srv).Logs(context.Background(), "c1", protocol.LogsOptions{Export: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer s.Body.Close()
	got, err := io.ReadAll(s.Body)
	if err != nil {
		t.Fatalf("读流失败：%v", err)
	}
	if string(got) != raw {
		t.Errorf("裸流应原样透传：\ngot  %q\nwant %q", got, raw)
	}
	if s.ContentType != "application/octet-stream" || s.Filename != "c1.log" {
		t.Errorf("响应头信息不符：%+v", s)
	}
}

// TestClient_LogsEnvelopeError 验证流开启前的失败按信封解读
// （容器不存在 → not_found 原样透传），且 body 被关掉不泄漏。
func TestClient_LogsEnvelopeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"容器不存在"}}`))
	}))
	defer srv.Close()

	s, err := quickClient(srv).Logs(context.Background(), "c1", protocol.LogsOptions{})
	if s != nil {
		t.Error("失败时不应返回流")
	}
	perr, ok := err.(*protocol.Error)
	if !ok || perr.Code != protocol.CodeNotFound {
		t.Fatalf("应透传 not_found，got %v", err)
	}
}
