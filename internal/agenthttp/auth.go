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
	// DefaultMaxBodyBytes 是参与签名的请求体默认上限（1 MiB）。
	//
	// 验签需要整个 body 的原始字节，所以这个上限同时就是「单个请求最大内存占用」。
	// 默认值覆盖除文件上传外的所有端点；文件上传用 AuthN 显式提高（见 cmd/agent），
	// 且不得超过 protocol.MaxFileUploadBytes——这是刻意的取舍：
	// 无上限等于给 Agent 装了个内存放大器。
	DefaultMaxBodyBytes = 1 << 20
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

// Auth 返回 Agent 侧鉴权中间件（请求体上限为 DefaultMaxBodyBytes）。
func Auth(token string, now func() time.Time) func(http.Handler) http.Handler {
	return AuthN(token, now, DefaultMaxBodyBytes)
}

// AuthN 与 Auth 同款，但可指定参与签名的请求体上限。
//
// 存在的理由只有一个：文件上传的请求体远大于 1 MiB，而验签必须先拿到完整 body。
// 因此给文件路由单独一个更大的上限，其余端点保持默认——上限的作用域越小越好。
//
// 校验顺序刻意固定为 Bearer → 时间戳 → 签名：
// 先挡掉没有凭证的请求，再挡掉过期请求，最后才做相对昂贵的 HMAC 计算。
// 注意 readBody 在验签之前：body 上限同时也是「持有效 Bearer 者在验签失败前
// 能迫使 Agent 分配的内存」，所以它必须是个有界的、由配置决定的值。
//
// now 可注入，便于测试时间戳窗口；传 nil 用 time.Now。
func AuthN(token string, now func() time.Time, maxBody int64) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
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

			body, err := readBody(r, maxBody)
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
func readBody(r *http.Request, maxBody int64) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	defer r.Body.Close()
	// 多读一个字节用于判断是否超限。
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBody {
		return nil, errBodyTooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

func writeAgentError(w http.ResponseWriter, code, message string) {
	httpx.WriteError(w, protocol.StatusForCode(code), code, message)
}
