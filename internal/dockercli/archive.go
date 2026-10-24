// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器文件流（P2.1）。零挂载访问层的地基：经 Engine 的 archive 接口
// 以 tar 流形式读写容器内文件。本文件只做传输，不猜业务语义——
// 「借道哪个容器」「路径怎么解析」是 fsapi（P2.2/P2.3）的事。

package dockercli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ArchiveStat 是 GET /containers/{id}/archive 响应头里的路径元信息
// （X-Docker-Container-Path-Stat，base64(JSON)）。字段原样透传，
// 缺失/多余由调用方判断——本包不补默认值（不猜语义红线）。
type ArchiveStat struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Mode       int64  `json:"mode"`
	Mtime      string `json:"mtime"`
	LinkTarget string `json:"linkTarget,omitempty"`
}

// CopyFromContainer 打开源路径的 tar 归档流。Engine 约定：
//   - path 指向文件：流内是「父目录/<文件名>」单条目；
//   - path 指向目录：流内是「目录/...」整棵树；
//   - stat 描述的是 path 本身（而非流内条目）。
//
// 关闭责任在调用方。path 为空或容器标识非法时拒绝请求。
func (c *Client) CopyFromContainer(ctx context.Context, id, path string) (io.ReadCloser, ArchiveStat, error) {
	if !idSafe(id) {
		return nil, ArchiveStat{}, fmt.Errorf("dockercli: 非法容器标识 %q", id)
	}
	if strings.TrimSpace(path) == "" {
		return nil, ArchiveStat{}, fmt.Errorf("dockercli: 源路径为空")
	}
	resp, err := c.doStream(ctx, http.MethodGet,
		"/containers/"+id+"/archive?path="+url.QueryEscape(path), false)
	if err != nil {
		return nil, ArchiveStat{}, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, ArchiveStat{}, fmt.Errorf("dockercli: 容器或路径不存在 (404): %s:%s", id, path)
		}
		return nil, ArchiveStat{}, fmt.Errorf("dockercli: archive get %s 返回 %d: %.200s", path, resp.StatusCode, body)
	}
	stat, err := decodeArchiveStat(resp.Header.Get("X-Docker-Container-Path-Stat"))
	if err != nil {
		resp.Body.Close()
		return nil, ArchiveStat{}, err
	}
	return resp.Body, stat, nil
}

// CopyToContainerOptions 控制 PUT archive 的行为位。
type CopyToContainerOptions struct {
	// NoOverwriteDirNonDir 为 true 时，目标已有的「目录↔文件」冲突直接报错
	// 而不是静默替换（Engine 语义原样转发）。
	NoOverwriteDirNonDir bool
	// CopyUIDGID 为 true 时保留 tar 条目里的 UID/GID（默认 Engine 会重映射）。
	CopyUIDGID bool
}

// CopyToContainer 把 tar 归档流写入容器 dstDir（流内条目按归档内相对路径落位）。
// dstDir 为空或容器标识非法时拒绝请求。流由本方法读完并关闭。
func (c *Client) CopyToContainer(ctx context.Context, id, dstDir string, tar io.Reader, o CopyToContainerOptions) error {
	if !idSafe(id) {
		return fmt.Errorf("dockercli: 非法容器标识 %q", id)
	}
	if strings.TrimSpace(dstDir) == "" {
		return fmt.Errorf("dockercli: 目标目录为空")
	}
	if tar == nil {
		return fmt.Errorf("dockercli: tar 流为空")
	}
	q := "?path=" + url.QueryEscape(dstDir)
	if o.NoOverwriteDirNonDir {
		q += "&noOverwriteDirNonDir=1"
	}
	if o.CopyUIDGID {
		q += "&copyUIDGID=1"
	}
	// 归档上传可能远超 do 的 10s 超时，走带 body 的流式通道。
	resp, err := c.doStreamBody(ctx, http.MethodPut,
		"/containers/"+id+"/archive"+q, "application/x-tar", tar)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("dockercli: 容器或目标目录不存在 (404): %s:%s", id, dstDir)
		}
		// :ro 等写失败场景 Engine 返回 403/500，原文透传给上层定提示（P2.2）。
		return fmt.Errorf("dockercli: archive put %s 返回 %d: %.200s", dstDir, resp.StatusCode, body)
	}
	return nil
}

// decodeArchiveStat 解 base64(JSON) 响应头。头缺失按 Engine 契约视为异常。
func decodeArchiveStat(h string) (ArchiveStat, error) {
	if h == "" {
		return ArchiveStat{}, fmt.Errorf("dockercli: 响应缺 X-Docker-Container-Path-Stat 头")
	}
	raw, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return ArchiveStat{}, fmt.Errorf("dockercli: stat 头解码失败: %w", err)
	}
	var s ArchiveStat
	if err := json.Unmarshal(raw, &s); err != nil {
		return ArchiveStat{}, fmt.Errorf("dockercli: stat 头 JSON 解析失败: %w", err)
	}
	return s, nil
}

// doStreamBody 与 logs.go 的 doStream 同族：流式透传、不套读超时，
// 区别仅在于可携带请求体（PUT archive 需要）。归档上传体量不定，
// 由 ctx 管生命周期，读超时交给调用方的 ctx。
func (c *Client) doStreamBody(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+c.versioned(path), body)
	if err != nil {
		return nil, fmt.Errorf("dockercli: 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dockercli: 引擎请求失败: %w", err)
	}
	return resp, nil
}

// versioned 补 API 版本前缀，与 do/doStream 的拼接规则一致。
func (c *Client) versioned(path string) string {
	if !strings.HasPrefix(path, "/_ping") {
		return "/v" + c.apiVersion + path
	}
	return path
}
