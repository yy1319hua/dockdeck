// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/agentlink"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// TestBuildReverseRequest 守的是「签名口径」：合成请求的 RequestURI
// 必须与 Hub 侧被封装的原始 RequestURI 逐字一致，否则 agenthttp 验签必失败。
func TestBuildReverseRequest(t *testing.T) {
	f := agentlink.Frame{
		T: agentlink.TypeReq,
		M: "GET",
		P: "/api/v1/containers?all=1&x=a%2Fb",
		H: map[string]string{
			"Authorization":          "Bearer tok",
			"X-DD-Timestamp":         "1790000000",
			"X-DD-Signature":         "abc",
			protocol.HeaderRequestID: "req-1",
		},
		B: []byte("body-bytes"),
	}
	req, err := buildReverseRequest(context.Background(), f)
	if err != nil {
		t.Fatalf("buildReverseRequest: %v", err)
	}
	if req.Method != f.M {
		t.Errorf("method = %q", req.Method)
	}
	if got := req.URL.RequestURI(); got != f.P {
		t.Errorf("RequestURI = %q，want %q（签名口径必须一致）", got, f.P)
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("Authorization 头未透传")
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("读 body: %v", err)
	}
	if !bytes.Equal(b, f.B) {
		t.Errorf("body = %q", b)
	}
	if req.ContentLength != int64(len(f.B)) {
		t.Errorf("ContentLength = %d", req.ContentLength)
	}
}

// TestFrameResponseWriter 守状态码/头/正文捕获与 8 MiB 上限。
func TestFrameResponseWriter(t *testing.T) {
	w := &frameResponseWriter{}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	if n, err := w.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if w.status != 201 {
		t.Errorf("status = %d", w.status)
	}
	if w.buf.String() != "hello" {
		t.Errorf("buf = %q", w.buf.String())
	}

	// 超限：不截断、标记 overflow，由 finish 转成 too_large 错误帧。
	over := &frameResponseWriter{}
	n, err := over.Write(bytes.Repeat([]byte("a"), agentlink.MaxRespBody+1))
	if err == nil || n != 0 {
		t.Errorf("超限应返回 0, error，得 %d, %v", n, err)
	}
	if !over.overflow {
		t.Error("应标记 overflow")
	}
}
