// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDoStreamBodyReadableAfterReturn 是 GAP-8 的回归测试。
//
// 原实现用 defer cancel()：doStream 一返回 ctx 就被取消，而流式响应
// 是由调用方随后读的，于是 Body 读到 0 字节（node1 上容器日志 tail=N 全空）。
// 这里让假 Engine 分块、带延迟地吐数据：若 cancel 仍提前触发，ReadAll
// 会返回 context canceled，测试即红。
func TestDoStreamBodyReadableAfterReturn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, chunk := range []string{"line1\n", "line2\n", "line3\n"} {
			_, _ = io.WriteString(w, chunk)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	c := &Client{
		HTTP: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", addr)
			},
		}},
		RequestTimeout: 2 * time.Second,
	}

	resp, err := c.doStream(context.Background(), http.MethodGet, "/containers/abc/logs?tail=3", false)
	if err != nil {
		t.Fatalf("doStream: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil {
		t.Fatalf("doStream 返回后读 Body 失败（ctx 被提前取消？）：%v", err)
	}
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	if got, want := string(body), "line1\nline2\nline3\n"; got != want {
		t.Errorf("Body = %q，want %q", got, want)
	}
}
