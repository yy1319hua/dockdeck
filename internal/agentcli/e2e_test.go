// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 写签名链路端到端测试：在本包内访问未导出的 do。
// 假 Agent 用真实 agenthttp.Auth 验签——链路测试的价值就在于连验签一起过。

package agentcli

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/fakeagent"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// TestE2E_WriteSignatureEndToEnd 验证写请求的 HMAC 头能通过真实验签中间件。
// 路径带查询串：Hub/Agent 两侧签名口径最容易劈叉的地方（S2.8 修过一次）。
func TestE2E_WriteSignatureEndToEnd(t *testing.T) {
	f := fakeagent.New("secret")
	srv := f.Server()
	t.Cleanup(srv.Close)

	var gotBody string
	srv.Config.Handler = withProbe(f.Handler(), &gotBody)

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
	f := fakeagent.New("secret")
	srv := f.Server()
	t.Cleanup(srv.Close)

	c := New(srv.URL, "wrong-token", nil)
	c.Now = func() time.Time { return time.Now() }
	_, err := c.do(context.Background(), http.MethodPost, "/api/v1/future-write", []byte(`{}`), nil)
	perr, ok := err.(*protocol.Error)
	if !ok || perr.Code != protocol.CodeUnauthorized {
		t.Fatalf("错误 token 应归 unauthorized，got %v", err)
	}
}

// withProbe 在处理器外记录原始请求体（鉴权要读 body 算签名，probe 只在鉴权后拿副本）。
// 注意顺序：probe 包在外层会把 body 消费掉，因此读完重新包 NopCloser 塞回去。
func withProbe(next http.Handler, body *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			*body = string(raw)
			r.Body = io.NopCloser(newRepeatReader(raw))
		}
		next.ServeHTTP(w, r)
	})
}

// repeatReader 让请求体可被重复读取（probe 读一次、验签再读一次）。
type repeatReader struct {
	data []byte
	pos  int
}

func newRepeatReader(b []byte) *repeatReader { return &repeatReader{data: b} }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
