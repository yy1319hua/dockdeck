// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Compose 接口的 Hub 侧客户端（S4.2）。路径与 Agent 侧路由一字不差：
// list 走 do（信封 JSON），action 走 do（信封 JSON，返回 ComposeActionResult），
// logs 走裸流（与容器日志同款，复用 LogsStream，不经信封解码）。
//
// 与镜像客户端同哲学：该拒的请求不出门——ComposeAction 先过
// ValidComposeAction，非法动作（如 rm）在 Hub 侧就拦下，不浪费一跳。

package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// ComposeList 列举白名单内 compose 项目。
func (c *Client) ComposeList(ctx context.Context) ([]protocol.ComposeProject, error) {
	out := []protocol.ComposeProject{}
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/compose", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ComposeAction 对白名单内项目执行写动作（up/down/restart/pull）。
// 动作先过 ValidComposeAction：该拒的（如 rm、任意字符串）不出门。
func (c *Client) ComposeAction(ctx context.Context, project, action string) (*protocol.ComposeActionResult, error) {
	if !protocol.ValidComposeAction(action) {
		return nil, fmt.Errorf("agentcli: 不支持的 compose 动作 %q", action)
	}
	var res protocol.ComposeActionResult
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/compose/"+project+"/"+action, nil, &res); err != nil {
		return nil, normalize(err)
	}
	return &res, nil
}

// ComposeLogs 打开项目日志流（GET /api/v1/compose/{project}/logs?follow=）。
//
// 与容器日志三处一致：不解信封、不施加客户端读超时（生命周期由 ctx 决定）、
// 不重试。项目名在本地校验非空：该拒的请求不出门。复用 LogsStream——
// compose 日志是纯文本（text/plain），Filename/Export 字段留空。
func (c *Client) ComposeLogs(ctx context.Context, project string, follow bool) (*LogsStream, error) {
	if project == "" {
		return nil, fmt.Errorf("agentcli: 项目名不能为空")
	}
	q := url.Values{}
	if follow {
		q.Set("follow", "1")
	}
	path := "/api/v1/compose/" + project + "/logs"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	req, err := c.newSignedRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.transport().Do(req)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeUpstreamUnavailable, err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		var env envelope
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, protocol.NewError(protocol.CodeUpstreamError, fmt.Sprintf("agent 返回 %d: %.200s", resp.StatusCode, raw))
	}
	return &LogsStream{Body: resp.Body, ContentType: resp.Header.Get("Content-Type")}, nil
}
