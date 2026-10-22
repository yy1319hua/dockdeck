// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 反向模式（M1）：Agent 主动连 Hub 的 WebSocket，Hub 把原本的 HTTP 请求
// 封成帧发过来。本地 mux 是唯一入口——帧会被合成为 http.Request 后
// ServeHTTP 到同一个 handler，因此路由、鉴权、错误映射与 HTTP 模式完全一致。
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/agentlink"
	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// runReverse 进入反向模式：拨通 Hub、握手、进入帧循环；断线后按指数退避重连。
//
// 与 M1 的区别：不再「断线即退出、靠容器 restart: unless-stopped 拉起」——改为
// 进程内重连（1s→2s→4s…封顶 30s，叠加 ±20% 抖动避免惊群）；单次会话持续 ≥60s
// 视为稳定并将退避重置。鉴权类永久失败（Dial 401/403）不重试，直接退出——
// 那种情况重试一万次也不会好，早退出能让运维直接看到原因。
func runReverse(cfg config.AgentConfig, h http.Handler) error {
	ctx := context.Background()
	attempt := 0
	for {
		started := time.Now()
		err := runSession(ctx, cfg, h)
		if err == nil {
			return nil
		}
		if errors.Is(err, errLinkAuth) {
			return err
		}
		if time.Since(started) >= stableSession {
			attempt = 0
		}
		delay := jitter(backoffDelay(attempt, linkRetryBase, linkRetryMax))
		log.Printf("agent: 反向连接中断，%s 后重试（第 %d 次）：%v", delay.Round(time.Millisecond), attempt+1, err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		attempt++
	}
}

// errLinkAuth 标记鉴权类永久失败：runReverse 见到它不重试、直接退出。
var errLinkAuth = errors.New("agent: 反向连接鉴权失败")

// errCreditWait 表示流式发送等待信用超时（消费端长时间不读）。
var errCreditWait = errors.New("agent: 流式发送等待信用超时")

// chunkedReq 累积一次分块请求（req S=true → data… → req_end 才派发）。
type chunkedReq struct {
	f   agentlink.Frame
	buf bytes.Buffer
}

// runSession 跑一次完整会话（拨号 → hello 握手 → 帧循环），返回即表示本次连接结束。
func runSession(ctx context.Context, cfg config.AgentConfig, h http.Handler) error {
	wsURL, err := linkURL(cfg)
	if err != nil {
		return err
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := crypto.SignRequest([]byte(cfg.Token), ts, http.MethodGet, agentlink.LinkPath, nil)

	opts := &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization":           {"Bearer " + cfg.Token},
		agenthttp.HeaderTimestamp: {ts},
		agenthttp.HeaderSignature: {sig},
	}}
	if cfg.EnrollInsecure {
		// 与 enroll 保持同一安全口径：Hub 用自签证书时默认跳过校验。
		opts.HTTPClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Hub 自签场景
		}}
	}

	ws, resp, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("%w: %v", errLinkAuth, err)
		}
		return fmt.Errorf("agent: 连接 Hub 失败: %w", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)
	lw := &linkWriter{ws: ws}

	host, _ := os.Hostname()
	hello, err := agentlink.Encode(agentlink.Frame{
		T: agentlink.TypeHello, V: agentlink.ProtocolVersion,
		Caps: []string{agentlink.CapBuffered, agentlink.CapStream, agentlink.CapChunkedReq, agentlink.CapPing},
		Host: host,
	})
	if err != nil {
		return err
	}
	if err := lw.write(ctx, websocket.MessageText, hello); err != nil {
		return fmt.Errorf("agent: 发送 hello 失败: %w", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, agentlink.HelloTimeout)
	typ, raw, err := ws.Read(readCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("agent: 等待 hello_ok 失败: %w", err)
	}
	if typ != websocket.MessageText {
		return errors.New("agent: hello_ok 不是文本帧")
	}
	ok, err := agentlink.Decode(raw)
	if err != nil || ok.T != agentlink.TypeHelloOK {
		return fmt.Errorf("agent: hello_ok 非法: %v", err)
	}
	log.Printf("agent: 反向连接已建立（server_id=%d, hub=%s）", ok.ID, cfg.HubURL)

	// 流式仅在 Hub 明确声明 CapStream 时启用；旧 Hub 未声明 → 退回整包 resp（M1 兼容）。
	streamOK := ok.HasCap(agentlink.CapStream)

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go pingLoop(pingCtx, lw, cfg.LinkPingInterval)

	inflight := maxInt(cfg.LinkMaxInflight, 1)
	sem := make(chan struct{}, inflight)
	var pending sync.Map // id → context.CancelFunc
	var credits sync.Map // id → *credit（流式响应背压窗口）

	// dispatchRequest 派发一个完整请求（含分块收齐后的）。
	dispatchRequest := func(f agentlink.Frame) {
		select {
		case sem <- struct{}{}:
		default:
			writeErr(lw, f.ID, protocol.CodeRateLimited, "并发请求超过上限")
			return
		}
		reqCtx, reqCancel := context.WithCancel(ctx)
		pending.Store(f.ID, reqCancel)
		cr := newCredit(agentlink.CreditWindow)
		credits.Store(f.ID, cr)
		go func(f agentlink.Frame, reqCtx context.Context, reqCancel context.CancelFunc, cr *credit) {
			defer func() {
				reqCancel()
				pending.Delete(f.ID)
				credits.Delete(f.ID)
				cr.close()
				<-sem
			}()
			serveLinkRequest(reqCtx, lw, h, f, streamOK, cr)
		}(f, reqCtx, reqCancel, cr)
	}
	// bulk 累积分块请求（req S=true → data… → req_end 才派发）。
	bulk := make(map[uint64]*chunkedReq)

	for {
		typ, raw, err := ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("agent: 反向连接中断: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		f, err := agentlink.Decode(raw)
		if err != nil {
			continue
		}
		switch f.T {
		case agentlink.TypeReq:
			if f.S {
				// 分块请求：先收齐 body 再派发。
				bulk[f.ID] = &chunkedReq{f: f}
				continue
			}
			dispatchRequest(f)
		case agentlink.TypeData:
			if cr, ok := bulk[f.ID]; ok {
				if cr.buf.Len()+len(f.B) > agentlink.MaxChunkedBody {
					delete(bulk, f.ID)
					writeErr(lw, f.ID, protocol.CodeTooLarge, "请求体超过分块上限")
					continue
				}
				cr.buf.Write(f.B)
			}
		case agentlink.TypeReqEnd:
			if cr, ok := bulk[f.ID]; ok {
				delete(bulk, f.ID)
				f2 := cr.f
				f2.B = cr.buf.Bytes()
				f2.S = false
				dispatchRequest(f2)
			}
		case agentlink.TypeCancel:
			if v, ok := pending.LoadAndDelete(f.ID); ok {
				v.(context.CancelFunc)()
			}
			if v, ok := credits.Load(f.ID); ok {
				v.(*credit).close()
			}
		case agentlink.TypeCredit:
			if v, ok := credits.Load(f.ID); ok {
				v.(*credit).add(f.Credit)
			}
		case agentlink.TypePing:
			pong, _ := agentlink.Encode(agentlink.Frame{T: agentlink.TypePong})
			_ = lw.write(ctx, websocket.MessageText, pong)
		case agentlink.TypePong:
			// 心跳应答，无需处理
		case agentlink.TypeGoAway:
			return fmt.Errorf("agent: Hub 要求断开连接: %s", f.Msg)
		}
	}
}

// serveLinkRequest 把一帧 req 变成真实 HTTP 调用。
// 必须走 mux（h.ServeHTTP）而不是直接调 handler：PathValue 由 ServeMux 填充。
func serveLinkRequest(ctx context.Context, lw *linkWriter, h http.Handler, f agentlink.Frame, streamOK bool, cr *credit) {
	req, err := buildReverseRequest(ctx, f)
	if err != nil {
		writeErr(lw, f.ID, protocol.CodeBadRequest, "构造请求失败: "+err.Error())
		return
	}
	fw := &frameResponseWriter{id: f.ID, lw: lw, stream: streamOK, credit: cr}
	h.ServeHTTP(fw, req)
	fw.finish()
}

func buildReverseRequest(ctx context.Context, f agentlink.Frame) (*http.Request, error) {
	var body io.Reader
	if len(f.B) > 0 {
		body = bytes.NewReader(f.B)
	}
	req, err := http.NewRequestWithContext(ctx, f.M, f.P, body)
	if err != nil {
		return nil, err
	}
	if len(f.B) > 0 {
		req.ContentLength = int64(len(f.B))
	}
	for k, v := range f.H {
		req.Header.Set(k, v)
	}
	// 不设置 RequestURI：agenthttp 用 URL.RequestURI() 重建签名口径，
	// 与 Hub 侧被封装的原始 RequestURI 完全一致。
	return req, nil
}

// frameResponseWriter 把 handler 的响应收进内存，结束后一次性发 resp 帧；
// 协商到 CapStream 且 handler 调用 Flush 时，改发 resp_start / data / end 流式帧。
type frameResponseWriter struct {
	id       uint64
	lw       *linkWriter
	stream   bool // 是否协商到 CapStream
	hdr      http.Header
	status   int
	buf      bytes.Buffer
	overflow bool
	started  bool    // 已发 resp_start，进入流式
	werr     error   // 流式写失败后短路，避免继续往断连写帧
	credit   *credit // 流式发送信用（背压窗口），nil 表示不启用
}

func (w *frameResponseWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = make(http.Header)
	}
	return w.hdr
}

func (w *frameResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *frameResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	// 流式模式没有 8MiB 整包上限（逐块经 Flush 发出）；非流式沿用旧上限。
	if !w.stream && w.buf.Len()+len(p) > agentlink.MaxRespBody {
		w.overflow = true
		return 0, io.ErrShortWrite
	}
	return w.buf.Write(p)
}

// Flush 实现 http.Flusher：首次调用发 resp_start（状态码+头），随后每次把缓冲
// 作为 data 帧发出。仅在协商到 CapStream 时生效；否则是空操作（M1 兼容）。
func (w *frameResponseWriter) Flush() {
	if !w.stream || w.werr != nil {
		return
	}
	if !w.started {
		if err := w.writeFrame(agentlink.Frame{
			T: agentlink.TypeRespStart, ID: w.id, St: w.statusOrOK(), H: w.headerMap(),
		}); err != nil {
			w.werr = err
			return
		}
		w.started = true
	}
	if w.buf.Len() > 0 {
		// 复制：buf 随后 Reset，底层数组会被后续 Write 复用。
		chunk := append([]byte(nil), w.buf.Bytes()...)
		// 背压：每块 data 先扣信用；不足则有界等待（超时即结束流）。
		if w.credit != nil && !w.credit.acquire(int64(len(chunk)), creditWait) {
			w.werr = errCreditWait
			return
		}
		if err := w.writeFrame(agentlink.Frame{T: agentlink.TypeData, ID: w.id, B: chunk}); err != nil {
			w.werr = err
			return
		}
		w.buf.Reset()
	}
}

func (w *frameResponseWriter) statusOrOK() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *frameResponseWriter) headerMap() map[string]string {
	if len(w.hdr) == 0 {
		return nil
	}
	out := make(map[string]string, len(w.hdr))
	for k, vs := range w.hdr {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

func (w *frameResponseWriter) writeFrame(f agentlink.Frame) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := agentlink.Encode(f)
	if err != nil {
		return err
	}
	return w.lw.write(ctx, websocket.MessageText, raw)
}

func (w *frameResponseWriter) finish() {
	if w.werr != nil {
		return // 流式写已失败（连接多半已断），不再尝试
	}
	// 流式：已发 resp_start 时，把剩余缓冲作为 data 发出，再发 end。
	if w.stream && w.started {
		if w.buf.Len() > 0 {
			chunk := append([]byte(nil), w.buf.Bytes()...)
			if w.credit != nil && !w.credit.acquire(int64(len(chunk)), creditWait) {
				return
			}
			if err := w.writeFrame(agentlink.Frame{T: agentlink.TypeData, ID: w.id, B: chunk}); err != nil {
				return
			}
			w.buf.Reset()
		}
		_ = w.writeFrame(agentlink.Frame{T: agentlink.TypeEnd, ID: w.id})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if w.overflow {
		writeErrCtx(ctx, w.lw, w.id, protocol.CodeTooLarge, "响应体超过反向隧道上限")
		return
	}
	st := w.status
	if st == 0 {
		st = http.StatusOK
	}
	f := agentlink.Frame{T: agentlink.TypeResp, ID: w.id, St: st, B: w.buf.Bytes()}
	if len(w.hdr) > 0 {
		f.H = make(map[string]string, len(w.hdr))
		for k, vs := range w.hdr {
			if len(vs) > 0 {
				f.H[k] = vs[0]
			}
		}
	}
	raw, err := agentlink.Encode(f)
	if err != nil {
		writeErrCtx(ctx, w.lw, w.id, protocol.CodeInternal, err.Error())
		return
	}
	_ = w.lw.write(ctx, websocket.MessageText, raw)
}

// linkWriter 串行化写：coder/websocket 虽允许并发写，但显式加锁让帧顺序可预期。
type linkWriter struct {
	mu sync.Mutex
	ws *websocket.Conn
}

func (w *linkWriter) write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ws.Write(ctx, typ, data)
}

func writeErr(lw *linkWriter, id uint64, code, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	writeErrCtx(ctx, lw, id, code, msg)
}

func writeErrCtx(ctx context.Context, lw *linkWriter, id uint64, code, msg string) {
	raw, err := agentlink.Encode(agentlink.Frame{T: agentlink.TypeErr, ID: id, Code: code, Msg: msg})
	if err != nil {
		return
	}
	_ = lw.write(ctx, websocket.MessageText, raw)
}

func pingLoop(ctx context.Context, lw *linkWriter, interval time.Duration) {
	if interval <= 0 {
		interval = 25 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			raw, err := agentlink.Encode(agentlink.Frame{T: agentlink.TypePing})
			if err != nil {
				continue
			}
			if err := lw.write(ctx, websocket.MessageText, raw); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func linkURL(cfg config.AgentConfig) (string, error) {
	u, err := url.Parse(strings.TrimRight(cfg.HubURL, "/"))
	if err != nil {
		return "", fmt.Errorf("agent: Hub 地址非法: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("agent: Hub 地址必须是 http(s)://host，得到 %q", cfg.HubURL)
	}
	u.Path = agentlink.LinkPath
	u.RawQuery = ""
	return u.String(), nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
