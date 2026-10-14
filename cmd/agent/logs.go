// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S3.3：Agent 日志端点 GET /api/v1/containers/{id}/logs。
// 查询参数：follow=1、tail=N、since=、until=、timestamps=1、export=1。
// 默认输出合流文本（Demux）；export=1 时原样导出 Engine 字节流（下载用）。
// 日志是读操作，不做自我保护守卫——看自己的日志天经地义。

package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// logsQuery 把查询串收拢成 LogsOptions；tail 校验失败时返回 err!=nil。
func logsQuery(r *http.Request) (dockercli.LogsOptions, error) {
	// 显式 ParseQuery 而非 r.URL.Query()：后者遇到非法查询串（如分号）
	// 会静默丢参数继续跑，校验就被整个绕过了。
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return dockercli.LogsOptions{}, err
	}
	var o dockercli.LogsOptions
	o.Follow = q.Get("follow") == "1"
	o.Tail = q.Get("tail")
	o.Since = q.Get("since")
	o.Until = q.Get("until")
	o.Timestamps = q.Get("timestamps") == "1"
	// tail 只放行 all 与纯数字，别把怪字符串透传给 Engine。
	// 规则单点定义在 protocol（Hub 拼装侧同一份），两侧不容许漂移。
	if !protocol.ValidTail(o.Tail) {
		return o, errors.New("tail 必须是 all 或数字")
	}
	// since/until 归一成 Unix 秒再落 Engine：实测 Engine 29.1.3 只认它，
	// RFC3339 再正确也会 500（ARCHITECTURE.md「Engine 契约实测基线」）。
	// 规则单点定义在 protocol，与 ValidTail 同款两侧共用——Hub 侧
	// （agentcli）用同一函数做出门前预检，归一本端独占。
	if v, ok := protocol.NormalizeTimeParam(o.Since); ok {
		o.Since = v
	} else {
		return o, errors.New("since 必须是 Unix 秒或 RFC3339")
	}
	if v, ok := protocol.NormalizeTimeParam(o.Until); ok {
		o.Until = v
	} else {
		return o, errors.New("until 必须是 Unix 秒或 RFC3339")
	}
	return o, nil
}

// logs 实现 GET /api/v1/containers/{id}/logs。
// follow=1 时是长连接流：逐帧 Flush，客户端断开即结束。
func (a *containersAPI) logs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	o, err := logsQuery(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	export := r.URL.Query().Get("export") == "1"

	stream, err := a.dc.Logs(r.Context(), id, o)
	if err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	defer stream.Close()

	if export {
		// 原始导出：复用流原样给下载方（docker CLI 也能直接消费这种流）。
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(id)+`.log"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, stream)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// 逐块 Flush：follow 模式下前端才能看到「滚动」，非 follow 也有首字节快的收益。
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	if err := dockercli.Demux(stream, flushWriter{w, flusher}); err != nil {
		// 流中途断掉：状态码已发出，只能记日志。前端以连接关闭为结束信号。
		log.Printf("agent: 日志流中断 (%s, %s=%q): %v", id, protocol.HeaderRequestID, r.Header.Get(protocol.HeaderRequestID), err)
	}
}

// flushWriter 让 Demux 的每次写入都能推给客户端。
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil && fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// sanitizeFilename 让 Content-Disposition 里的文件名不带奇怪字符。
func sanitizeFilename(id string) string {
	out := make([]rune, 0, len(id))
	for _, ch := range id {
		if (ch >= 0x30 && ch <= 0x39) || (ch >= 0x41 && ch <= 0x5A) || (ch >= 0x61 && ch <= 0x7A) || ch == 0x2D || ch == 0x5F {
			out = append(out, ch)
		}
	}
	if len(out) == 0 {
		return "container"
	}
	return string(out)
}
