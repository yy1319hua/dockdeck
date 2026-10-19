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

// runReverse 进入反向模式：拨通 Hub、握手、然后进入帧循环。
//
// M1 不做自动重连：连接断开即返回错误，由容器的 restart: unless-stopped 拉起。
// 心跳已覆盖 Cloudflare 一类的空闲超时，剩余断线基本是真实网络故障，重启是合理恢复手段。
func runReverse(cfg config.AgentConfig, h http.Handler) error {
	wsURL, err := linkURL(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()

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

	ws, _, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		return fmt.Errorf("agent: 连接 Hub 失败: %w", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)
	lw := &linkWriter{ws: ws}

	host, _ := os.Hostname()
	hello, err := agentlink.Encode(agentlink.Frame{
		T: agentlink.TypeHello, V: agentlink.ProtocolVersion,
		Caps: []string{agentlink.CapBuffered, agentlink.CapPing},
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

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go pingLoop(pingCtx, lw, cfg.LinkPingInterval)

	inflight := maxInt(cfg.LinkMaxInflight, 1)
	sem := make(chan struct{}, inflight)
	var pending sync.Map // id → context.CancelFunc

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
			select {
			case sem <- struct{}{}:
			default:
				writeErr(lw, f.ID, protocol.CodeRateLimited, "并发请求超过上限")
				continue
			}
			reqCtx, reqCancel := context.WithCancel(ctx)
			pending.Store(f.ID, reqCancel)
			go func(f agentlink.Frame, reqCtx context.Context, reqCancel context.CancelFunc) {
				defer func() {
					reqCancel()
					pending.Delete(f.ID)
					<-sem
				}()
				serveLinkRequest(reqCtx, lw, h, f)
			}(f, reqCtx, reqCancel)
		case agentlink.TypeCancel:
			if v, ok := pending.LoadAndDelete(f.ID); ok {
				v.(context.CancelFunc)()
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
func serveLinkRequest(ctx context.Context, lw *linkWriter, h http.Handler, f agentlink.Frame) {
	req, err := buildReverseRequest(ctx, f)
	if err != nil {
		writeErr(lw, f.ID, protocol.CodeBadRequest, "构造请求失败: "+err.Error())
		return
	}
	fw := &frameResponseWriter{id: f.ID, lw: lw}
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

// frameResponseWriter 把 handler 的响应收进内存，结束后一次性发 resp 帧。
// M2 流式会在 Flush 时改发 resp_start/data，届时只需实现 http.Flusher。
type frameResponseWriter struct {
	id       uint64
	lw       *linkWriter
	hdr      http.Header
	status   int
	buf      bytes.Buffer
	overflow bool
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
	if w.buf.Len()+len(p) > agentlink.MaxRespBody {
		w.overflow = true
		return 0, io.ErrShortWrite
	}
	return w.buf.Write(p)
}

func (w *frameResponseWriter) finish() {
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
