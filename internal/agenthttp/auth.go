// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package agenthttp 是 Agent 侧的 HTTP 层：鉴权中间件与只读元信息端点。
//
// Agent 只做「被 Hub 调用」这一件事，所有请求都必须通过 Bearer + HMAC 双重校验；
// 没有登录页、没有会话、没有免鉴权端点。
package agenthttp

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// Hub→Agent 请求头。
const (
	// HeaderTimestamp 是 Unix 秒时间戳，参与签名并用于防重放。
	HeaderTimestamp = "X-DD-Timestamp"
	// HeaderSignature 是 HMAC-SHA256 的十六进制小写结果。
	HeaderSignature = "X-DD-Signature"
)

const (
	// MaxClockSkew 是允许的时间戳偏移。超出即判为重放或时钟异常。
	MaxClockSkew = 120 * time.Second
	// maxBodyBytes 是参与签名的请求体上限。超过直接拒绝，
	// 避免为算签名而把超大 body 整个读进内存。
	maxBodyBytes = 1 << 20
)

// errBodyTooLarge 由 readBody 返回，调用方据此回 413。
var errBodyTooLarge = errors.New("请求体过大")

// CanonicalPath 返回参与签名的路径表示。
//
// Hub 侧签名时必须调用同一个函数：签名覆盖 r.URL.RequestURI()（含查询串），
// 若两侧一个用 Path 一个用 RequestURI，所有带查询参数的请求都会验签失败。
func CanonicalPath(r *http.Request) string {
	return r.URL.RequestURI()
}

// Auth 返回 Agent 侧鉴权中间件。
//
// 校验顺序刻意固定为 Bearer → 时间戳 → 签名：
// 先挡掉没有凭证的请求，再挡掉过期请求，最后才做相对昂贵的 HMAC 计算。
// token 比较用 ConstantTimeCompare，避免时序侧信道。
//
// now 可注入，便于测试时间戳窗口；传 nil 用 time.Now。
func Auth(token string, now func() time.Time) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	secret := []byte(token)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !bearerMatches(r.Header.Get("Authorization"), token) {
				writeAgentError(w, protocol.CodeUnauthorized, "缺少或无效的 Bearer 凭证")
				return
			}

			ts := r.Header.Get(HeaderTimestamp)
			sec, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				// 时间戳缺失会让防重放失效，因此与签名错误同等对待。
				writeAgentError(w, protocol.CodeAgentSignatureInvalid, "时间戳缺失或非法")
				return
			}
			if skew := now().Sub(time.Unix(sec, 0)); skew > MaxClockSkew || skew < -MaxClockSkew {
				writeAgentError(w, protocol.CodeAgentSignatureInvalid, "时间戳超出允许窗口，请检查时钟同步")
				return
			}

			body, err := readBody(r)
			if err != nil {
				if errors.Is(err, errBodyTooLarge) {
					writeAgentError(w, protocol.CodeTooLarge, "请求体超过上限")
					return
				}
				writeAgentError(w, protocol.CodeBadRequest, "读取请求体失败")
				return
			}

			sig := r.Header.Get(HeaderSignature)
			if sig == "" {
				writeAgentError(w, protocol.CodeAgentSignatureInvalid, "缺少签名头")
				return
			}
			if !crypto.VerifyRequest(secret, ts, r.Method, CanonicalPath(r), body, sig) {
				writeAgentError(w, protocol.CodeAgentSignatureInvalid, "签名校验失败")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// bearerMatches 校验 Authorization: Bearer <token>。
func bearerMatches(header, token string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// readBody 读取并回填 body：签名需要原始字节，而后续 handler 也要能再读一次。
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	defer r.Body.Close()
	// 多读一个字节用于判断是否超限。
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

func writeAgentError(w http.ResponseWriter, code, message string) {
	httpx.WriteError(w, protocol.StatusForCode(code), code, message)
}
