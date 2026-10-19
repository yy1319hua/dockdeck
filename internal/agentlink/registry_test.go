// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentlink

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialRaw 拿一条原始 ws 连接供 Registry 使用（Registry 不负责握手）。
func dialRaw(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return ws
}

func silentServer(t *testing.T) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				return
			}
		}
	}))
	return "ws" + strings.TrimPrefix(srv.URL, "http"), srv.Close
}

func TestRegistryAttachReplace(t *testing.T) {
	url, closeFn := silentServer(t)
	defer closeFn()
	reg := NewRegistry(8)

	ws1 := dialRaw(t, url)
	c1, err := reg.Attach(7, ws1, Frame{T: TypeHello, V: 1, Host: "a"})
	if err != nil {
		t.Fatalf("Attach c1: %v", err)
	}
	if reg.Conn(7) != c1 {
		t.Fatal("第一连接未登记")
	}

	// 同 server 再连：新连接顶替，旧连接应被关闭。
	ws2 := dialRaw(t, url)
	c2, err := reg.Attach(7, ws2, Frame{T: TypeHello, V: 1, Host: "b"})
	if err != nil {
		t.Fatalf("Attach c2: %v", err)
	}
	if reg.Conn(7) != c2 {
		t.Fatal("顶号后应指向新连接")
	}
	select {
	case <-c1.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("旧连接未被关闭")
	}
	// 旧连接关闭触发的 Detach 不能误删新连接。
	time.Sleep(100 * time.Millisecond)
	if reg.Conn(7) != c2 {
		t.Fatal("Detach 误删了顶号后的新连接")
	}
	reg.CloseAll()
}

func TestRegistryLimit(t *testing.T) {
	url, closeFn := silentServer(t)
	defer closeFn()
	reg := NewRegistry(1)

	ws1 := dialRaw(t, url)
	if _, err := reg.Attach(1, ws1, Frame{T: TypeHello, V: 1}); err != nil {
		t.Fatalf("第一台应成功: %v", err)
	}
	ws2 := dialRaw(t, url)
	if _, err := reg.Attach(2, ws2, Frame{T: TypeHello, V: 1}); err != ErrTooMany {
		t.Fatalf("超过上限应返回 ErrTooMany，得 %v", err)
	}
	reg.CloseAll()
}
