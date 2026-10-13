// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 写签名链路端到端测试：在本包内访问未导出的 do。
// 假 Agent 用真实 agenthttp.Auth 验签——链路测试的价值就在于连验签一起过。

package agentcli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// writeFixture 起一个「自签 TLS + 真实 Auth 中间件 + 写回显端点」的假 Agent。
// S2 的 Agent 还没有写端点，这里只验鉴权链路——S3 有了真写端点后应迁过去。
func writeFixture(t *testing.T, token string, gotBody *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/v1/future-write", agenthttp.Auth(token, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*gotBody = string(raw)
		w.WriteHeader(http.StatusNoContent)
	})))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestE2E_WriteSignatureEndToEnd 验证写请求的 HMAC 头能通过真实验签中间件。
// 路径带查询串：Hub/Agent 两侧签名口径最容易劈叉的地方（S2.8 修过一次）。
func TestE2E_WriteSignatureEndToEnd(t *testing.T) {
	var gotBody string
	srv := writeFixture(t, "secret", &gotBody)

	c := New(srv.URL, "secret", nil)
	c.Now = func() time.Time { return time.Now() }
	if _, err := c.do(context.Background(), http.MethodPost,
		"/api/v1/future-write?dry_run=true&target=c1", []byte(`{"op":"stop"}`), nil); err != nil {
		t.Fatalf("合法签名应通过中间件：%v", err)
	}
	if gotBody != `{"op":"stop"}` {
		t.Errorf("请求体应原样到达，got %q", gotBody)
	}
}

// TestE2E_WriteWrongTokenRejected 验证错误 token 的写请求被中间件挡下，
// 错误码为 unauthorized（与签名失败可区分，审计要求两者可分辨）。
func TestE2E_WriteWrongTokenRejected(t *testing.T) {
	var gotBody string
	srv := writeFixture(t, "secret", &gotBody)

	c := New(srv.URL, "wrong-token", nil)
	c.Now = func() time.Time { return time.Now() }
	_, err := c.do(context.Background(), http.MethodPost, "/api/v1/future-write", []byte(`{}`), nil)
	perr, ok := err.(*protocol.Error)
	if !ok || perr.Code != protocol.CodeUnauthorized {
		t.Fatalf("错误 token 应归 unauthorized，got %v", err)
	}
}
