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

// ComposeFileGet 读取指定项目的 compose 文件内容（GET /api/v1/compose/{project}/file）。
// 空项目名本地拦——该拒的请求不出门（与 ComposeLogs 同口径）。
func (c *Client) ComposeFileGet(ctx context.Context, project string) (*protocol.ComposeFile, error) {
	if project == "" {
		return nil, fmt.Errorf("agentcli: 项目名不能为空")
	}
	var res protocol.ComposeFile
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/compose/"+project+"/file", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ComposeFileDiff 提交新内容算 unified diff 供预览（POST /api/v1/compose/{project}/diff）。
// 服务端不落盘、不产生备份；这里是「先看再写」两段式里的第一段。
func (c *Client) ComposeFileDiff(ctx context.Context, project string, content string) (*protocol.ComposeFileDiff, error) {
	if project == "" {
		return nil, fmt.Errorf("agentcli: 项目名不能为空")
	}
	body, err := json.Marshal(protocol.ComposeFileUpdate{Content: content})
	if err != nil {
		return nil, err
	}
	var res protocol.ComposeFileDiff
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/compose/"+project+"/diff", body, &res); err != nil {
		return nil, normalize(err)
	}
	return &res, nil
}

// ComposeFilePut 写入新内容（PUT /api/v1/compose/{project}/file）。Agent 侧写前自动
// 备份 <file>.bak.<RFC3339>，这里把备份路径回传，供调用方展示/回滚。
func (c *Client) ComposeFilePut(ctx context.Context, project string, content string) (*protocol.ComposeFileWritten, error) {
	if project == "" {
		return nil, fmt.Errorf("agentcli: 项目名不能为空")
	}
	body, err := json.Marshal(protocol.ComposeFileUpdate{Content: content})
	if err != nil {
		return nil, err
	}
	var res protocol.ComposeFileWritten
	if _, err := c.do(ctx, http.MethodPut, "/api/v1/compose/"+project+"/file", body, &res); err != nil {
		return nil, normalize(err)
	}
	return &res, nil
}

// ComposeBackups 列举项目的写前备份（只读）。只回文件名/大小/时间，不含目录。
func (c *Client) ComposeBackups(ctx context.Context, project string) ([]protocol.ComposeBackup, error) {
	if project == "" {
		return nil, fmt.Errorf("agentcli: 项目名不能为空")
	}
	var res protocol.ComposeBackupsList
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/compose/"+project+"/backups", nil, &res); err != nil {
		return nil, err
	}
	return res.Backups, nil
}

// ComposeBackupContent 读备份正文（预览与 diff 用）。
// 备份名走查询串并转义——它含 RFC3339 的 "+" 与 ":"（如 +08:00），不转义
// 会被当成空格/分隔符吃掉一截。
func (c *Client) ComposeBackupContent(ctx context.Context, project, name string) (*protocol.ComposeBackupContent, error) {
	if project == "" {
		return nil, fmt.Errorf("agentcli: 项目名不能为空")
	}
	if name == "" {
		return nil, fmt.Errorf("agentcli: 备份名不能为空")
	}
	var res protocol.ComposeBackupContent
	path := "/api/v1/compose/" + project + "/backups/content?file=" + url.QueryEscape(name)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ComposeRestore 用备份覆盖当前 compose 文件。
// 恢复本身是一次覆盖写，Agent 侧会先把「恢复前的当前内容」备份下来（Q2-A），
// 响应回传那份新备份的文件名。
func (c *Client) ComposeRestore(ctx context.Context, project, name string) (*protocol.ComposeBackupRestored, error) {
	if project == "" {
		return nil, fmt.Errorf("agentcli: 项目名不能为空")
	}
	if name == "" {
		return nil, fmt.Errorf("agentcli: 备份名不能为空")
	}
	body, err := json.Marshal(protocol.ComposeBackupUpdate{File: name})
	if err != nil {
		return nil, err
	}
	var res protocol.ComposeBackupRestored
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/compose/"+project+"/backups/restore", body, &res); err != nil {
		return nil, normalize(err)
	}
	return &res, nil
}
