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
// st 仅在 resp_start 时非空——直接携带流引用，避免 Do 回查 map 时被 end 抢先删除。
type result struct {
	f   Frame
	st  *stream
	err error
}

type call struct {
	ch chan result // 容量 1，deliver/fail 都用非阻塞发送
}

// errStreamSlow 表示消费端过慢导致流被中止（M2 ② 的临时策略，④ 会用信用窗口替代）。
var errStreamSlow = errors.New("agentlink: 流消费过慢")

// stream 是一条 M2 流式响应的数据通道。
//
// push 非阻塞：消费端太慢时返回 false，由 readLoop 结束该流，避免读循环被阻塞
// 而拖死同连接上的其它请求。真正的信用窗口流控在 M2 ④。
type stream struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once
	err  error
}

func newStream() *stream {
	return &stream{ch: make(chan []byte, 64), done: make(chan struct{})}
}

// push 非阻塞投递一块数据；流已结束或缓冲满返回 false。
func (s *stream) push(b []byte) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.ch <- b:
		return true
	default:
		return false
	}
}

// finish 结束流；err 非空表示异常终止（消费端会收到该错误）。
func (s *stream) finish(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.done)
	})
}

// streamBody 是流式 http.Response.Body：把 data 块交给调用方，直到 end/错误。
// onConsume 在每次真正读出 n 字节后回调，用于向 Agent 补充信用（背压窗口）。
type streamBody struct {
	s         *stream
	rem       []byte
	onConsume func(int)
}

func (b *streamBody) Read(p []byte) (int, error) {
	for {
		if len(b.rem) > 0 {
			n := copy(p, b.rem)
			b.rem = b.rem[n:]
			if b.onConsume != nil && n > 0 {
				b.onConsume(n)
			}
			return n, nil
		}
		select {
		case chunk := <-b.s.ch:
			b.rem = chunk
		case <-b.s.done:
			// 先排空缓冲，再报 EOF/错误。
			select {
			case chunk := <-b.s.ch:
				b.rem = chunk
				continue
			default:
			}
			if b.s.err != nil {
				return 0, b.s.err
			}
			return 0, io.EOF
		}
	}
}

func (b *streamBody) Close() error {
	b.s.finish(nil)
	return nil
}

// Conn 是一条已建立的反向连接（Agent 主动拨入）。
// 它实现 agentcli.Doer：agentcli 把 Link 指向它，全部请求逻辑原样复用。
type Conn struct {
	serverID int64
	ws       *websocket.Conn
	send     chan []byte

	mu      sync.Mutex
	pending map[uint64]*call
	streams map[uint64]*stream

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
		streams:  make(map[uint64]*stream),
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

// Host 返回 Agent 在 hello 帧里上报的服务器名（仅用于日志）。
func (c *Conn) Host() string { return c.host }

// HasCap 判断 Agent 是否声明了某项能力。
func (c *Conn) HasCap(cap string) bool {
	for _, v := range c.caps {
		if v == cap {
			return true
		}
	}
	return false
}

// chunkSize 是分块请求体每块的大小（远小于 MaxRespBody，避免单帧过大）。
const chunkSize = 64 << 10

// Done 在连接关闭时关闭。
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Do 实现 agentcli.Doer：把 HTTP 请求封成 req 帧，等一条 resp/err 帧。
//
// 生命周期：先登记 pending 再发送，避免响应比登记更快到（理论上可能）。
// ctx 取消时尽力发 cancel 帧，让 Agent 中止对应的 docker 调用。
func (c *Conn) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	chunked := false
	if req.Body != nil {
		defer req.Body.Close()
		// 超 1MiB 或长度未知 → 走分块（需 Agent 声明 CapChunkedReq）。
		if c.HasCap(CapChunkedReq) && (req.ContentLength > MaxReqBody || req.ContentLength < 0) {
			chunked = true
		} else {
			b, err := io.ReadAll(io.LimitReader(req.Body, MaxReqBody+1))
			if err != nil {
				return nil, fmt.Errorf("agentlink: 读请求体失败: %w", err)
			}
			if len(b) > MaxReqBody {
				return nil, &protocol.Error{Code: protocol.CodeTooLarge, Message: "请求体超过反向隧道上限"}
			}
			body = b
		}
	}

	id := c.nextID.Add(1)
	fr := Frame{
		T: TypeReq, ID: id, V: ProtocolVersion,
		M: req.Method,
		P: req.URL.RequestURI(),
		H: pickHeaders(req.Header),
		B: body,
	}
	if chunked {
		fr.S = true // 流式请求：body 由后续 data 帧补齐
	}
	raw, err := Encode(fr)
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
	if chunked {
		if err := c.sendChunked(id, req.Body); err != nil {
			c.drop(id)
			return nil, err
		}
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
		if res.f.T == TypeRespStart {
			if res.st == nil {
				return nil, ErrClosed
			}
			return startToResponse(res.f, res.st, c.creditFunc(res.f.ID)), nil
		}
		return frameToResponse(res.f), nil
	case <-req.Context().Done():
		c.drop(id)
		c.closeStream(id)
		c.enqueue(Frame{T: TypeCancel, ID: id, Msg: "调用方已取消"})
		return nil, req.Context().Err()
	case <-c.closed:
		c.drop(id)
		c.closeStream(id)
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
		case TypeRespStart:
			if c.deliverStart(f) == nil {
				// Do 已放弃（超时/取消）：建流无意义，通知 Agent 中止该流。
				c.enqueue(Frame{T: TypeCancel, ID: f.ID, Msg: "调用方已取消"})
			}
		case TypeData:
			c.mu.Lock()
			st := c.streams[f.ID]
			c.mu.Unlock()
			if st != nil && !st.push(f.B) {
				// 消费端过慢：结束该流，不拖累同连接其它请求（④ 用信用窗口改进）。
				c.removeStream(f.ID, errStreamSlow)
				c.enqueue(Frame{T: TypeCancel, ID: f.ID, Msg: "流消费过慢"})
			}
		case TypeEnd:
			c.removeStream(f.ID, nil)
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

// sendChunked 把 body 切成 data 帧发出，最后发 req_end。
// 总量超过 MaxChunkedBody 时中止并通知 Agent 取消。
func (c *Conn) sendChunked(id uint64, body io.Reader) error {
	buf := make([]byte, chunkSize)
	var total int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > MaxChunkedBody {
				c.enqueue(Frame{T: TypeCancel, ID: id, Msg: "请求体超过分块上限"})
				return &protocol.Error{Code: protocol.CodeTooLarge, Message: "请求体超过反向隧道上限"}
			}
			chunk := append([]byte(nil), buf[:n]...)
			raw, err := Encode(Frame{T: TypeData, ID: id, B: chunk})
			if err != nil {
				return err
			}
			if err := c.sendRaw(raw); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("agentlink: 读请求体失败: %w", rerr)
		}
	}
	raw, err := Encode(Frame{T: TypeReqEnd, ID: id})
	if err != nil {
		return err
	}
	return c.sendRaw(raw)
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

// deliverStart 把 resp_start 交付给等待中的 Do，并登记流。返回 nil 表示 Do 已放弃。
func (c *Conn) deliverStart(f Frame) *stream {
	st := newStream()
	c.mu.Lock()
	cl, ok := c.pending[f.ID]
	if ok {
		delete(c.pending, f.ID)
		c.streams[f.ID] = st
	}
	c.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case cl.ch <- result{f: f, st: st}:
	default:
	}
	return st
}

// removeStream 结束并移除一条流（err=nil 为正常结束）。
func (c *Conn) removeStream(id uint64, err error) {
	c.mu.Lock()
	st := c.streams[id]
	delete(c.streams, id)
	c.mu.Unlock()
	if st != nil {
		st.finish(err)
	}
}

// closeStream 在调用方取消时结束该流。
func (c *Conn) closeStream(id uint64) { c.removeStream(id, ErrClosed) }

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
		for id, st := range c.streams {
			delete(c.streams, id)
			st.finish(err)
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

// startToResponse 把 resp_start 帧 + 流 变成可流式读取的 http.Response。
// ContentLength=-1：长度未知（流式），由调用方逐块读。
func startToResponse(f Frame, st *stream, onConsume func(int)) *http.Response {
	h := make(http.Header, len(f.H))
	for k, v := range f.H {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode:    f.St,
		Status:        fmt.Sprintf("%d %s", f.St, http.StatusText(f.St)),
		Header:        h,
		Body:          &streamBody{s: st, onConsume: onConsume},
		ContentLength: -1,
	}
}

// creditFunc 返回「消费 n 字节即回一个 credit 帧」的回调：Agent 据此放行后续 data。
func (c *Conn) creditFunc(id uint64) func(int) {
	return func(n int) {
		if n > 0 {
			c.enqueue(Frame{T: TypeCredit, ID: id, Credit: int64(n)})
		}
	}
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
