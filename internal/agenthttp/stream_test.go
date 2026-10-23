// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agenthttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/yy1319hua/dockdeck/internal/agentlink"
)

// TestFrameResponseWriterStreams 验证 Agent 侧：协商到 CapStream 后，每次 Flush
// 发 resp_start/data，finish 发 end；顺序与内容正确。
func TestFrameResponseWriterStreams(t *testing.T) {
	frames := make(chan agentlink.Frame, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			typ, raw, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			f, err := agentlink.Decode(raw)
			if err != nil {
				continue
			}
			frames <- f
		}
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()
	lw := &linkWriter{ws: ws}

	fw := &frameResponseWriter{id: 7, lw: lw, stream: true}
	fw.Header().Set("Content-Type", "text/plain")
	fw.WriteHeader(http.StatusOK)
	_, _ = fw.Write([]byte("a"))
	fw.Flush()
	_, _ = fw.Write([]byte("b"))
	fw.Flush()
	fw.finish()

	want := []struct {
		t    agentlink.Type
		body string
	}{
		{agentlink.TypeRespStart, ""},
		{agentlink.TypeData, "a"},
		{agentlink.TypeData, "b"},
		{agentlink.TypeEnd, ""},
	}
	for i, w := range want {
		select {
		case f := <-frames:
			if f.ID != 7 {
				t.Errorf("第 %d 帧 ID = %d，want 7", i, f.ID)
			}
			if f.T != w.t || string(f.B) != w.body {
				t.Fatalf("第 %d 帧 = (%s,%q)，want (%s,%q)", i, f.T, string(f.B), w.t, w.body)
			}
			if w.t == agentlink.TypeRespStart && f.St != http.StatusOK {
				t.Errorf("resp_start 状态 = %d，want 200", f.St)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("等第 %d 帧超时", i)
		}
	}
}
