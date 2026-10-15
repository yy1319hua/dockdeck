// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 文件管理接口的 Hub 侧客户端（S5.5）。路径与查询串与 Agent 侧路由一字不差：
// list / stat / action / 写 走 do（信封 JSON），content 读走裸流——
// 与容器日志同款，不经信封解码，因为正文可能是任意二进制，套信封要么
// base64 膨胀 33%，要么整份读进内存，两条路都与「流式」相悖。
//
// 该拒的不出门：动作名先用 protocol.ValidFileAction 校验，写正文超限用共享常量
// MaxFileUploadBytes 拦下——这两处 Hub 与 Agent 共用同一源头，不存在漂移。
// 反过来，root 的合法范围只有 Agent 知道（它才知道配了几个根），故不做本地范围
// 校验：本地拦一半会变成两处口径，漂移比多一跳更贵。

package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// FileStream 是一次打开的文件正文流。Body 的关闭责任在调用方。
type FileStream struct {
	Body io.ReadCloser
	// ContentType 由 Agent 探测得出，未知时为 application/octet-stream。
	ContentType string
	// Path 是相对路径（响应头 X-DD-File-Path），不暴露宿主机绝对路径。
	Path string
	// Size 是元信息里的原始字节数（X-DD-File-Size）。
	Size int64
	// Truncated 表示正文已按单文件上限截断。此时调用方不该给出 Content-Length
	// ——给了等于撒谎：客户端会把「被我们截了」当成「文件就这么长」。
	Truncated bool
}

// fileItemQuery 供 stat / content 读写使用：只认 root 与 path。
//
// path 为空表示根自身，是一次合法查询而非「缺参数」，故照发不跳过。
// 值一律经 url.Values 编码：路径里可能有空格与非 ASCII 字符，手拼会把请求打歪。
func fileItemQuery(root int, path string) string {
	q := url.Values{}
	if root != 0 {
		q.Set("root", strconv.Itoa(root))
	}
	q.Set("path", path)
	return q.Encode()
}

// fileListQuery 供 list 使用：额外带上 depth。
func fileListQuery(root int, path string, depth int) string {
	q := url.Values{}
	if root != 0 {
		q.Set("root", strconv.Itoa(root))
	}
	q.Set("path", path)
	q.Set("depth", strconv.Itoa(depth))
	return q.Encode()
}

// fileRootQuery 供 action 使用：动作的 path/dest 走请求体，查询串只要 root。
func fileRootQuery(root int) string {
	q := url.Values{}
	if root != 0 {
		q.Set("root", strconv.Itoa(root))
	}
	return q.Encode()
}

// FileList 列举目录内容（GET /api/v1/files?root=&path=&depth=）。
func (c *Client) FileList(ctx context.Context, root int, path string, depth int) (*protocol.FilesList, error) {
	var res protocol.FilesList
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/files?"+fileListQuery(root, path, depth), nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// FileStat 取文件或目录的元信息（GET /api/v1/files/stat?root=&path=）。
func (c *Client) FileStat(ctx context.Context, root int, path string) (*protocol.FileMeta, error) {
	var res protocol.FileMeta
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/files/stat?"+fileItemQuery(root, path), nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// FileContent 打开文件正文流（GET /api/v1/files/content?root=&path=）。
//
// 与信封类方法的三处区别同 ComposeLogs：不解信封、不施加客户端读超时（下载大文件
// 同样不能掐）、不重试。
func (c *Client) FileContent(ctx context.Context, root int, path string) (*FileStream, error) {
	p := "/api/v1/files/content"
	if enc := fileItemQuery(root, path); enc != "" {
		p += "?" + enc
	}
	req, err := c.newSignedRequest(ctx, http.MethodGet, p, nil)
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
		return nil, protocol.NewError(protocol.CodeUpstreamError,
			fmt.Sprintf("agent 返回 %d: %.200s", resp.StatusCode, raw))
	}
	// Size 只是展示用元信息：解析不出,也不值得让整条流失败，取零即可。
	size, _ := strconv.ParseInt(resp.Header.Get("X-DD-File-Size"), 10, 64)
	return &FileStream{
		Body:        resp.Body,
		ContentType: resp.Header.Get("Content-Type"),
		Path:        resp.Header.Get("X-DD-File-Path"),
		Size:        size,
		Truncated:   resp.Header.Get("X-DD-Truncated") == "1",
	}, nil
}

// FileWrite 写入文件内容（PUT /api/v1/files/content?root=&path=）。
//
// content 就是文件内容本身（不套信封）；调用方需先把它读进内存——HMAC 签名必须
// 持有完整 body，这条约束与 Agent 侧同源（见 MaxFileUploadBytes 的注释），
// 因此这里也按同一上限把超限请求拦在出门前。
func (c *Client) FileWrite(ctx context.Context, root int, path string, content []byte) (*protocol.FileWritten, error) {
	if int64(len(content)) > protocol.MaxFileUploadBytes {
		return nil, fmt.Errorf("agentcli: 上传内容 %d 字节超过共享上限 %d 字节",
			len(content), protocol.MaxFileUploadBytes)
	}
	var res protocol.FileWritten
	if _, err := c.do(ctx, http.MethodPut, "/api/v1/files/content?"+fileItemQuery(root, path), content, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// FileAction 执行 mkdir / delete / rename（POST /api/v1/files/{action}?root=）。
//
// 动作名在本地校验：它是 URL 的一段，拼请求前拦下比让 Agent 回 400 更省一跳，
// 且判定源是共享的 protocol.ValidFileAction，两侧不会漂移。
// rename 需要 dest 这条业务规则不在此处重复——那是 fsapi 的职责，
// 复制过来就多一处随 fsapi 变更而失真的地方。
func (c *Client) FileAction(ctx context.Context, root int, action, path, dest string) (*protocol.FileActionDone, error) {
	if !protocol.ValidFileAction(action) {
		return nil, fmt.Errorf("agentcli: 不支持的文件动作 %q", action)
	}
	body, err := json.Marshal(protocol.FileAction{Action: action, Path: path, Dest: dest})
	if err != nil {
		return nil, err
	}
	ep := "/api/v1/files/" + url.PathEscape(action)
	if enc := fileRootQuery(root); enc != "" {
		ep += "?" + enc
	}
	var res protocol.FileActionDone
	if _, err := c.do(ctx, http.MethodPost, ep, body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
