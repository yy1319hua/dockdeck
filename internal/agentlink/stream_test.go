// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentlink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestStreamBodyReadAndEOF(t *testing.T) {
	st := newStream()
	body := &streamBody{s: st}
	st.push([]byte("ab"))
	st.push([]byte("cd"))
	st.finish(nil)

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "abcd" {
		t.Errorf("body = %q，want abcd", got)
	}
}

func TestStreamBodyError(t *testing.T) {
	st := newStream()
	body := &streamBody{s: st}
	st.push([]byte("x"))
	st.finish(errStreamSlow)

	_, err := io.ReadAll(body)
	if !errors.Is(err, errStreamSlow) {
		t.Errorf("err = %v，want errStreamSlow", err)
	}
}

// TestConnStreamingRoundTrip 验证 Hub 侧消费 resp_start/data/end：Do 返回可流式
// 读取的 Response，Body 依次读到 data 块，end 后 EOF。
func TestConnStreamingRoundTrip(t *testing.T) {
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
			f, err := Decode(raw)
			if err != nil || f.T != TypeReq {
				continue
			}
			send := func(fr Frame) {
				out, _ := Encode(fr)
				_ = ws.Write(context.Background(), websocket.MessageText, out)
			}
			send(Frame{T: TypeRespStart, ID: f.ID, St: 200, H: map[string]string{"Content-Type": "text/plain"}})
			send(Frame{T: TypeData, ID: f.ID, B: []byte("hello ")})
			send(Frame{T: TypeData, ID: f.ID, B: []byte("world")})
			send(Frame{T: TypeEnd, ID: f.ID})
		}
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := newConn(1, ws, Frame{T: TypeHello, V: 1, Caps: []string{CapBuffered, CapStream}})
	defer c.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://docker/api/v1/containers/x/logs?follow=1", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d，want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q", ct)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(b) != "hello world" {
		t.Errorf("body = %q，want %q", string(b), "hello world")
	}
}

// TestConnChunkedRequest 验证 >1MiB 请求体走 req(S=true)+data…+req_end，且对端能重组。
func TestConnChunkedRequest(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(32 << 20) // 分块帧 64KB，默认 32KB 上限会拒收
		var buf bytes.Buffer
		for {
			typ, raw, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			f, err := Decode(raw)
			if err != nil {
				continue
			}
			switch f.T {
			case TypeReq:
				buf.Reset()
				buf.Write(f.B)
			case TypeData:
				buf.Write(f.B)
			case TypeReqEnd:
				got <- buf.String()
				out, _ := Encode(Frame{T: TypeResp, ID: f.ID, St: 200})
				_ = ws.Write(context.Background(), websocket.MessageText, out)
			}
		}
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(32 << 20)
	c := newConn(1, ws, Frame{T: TypeHello, V: 1, Caps: []string{CapBuffered, CapChunkedReq}})
	defer c.Close()

	payload := strings.Repeat("x", MaxReqBody+123) // 明确超过 1MiB
	req, _ := http.NewRequest(http.MethodPost, "http://docker/upload", strings.NewReader(payload))
	if req.ContentLength <= MaxReqBody {
		t.Fatalf("测试前提不成立：ContentLength=%d", req.ContentLength)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	select {
	case b := <-got:
		if b != payload {
			t.Errorf("重组长度 %d，want %d", len(b), len(payload))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到 req_end")
	}
}
