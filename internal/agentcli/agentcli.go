// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package agentcli 是 Hub 侧调用 Agent HTTP API 的客户端。
//
// 它只负责传输策略，不做业务判断：
//   - 超时分级：读（GET/HEAD）与写（其余 method）分别施加超时，写更短——
//     Hub 对 Agent 的写操作要求快速见效，宁可失败也不挂死轮询循环；
//   - 重试分级：读是幂等的，网络错误 / 429 / 5xx 可重试；写绝不重试——
//     Agent 侧写操作尚无幂等去重（S3 引入 X-DD-Request-Id），盲目重试
//     可能把一次 stop 变成两次，违反「不猜语义」红线；
//   - /test 连通性：纳管与巡检共用，顺带报告协议版本是否兼容；
//   - 每个请求（含读）都附 HMAC 签名：Agent 侧 Auth 中间件对全部请求强制验签。
package agentcli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 默认策略。写成导出常量而非隐式字面量，让调用方知道自己拿到的是什么。
const (
	// DefaultReadTimeout 覆盖采集快照：/proc 解析是毫秒级，10s 已极宽。
	DefaultReadTimeout = 10 * time.Second
	// DefaultWriteTimeout 是写操作上限：必须快速见效，见包注释。
	DefaultWriteTimeout = 5 * time.Second
	// DefaultMaxReadTries 是读请求最大尝试次数（含首次）。
	DefaultMaxReadTries = 3
	// DefaultRetryBackoff 是读重试的基础等待，逐次翻倍。
	DefaultRetryBackoff = 200 * time.Millisecond
)

// envelope 与 Agent 侧 httpx.Envelope 对齐，但只取客户端需要的部分。
type envelope struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error *protocol.Error `json:"error,omitempty"`
}

// Client 是单台 Agent 的客户端。零值不可用，须用 New 构造。
type Client struct {
	// BaseURL 是 Agent 根地址（如 https://100.x.y.z:9527），不带尾斜杠。
	BaseURL string
	// Token 是这台 Agent 的共享凭证。
	Token string

	// ReadTimeout / WriteTimeout 施加为每次请求的 ctx 超时（与调用方 ctx 取更紧者）。
	// 置 0 用默认值；置负数表示不限（仅测试用）。
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// MaxReadTries 是读请求最大尝试次数（含首次）。置 0 用默认值。
	MaxReadTries int
	// RetryBackoff 是读重试基础等待，逐次翻倍。置 0 用默认值。
	RetryBackoff time.Duration

	// HTTP 是传输层，可注入自定 TLS（Agent 用自签证书，Hub 纳管时按指纹信任）。
	// nil 时使用标准传输并跳过证书校验——指纹信任是 Hub 上层职责，客户端不越权。
	HTTP *http.Client

	// Now 可注入，测试签名时间戳用。
	Now func() time.Time
}

// New 构造一个使用默认策略的客户端。tlsCfg 可为 nil（明文或上层已处理）。
func New(baseURL, token string, tlsCfg *tls.Config) *Client {
	// Agent 全程自签证书：tlsCfg 为 nil 时跳过校验，指纹信任由 Hub 上层负责。
	var tr *http.Transport
	if tlsCfg != nil {
		tr = &http.Transport{TLSClientConfig: tlsCfg}
	} else {
		tr = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return &Client{
		BaseURL:      baseURL,
		Token:        token,
		ReadTimeout:  DefaultReadTimeout,
		WriteTimeout: DefaultWriteTimeout,
		MaxReadTries: DefaultMaxReadTries,
		RetryBackoff: DefaultRetryBackoff,
		HTTP:         &http.Client{Transport: tr},
	}
}

// TestResult 是 /test 连通性检查的结果。
type TestResult struct {
	// Latency 是从发请求到收到完整响应的耗时。
	Latency time.Duration
	// Info 是 Agent 上报的静态信息。
	Info protocol.AgentInfo
	// ProtocolCompatible 表示 Agent 协议版本与 Hub 一致。
	// 不一致时上层应拒绝纳管而不是猜字段语义，因此单独报告而非直接报错。
	ProtocolCompatible bool
}

// Test 检查 Agent 连通性（GET /api/v1/info，读语义可重试）。
func (c *Client) Test(ctx context.Context) (*TestResult, error) {
	start := time.Now()
	var info protocol.AgentInfo
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/info", nil, &info); err != nil {
		return nil, err
	}
	return &TestResult{
		Latency:            time.Since(start),
		Info:               info,
		ProtocolCompatible: info.ProtocolVersion == protocol.ProtocolVersion,
	}, nil
}

// Info 拉取 Agent 静态信息（读语义）。
func (c *Client) Info(ctx context.Context) (*protocol.AgentInfo, error) {
	var info protocol.AgentInfo
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/info", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Metrics 拉取一次主机指标快照（读语义）。
func (c *Client) Metrics(ctx context.Context) (*protocol.HostMetrics, error) {
	var m protocol.HostMetrics
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/metrics", nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ContainerStats 拉取每容器资源占用快照（读语义）。
func (c *Client) ContainerStats(ctx context.Context) ([]protocol.ContainerStat, error) {
	var stats []protocol.ContainerStat
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/containers/stats", nil, &stats); err != nil {
		return nil, err
	}
	return stats, nil
}

// retryableStatus 判断读请求遇到该状态码是否值得重试：
// 429 是限流，5xx 是 Agent 侧瞬态故障；其余 4xx 是调用方的问题，重试无用。
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// httpStatusError 是「Agent 返回了非 2xx」的载体。
// 业务错误（信封里的 protocol.Error）直接透传；这个类型兜住信封之外的
// 异常状态（如裸 500、无法解析的响应体），让重试判定有依据。
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("agent 返回 %d: %.200s", e.Status, e.Body)
}

// do 执行一次请求并解码信封。GET/HEAD 走读策略（可重试），其余走写策略（不重试）。
//
// 返回值是 Agent 的 HTTP 状态码（成功路径用得上）。错误统一归一为
// *protocol.Error：网络不可达 → upstream_unavailable，信封业务错误 → 原样透传，
// 其余非 2xx → upstream_error。
func (c *Client) do(ctx context.Context, method, path string, body []byte, dst interface{}) (int, error) {
	read := method == http.MethodGet || method == http.MethodHead
	tries, timeout := 1, c.WriteTimeout
	if read {
		tries, timeout = c.MaxReadTries, c.ReadTimeout
		if tries <= 0 {
			tries = 1
		}
	}
	backoff := c.RetryBackoff
	if backoff <= 0 {
		backoff = DefaultRetryBackoff
	}

	var lastErr error
	var lastStatus int
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return lastStatus, protocol.NewError(protocol.CodeUpstreamUnavailable, "等待重试期间被取消: "+ctx.Err().Error())
			case <-timer.C:
			}
			backoff *= 2
		}
		status, err := c.once(ctx, method, path, body, dst, timeout)
		lastStatus, lastErr = status, err
		if err == nil {
			return status, nil
		}
		// 业务错误（确定性结果）与确定性 HTTP 状态（非 5xx/429）立即返回，
		// 不分读写——只有瞬态失败才值得重试，且写请求 tries 恒为 1，天然不重试。
		if _, ok := err.(*protocol.Error); ok {
			return status, err
		}
		if hse, ok := err.(*httpStatusError); ok && !retryableStatus(hse.Status) {
			return status, normalize(hse)
		}
		if !read {
			break // 写策略：无论结果如何都不再尝试
		}
	}
	if lastErr == nil {
		return lastStatus, nil
	}
	return lastStatus, normalize(lastErr)
}

// normalize 把传输层/HTTP 层错误归一为 *protocol.Error。
func normalize(err error) *protocol.Error {
	if pe, ok := err.(*protocol.Error); ok {
		return pe
	}
	if hse, ok := err.(*httpStatusError); ok {
		if retryableStatus(hse.Status) {
			return protocol.NewError(protocol.CodeUpstreamError, fmt.Sprintf("agent 状态码 %d", hse.Status))
		}
		return protocol.NewError(protocol.CodeUpstreamError, hse.Error())
	}
	return protocol.NewError(protocol.CodeUpstreamUnavailable, err.Error())
}

// once 发起一次真实请求并解码响应。错误分类：
//   - 网络层失败（含超时、ctx 取消）→ 原始 error（normalize 时归 upstream_unavailable）
//   - 信封里的业务错误 → *protocol.Error
//   - 其它非 2xx → *httpStatusError
func (c *Client) once(ctx context.Context, method, path string, body []byte, dst interface{}, timeout time.Duration) (int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	// 每个请求都附带时间戳 + HMAC 签名。agenthttp.Auth 对读也强制验签
	// （「Bearer + HMAC 双重校验」是 Agent 侧既有契约），客户端必须对齐——
	// 这里曾按「只签写」实现，S2.8 端到端测试用真实验签中间件把它抓了出来。
	// 签名路径用 agenthttp.CanonicalPath（RequestURI 含查询串），两侧一字不差。
	stamp := strconv.FormatInt(c.now().Unix(), 10)
	req.Header.Set(agenthttp.HeaderTimestamp, stamp)
	req.Header.Set(agenthttp.HeaderSignature, crypto.SignRequest([]byte(c.Token), stamp, method, agenthttp.CanonicalPath(req), body))
	resp, err := c.transport().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 有信封错误体就透传业务错误，否则按状态码兜底。
		var env envelope
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			return resp.StatusCode, env.Error
		}
		return resp.StatusCode, &httpStatusError{Status: resp.StatusCode, Body: string(raw)}
	}
	if resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(raw)) == 0 {
		// 204 或空响应体：没有信封可解，写操作常用。dst 不触碰，调用方自行判断。
		return resp.StatusCode, nil
	}
	var out envelope
	if err := json.Unmarshal(raw, &out); err != nil {
		return resp.StatusCode, protocol.NewError(protocol.CodeUpstreamError, "响应不是合法信封: "+err.Error())
	}
	if out.Error != nil {
		return resp.StatusCode, out.Error
	}
	if dst != nil && len(out.Data) > 0 {
		if err := json.Unmarshal(out.Data, dst); err != nil {
			return resp.StatusCode, protocol.NewError(protocol.CodeUpstreamError, "响应数据解码失败: "+err.Error())
		}
	}
	return resp.StatusCode, nil
}

// now 返回当前时间，测试可注入。
func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// transport 返回传输层，懒初始化（New 之外手工构造 Client 的兜底）。
func (c *Client) transport() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	c.HTTP = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	return c.HTTP
}
