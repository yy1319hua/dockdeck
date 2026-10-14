// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package dockercli 是 Docker Engine API 的最小客户端。
//
// 只依赖标准库：unix socket 直连 + JSON 解码，不引 docker/docker 这个庞然大物
// （交叉编译体积与「不把 /containers 语义封装成黑盒」同 collector 的取舍）。
//
// S3.1 范围：连接、版本协商、ping/version/info 三个只读调用。
// 容器操作（S3.2）与 stats（ContainerStatsSource 真源）在后续小步追加，
// 但传输层与本文件在此一次做对。
//
// 「不猜语义」红线：本包只转发 Engine API 的返回，不做任何业务推断；
// 字段缺失/多余都原样保留，语义以官方 API 文档为准。
package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultSocketPath 是 Engine API 的默认 socket 位置（Ubuntu/Debian 官方包如此）。
	DefaultSocketPath = "/var/run/docker.sock"
	// DefaultAPIVersion 是协商失败时的兜底 API 版本。选一个老而稳的版本：
	// Engine 对低于服务器版本的请求会自动向下降级，取太新反而 400。
	DefaultAPIVersion = "1.44"
	// RequestTimeout 是单次请求上限。Engine API 的只读调用是毫秒级，
	// stats?stream=false 也在秒级返回，10s 足够宽。
	RequestTimeout = 10 * time.Second
)

// Client 是单台宿主机 Engine API 的客户端。并发安全（http.Client 天然并发）。
type Client struct {
	// SocketPath 是 unix socket 路径，测试时可指向临时 socket。
	SocketPath string
	// HTTP 可注入自定义传输（测试用假 Engine 时覆盖 DialContext 即可）。
	HTTP *http.Client
	// RequestTimeout 可覆盖单次请求超时；置负数表示不限（仅测试用）。
	RequestTimeout time.Duration

	// apiVersion 是当前协商出的版本前缀（不带 "v"）。
	apiVersion string
}

// New 构造客户端，apiVersion 先用默认值，待 NegotiateAPIVersion 修正。
func New(socketPath string) *Client {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// 无视请求里的 network/addr，全部打进 unix socket——
			// 这是 Engine API 客户端的标准做法，URL 里的 host 仅是占位。
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		SocketPath:     socketPath,
		HTTP:           &http.Client{Transport: tr},
		RequestTimeout: RequestTimeout,
		apiVersion:     DefaultAPIVersion,
	}
}

// PingResult 是 /_ping 的响应头摘录。Engine 把版本与能力放在响应头而非响应体。
type PingResult struct {
	// APIVersion 是服务器支持的最新 API 版本（响应头 Api-Version）。
	APIVersion string
	// Server 是完整版本串（响应头 Server，如 Docker/29.1.3 (linux)）。
	Server string
	// OSType 是 linux/windows（响应头 Ostype），决定部分接口的可用性。
	OSType string
	// Experimental 标记服务器是否实验特性开启。
	Experimental bool
	// BuilderVersion 是 BuildKit 版本头（响应头 Builder-Version），可能为空。
	BuilderVersion string
}

// Ping 探活并取回服务器版本头。GET /_ping（不带版本前缀，Engine 约定如此）。
func (c *Client) Ping(ctx context.Context) (*PingResult, error) {
	status, body, header, err := c.do(ctx, http.MethodGet, "/_ping")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("dockercli: /_ping 返回 %d: %.100s", status, body)
	}
	return &PingResult{
		APIVersion:     header.Get("Api-Version"),
		Server:         header.Get("Server"),
		OSType:         header.Get("Ostype"),
		Experimental:   header.Get("Docker-Experimental") == "true",
		BuilderVersion: header.Get("Builder-Version"),
	}, nil
}

// NegotiateAPIVersion 用 /_ping 的 Api-Version 头更新请求前缀。
// 失败（engine 不可达或头非法）时保持原值并返回错误——调用方决定是
// 继续用默认版本硬试，还是报告引擎不可用；本包不吞这个决定。
func (c *Client) NegotiateAPIVersion(ctx context.Context) error {
	p, err := c.Ping(ctx)
	if err != nil {
		return err
	}
	if !isAPIVersion(p.APIVersion) {
		return fmt.Errorf("dockercli: Api-Version 头非法: %q", p.APIVersion)
	}
	c.apiVersion = p.APIVersion
	return nil
}

// APIVersion 返回当前生效的版本前缀（诊断用）。
func (c *Client) APIVersion() string { return c.apiVersion }

// isAPIVersion 宽松校验 "X.Y" 形式。Engine 的版本号只用数字与点。
func isAPIVersion(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

// do 执行一次请求：拼版本前缀（/_ping 除外）、施加超时、读响应。
// 返回状态码、响应体、响应头。错误统一包裹，不引入新的错误类型——
// S3.2 引入写操作后再按 protocol.Error 归一，现在过早。
func (c *Client) do(ctx context.Context, method, path string) (int, []byte, http.Header, error) {
	if !strings.HasPrefix(path, "/_ping") {
		path = "/v" + c.apiVersion + path
	}
	if c.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.RequestTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("dockercli: 构造请求失败: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("dockercli: 引擎请求失败: %w", err)
	}
	defer resp.Body.Close()
	// Engine 的错误体是 JSON，但最大也就几百字节；1MB 上限防异常服务器。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("dockercli: 读响应失败: %w", err)
	}
	return resp.StatusCode, body, resp.Header, nil
}

// getJSON 请求并解码 JSON 响应体。
func (c *Client) getJSON(ctx context.Context, path string, dst interface{}) error {
	status, body, _, err := c.do(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("dockercli: %s 返回 %d: %.200s", path, status, body)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("dockercli: %s 响应解码失败: %w", path, err)
	}
	return nil
}
