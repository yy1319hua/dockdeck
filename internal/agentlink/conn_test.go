// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// echoLinkServer 起一个最小 Agent 对端：req → resp（body 回显 path），cancel 记到 channel。
func echoLinkServer(t *testing.T, slow bool) (string, <-chan uint64, func()) {
	t.Helper()
	cancels := make(chan uint64, 32)
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
			if err != nil {
				continue
			}
			switch f.T {
			case TypeReq:
				if slow {
					continue // 故意不回，供超时用例
				}
				out, _ := Encode(Frame{T: TypeResp, ID: f.ID, St: 200, B: []byte(f.P)})
				_ = ws.Write(context.Background(), websocket.MessageText, out)
			case TypeCancel:
				select {
				case cancels <- f.ID:
				default:
				}
			case TypePing:
				out, _ := Encode(Frame{T: TypePong})
				_ = ws.Write(context.Background(), websocket.MessageText, out)
			}
		}
	}))
	return "ws" + strings.TrimPrefix(srv.URL, "http"), cancels, srv.Close
}

func dialConn(t *testing.T, url string) *Conn {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return newConn(1, ws, Frame{T: TypeHello, V: 1, Host: "test", Caps: []string{CapBuffered}})
}

func TestConnDoRoundTrip(t *testing.T) {
	url, _, closeFn := echoLinkServer(t, false)
	defer closeFn()
	c := dialConn(t, url)
	defer c.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://docker/api/v1/containers?all=1", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "/api/v1/containers?all=1" {
		t.Errorf("body = %q", b)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// TestConnConcurrent 验证一条连接上多路复用：50 个请求并发，各自拿到自己的响应。
func TestConnConcurrent(t *testing.T) {
	url, _, closeFn := echoLinkServer(t, false)
	defer closeFn()
	c := dialConn(t, url)
	defer c.Close()

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := fmt.Sprintf("/api/v1/x/%d?i=%d", i, i)
			req, _ := http.NewRequest(http.MethodGet, "http://docker"+p, nil)
			resp, err := c.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if string(b) != p {
				errCh <- fmt.Errorf("第 %d 个请求串了响应: %q", i, b)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestConnTimeoutSendsCancel 验证 ctx 超时后：Do 返回错误，且尽力发出 cancel 帧。
func TestConnTimeoutSendsCancel(t *testing.T) {
	url, cancels, closeFn := echoLinkServer(t, true)
	defer closeFn()
	c := dialConn(t, url)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/slow", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("应返回超时错误")
	}
	select {
	case <-cancels:
	case <-time.After(2 * time.Second):
		t.Fatal("超时后未发出 cancel 帧")
	}
}

// TestConnCloseFailsPending 验证连接关闭后，新请求立即失败而不是挂死。
func TestConnCloseFailsPending(t *testing.T) {
	url, _, closeFn := echoLinkServer(t, false)
	defer closeFn()
	c := dialConn(t, url)
	c.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://docker/x", nil)
	if _, err := c.Do(req); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v，期望 ErrClosed", err)
	}
}

// TestConnPingPong 验证心跳帧不会打断正常请求。
func TestConnPingPong(t *testing.T) {
	url, _, closeFn := echoLinkServer(t, false)
	defer closeFn()
	c := dialConn(t, url)
	defer c.Close()

	c.enqueue(Frame{T: TypePing})
	time.Sleep(50 * time.Millisecond)

	req, _ := http.NewRequest(http.MethodGet, "http://docker/alive", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("心跳后请求失败: %v", err)
	}
	resp.Body.Close()
}
