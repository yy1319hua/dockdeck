// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentlink

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// LinkPath 是反向长连接的握手路径。
//
// 它不登记在 route.Table 里：这是 101 升级端点，不是普通 JSON API，
// 放进 OpenAPI 只会让生成器和一致性测试都变复杂。鉴权在 handler 内自做。
const LinkPath = "/api/v1/agent/link"

// HelloTimeout 是握手后等待 hello 帧的上限。
const HelloTimeout = 5 * time.Second

// Handler 处理 Agent 的反向连接握手。
type Handler struct {
	Store *store.Store
	Reg   *Registry
	// Disabled 为 true 时一律拒绝（回滚开关：让所有请求走原 HTTP 路径）。
	Disabled bool
	Now      func() time.Time
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Disabled || h.Reg == nil {
		http.Error(w, "reverse link disabled", http.StatusServiceUnavailable)
		return
	}
	// 查询串不参与握手；带了说明调用方拼错了，直接拒，避免签名口径歧义。
	if r.URL.RawQuery != "" {
		http.Error(w, "link 不接受查询串", http.StatusBadRequest)
		return
	}

	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "缺少 Bearer 令牌", http.StatusUnauthorized)
		return
	}
	tsRaw := r.Header.Get(agenthttp.HeaderTimestamp)
	sig := r.Header.Get(agenthttp.HeaderSignature)
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil || sig == "" {
		http.Error(w, "缺少或非法的时间戳/签名", http.StatusUnauthorized)
		return
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if d := now().Sub(time.Unix(ts, 0)); d > agenthttp.MaxClockSkew || d < -agenthttp.MaxClockSkew {
		http.Error(w, "时间戳超出允许窗口", http.StatusUnauthorized)
		return
	}

	conn, err := h.Store.ServerConnByToken(r.Context(), token)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "令牌无效", http.StatusUnauthorized)
		return
	case errors.Is(err, store.ErrServerDisabled):
		http.Error(w, "服务器已禁用", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "读取服务器信息失败", http.StatusInternalServerError)
		return
	}
	if !crypto.VerifyRequest([]byte(conn.Token), tsRaw, r.Method, r.URL.RequestURI(), nil, sig) {
		http.Error(w, "签名校验失败", http.StatusUnauthorized)
		return
	}

	// 长连接不受 http.Server 的读写超时约束（Hub 目前只设了 ReadHeaderTimeout，
	// 这里仍显式清一次 deadline，避免将来加超时时被无声切断）。
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 鉴权已由 Bearer+HMAC 完成；Agent 不是浏览器，不适用 Origin 校验。
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	ws.SetReadLimit(16 << 20)

	ctx, cancel := context.WithTimeout(r.Context(), HelloTimeout)
	typ, raw, err := ws.Read(ctx)
	cancel()
	if err != nil || typ != websocket.MessageText {
		_ = ws.Close(websocket.StatusProtocolError, "缺少 hello 帧")
		return
	}
	hello, err := Decode(raw)
	if err != nil || hello.T != TypeHello {
		_ = ws.Close(websocket.StatusProtocolError, "hello 帧非法")
		return
	}
	if hello.V != ProtocolVersion {
		_ = ws.Close(websocket.StatusProtocolError, "协议版本不兼容")
		return
	}

	lc, err := h.Reg.Attach(conn.ID, ws, hello)
	if err != nil {
		return // Attach 内部已关闭 ws
	}
	lc.enqueue(Frame{
		T: TypeHelloOK, ID: uint64(conn.ID), V: ProtocolVersion,
		Caps: []string{CapBuffered, CapStream, CapPing},
	})

	// 阻塞到连接结束：handler 返回会让底层 ResponseWriter 生命周期结束，
	// 虽然 coder/websocket 已接管连接，但显式等待更稳，也让日志时机可预期。
	<-lc.Done()
}

func bearerToken(h string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	t := strings.TrimSpace(h[len(prefix):])
	return t, t != ""
}
