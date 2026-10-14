// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器日志的 Hub 侧客户端（S3.4）。
//
// 日志响应不是信封：Agent 直接给裸流（合流文本 text/plain，或 export 的
// 原始字节 octet-stream）。因此本方法不复用 do()——那套信封解码与读超时
// 分级都是 JSON API 的事，用在流上会把长连接掐断。

package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// LogsStream 是一条打开的日志流。Body 的关闭责任在调用方。
type LogsStream struct {
	// Body 是响应体：合流文本或 export 原始字节（见 ContentType）。
	Body io.ReadCloser
	// ContentType 是 Agent 给出的内容类型（text/plain 或 application/octet-stream）。
	ContentType string
	// Filename 是 export=1 时 Agent 建议的下载文件名；非 export 为空。
	Filename string
}

// Logs 打开容器日志流（GET /api/v1/containers/{id}/logs）。
//
// 与信封类方法的三个关键区别：
//   - 不解信封：裸流原样给调用方，Hub 不猜一个字节；
//   - 不施加客户端超时：流的生命周期由调用方 ctx 决定——follow 是长连接，
//     任何读超时都会把它掐断；非 follow 的大导出同理；
//   - 不重试：流式响应没有「安全重放」语义（GET 幂等不代表流幂等），
//     连接级失败由调用方决定重开，客户端不擅自重连。
//
// ID 与 tail 在本地校验（与 Agent 侧同款规则）：该拒的请求不出门。
func (c *Client) Logs(ctx context.Context, id string, o protocol.LogsOptions) (*LogsStream, error) {
	if !protocol.ValidContainerID(id) {
		return nil, fmt.Errorf("agentcli: 非法容器标识 %q", id)
	}
	if !protocol.ValidTail(o.Tail) {
		return nil, fmt.Errorf("agentcli: tail 必须是 all 或数字，got %q", o.Tail)
	}
	q := url.Values{}
	if o.Follow {
		q.Set("follow", "1")
	}
	if o.Tail != "" {
		q.Set("tail", o.Tail)
	}
	// since/until 必须经 Values 编码：RFC3339 的 "+08:00" 不转义会被
	// 查询串解析成空格，时区信息静默丢失。
	if o.Since != "" {
		q.Set("since", o.Since)
	}
	if o.Until != "" {
		q.Set("until", o.Until)
	}
	if o.Timestamps {
		q.Set("timestamps", "1")
	}
	if o.Export {
		q.Set("export", "1")
	}
	path := "/api/v1/containers/" + id + "/logs"
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
		// 失败响应是信封（Agent 的 WriteError），按信封类方法同款解读，
		// 读完即关——流式路径也不能漏关 body。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		var env envelope
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, protocol.NewError(protocol.CodeUpstreamError,
			fmt.Sprintf("agent 返回 %d: %.200s", resp.StatusCode, raw))
	}
	return &LogsStream{
		Body:        resp.Body,
		ContentType: resp.Header.Get("Content-Type"),
		Filename:    filenameFromDisposition(resp.Header.Get("Content-Disposition")),
	}, nil
}

// filenameFromDisposition 从 Content-Disposition 取文件名（export 下载用）。
// 取不到返回空——调用方自行兜底，不猜名字。
func filenameFromDisposition(h string) string {
	if h == "" {
		return ""
	}
	for _, part := range strings.Split(h, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "filename="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
