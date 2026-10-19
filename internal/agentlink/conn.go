// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentlink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

var (
	// ErrClosed 表示连接已关闭（正常关闭或对端断开）。
	ErrClosed = errors.New("agentlink: 连接已关闭")
	// ErrReplaced 表示本连接被同一服务器的新连接顶替。
	ErrReplaced = errors.New("agentlink: 连接已被新连接替换")
	// ErrTooMany 表示 Hub 的反向连接数已达上限。
	ErrTooMany = errors.New("agentlink: 反向连接数已达上限")
)

const (
	// writeTimeout 是单帧写超时：写不出去就不无限积压，宁可断连。
	writeTimeout = 10 * time.Second
	// readIdleTimeout 是读空闲上限。Agent 每 25s 发一次 ping，
	// 这里留 3 倍余量；超时即判死，避免僵尸连接挂在 registry 里。
	readIdleTimeout = 90 * time.Second
)

// result 是单次请求的终态：要么有帧，要么有错误。
type result struct {
	f   Frame
	err error
}

type call struct {
	ch chan result // 容量 1，deliver/fail 都用非阻塞发送
}

// Conn 是一条已建立的反向连接（Agent 主动拨入）。
// 它实现 agentcli.Doer：agentcli 把 Link 指向它，全部请求逻辑原样复用。
type Conn struct {
	serverID int64
	ws       *websocket.Conn
	send     chan []byte

	mu      sync.Mutex
	pending map[uint64]*call

	nextID    atomic.Uint64
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error

	caps []string
	host string
}

func newConn(serverID int64, ws *websocket.Conn, hello Frame) *Conn {
	c := &Conn{
		serverID: serverID,
		ws:       ws,
		send:     make(chan []byte, 256),
		pending:  make(map[uint64]*call),
		closed:   make(chan struct{}),
		caps:     hello.Caps,
		host:     hello.Host,
	}
	go c.readLoop()
	go c.writeLoop()
	return c
}

// ServerID 返回该连接对应的服务器 ID。
func (c *Conn) ServerID() int64 { return c.serverID }

// Host 返回 Agent 在 hello 帧里上报的主机名（仅用于日志）。
func (c *Conn) Host() string { return c.host }

// Done 在连接关闭时关闭。
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Do 实现 agentcli.Doer：把 HTTP 请求封成 req 帧，等一条 resp/err 帧。
//
// 生命周期：先登记 pending 再发送，避免响应比登记更快到（理论上可能）。
// ctx 取消时尽力发 cancel 帧，让 Agent 中止对应的 docker 调用。
func (c *Conn) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		defer req.Body.Close()
		b, err := io.ReadAll(io.LimitReader(req.Body, MaxReqBody+1))
		if err != nil {
			return nil, fmt.Errorf("agentlink: 读请求体失败: %w", err)
		}
		if len(b) > MaxReqBody {
			return nil, &protocol.Error{Code: protocol.CodeTooLarge, Message: "请求体超过反向隧道上限"}
		}
		body = b
	}

	id := c.nextID.Add(1)
	raw, err := Encode(Frame{
		T: TypeReq, ID: id, V: ProtocolVersion,
		M: req.Method,
		P: req.URL.RequestURI(),
		H: pickHeaders(req.Header),
		B: body,
	})
	if err != nil {
		return nil, err
	}

	cl := &call{ch: make(chan result, 1)}
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return nil, c.err()
	default:
	}
	c.pending[id] = cl
	c.mu.Unlock()

	if err := c.sendRaw(raw); err != nil {
		c.drop(id)
		return nil, err
	}

	select {
	case res := <-cl.ch:
		if res.err != nil {
			return nil, res.err
		}
		if res.f.T == TypeErr {
			code := res.f.Code
			if code == "" {
				code = protocol.CodeUpstreamError
			}
			return nil, &protocol.Error{Code: code, Message: res.f.Msg}
		}
		return frameToResponse(res.f), nil
	case <-req.Context().Done():
		c.drop(id)
		c.enqueue(Frame{T: TypeCancel, ID: id, Msg: "调用方已取消"})
		return nil, req.Context().Err()
	case <-c.closed:
		c.drop(id)
		return nil, c.err()
	}
}

// Close 关闭连接并让所有等待中的请求失败。
func (c *Conn) Close() { c.fail(ErrClosed) }

// GoAway 尽力把「被顶替」告知对端，再延迟关闭——
// 立即 Close 会让 goaway 帧没机会写出去。
func (c *Conn) GoAway(code, reason string) {
	c.enqueue(Frame{T: TypeGoAway, Code: code, Msg: reason})
	time.AfterFunc(300*time.Millisecond, c.Close)
}

func (c *Conn) readLoop() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), readIdleTimeout)
		typ, raw, err := c.ws.Read(ctx)
		cancel()
		if err != nil {
			c.fail(fmt.Errorf("agentlink: 连接读失败: %w", err))
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		f, err := Decode(raw)
		if err != nil {
			continue // 单帧坏了不该拖垮整条连接
		}
		switch f.T {
		case TypeResp, TypeErr:
			c.deliver(f)
		case TypePing:
			c.enqueue(Frame{T: TypePong})
		case TypePong:
			// 心跳应答；读循环本身存活已证明连接可用
		case TypeGoAway:
			c.fail(ErrReplaced)
			return
		case TypeHello:
			// 重复 hello：已握手，忽略
		default:
			c.enqueue(Frame{T: TypeErr, ID: f.ID, Code: protocol.CodeBadRequest,
				Msg: "M1 不支持的帧类型: " + string(f.T)})
		}
	}
}

func (c *Conn) writeLoop() {
	for {
		select {
		case raw := <-c.send:
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, raw)
			cancel()
			if err != nil {
				c.fail(fmt.Errorf("agentlink: 连接写失败: %w", err))
				return
			}
		case <-c.closed:
			return
		}
	}
}

func (c *Conn) enqueue(f Frame) {
	raw, err := Encode(f)
	if err != nil {
		return
	}
	select {
	case c.send <- raw:
	case <-c.closed:
	}
}

func (c *Conn) sendRaw(raw []byte) error {
	select {
	case c.send <- raw:
		return nil
	case <-c.closed:
		return c.err()
	}
}

func (c *Conn) deliver(f Frame) {
	c.mu.Lock()
	cl, ok := c.pending[f.ID]
	if ok {
		delete(c.pending, f.ID)
	}
	c.mu.Unlock()
	if !ok {
		return // 迟到帧（调用方已超时/取消）直接丢弃
	}
	select {
	case cl.ch <- result{f: f}:
	default:
	}
}

func (c *Conn) drop(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) fail(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		close(c.closed)
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
		c.mu.Lock()
		for id, cl := range c.pending {
			delete(c.pending, id)
			select {
			case cl.ch <- result{err: err}:
			default:
			}
		}
		c.mu.Unlock()
	})
}

func (c *Conn) err() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	return ErrClosed
}

// pickHeaders 只转发签名与内容相关头，绝不透传 hop-by-hop 或客户端身份头。
func pickHeaders(h http.Header) map[string]string {
	out := make(map[string]string, 5)
	for _, k := range []string{
		"Authorization",
		"X-DD-Timestamp",
		"X-DD-Signature",
		"Content-Type",
		protocol.HeaderRequestID,
	} {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func frameToResponse(f Frame) *http.Response {
	h := make(http.Header, len(f.H))
	for k, v := range f.H {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode:    f.St,
		Status:        fmt.Sprintf("%d %s", f.St, http.StatusText(f.St)),
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(f.B)),
		ContentLength: int64(len(f.B)),
	}
}
