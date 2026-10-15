// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S5.1：Agent 文件管理接口。
//
// 所有路径先在 fsapi.Guard 里过完整套关卡（词法 → 白名单词法前缀 → realpath →
// 再前缀 → 黑名单），任何文件系统操作都排在其后。本文件只做三件事：
// 解析查询参数、调 fsapi、把 fsapi 的哨兵错误映射成状态码。
//
// 未配 --fs-root 时 newFilesAPI 返回 nil，main.go 一条路由都不注册——
// 模块在网络层就不存在（红线第 3 条「默认关闭」）。

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/fsapi"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// filesAPI 聚合文件端点的依赖。
type filesAPI struct {
	g *fsapi.Guard
	// maxBody 是这些路由的签名体上限：上传要把整个 body 读进来验签，
	// 所以它跟着单文件上限走（且不超过 protocol.MaxFileUploadBytes）。
	maxBody int64
}

// newFilesAPI 构造文件端点。未配根目录时返回 (nil, nil)，调用方据此不注册路由。
//
// 配了根目录却不可用时**返回错误让启动失败**，而不是静默降级成「模块关闭」：
// 用户以为文件管理开着、实际是关的，这种误解比启动失败危险得多。
func newFilesAPI(cfg config.AgentConfig) (*filesAPI, error) {
	if len(cfg.FsRoots) == 0 {
		log.Printf("agent: 未配置 --fs-root，文件管理模块不注册任何路由")
		return nil, nil
	}
	g, err := fsapi.NewGuard(cfg.FsRoots, cfg.FsDeny, cfg.FsMaxFile, cfg.FsMaxDepth, cfg.FsMaxEntries)
	if err != nil {
		return nil, err
	}
	maxBody := cfg.FsMaxFile
	if maxBody < agenthttp.DefaultMaxBodyBytes {
		maxBody = agenthttp.DefaultMaxBodyBytes
	}
	// 只记数量与上限，不记绝对路径：日志会被采集，宿主机路径属敏感信息。
	log.Printf("agent: 文件管理已启用（%d 个根白名单，单文件上限 %d 字节，深度上限 %d）",
		g.Roots(), g.MaxFile(), g.MaxDepth())
	return &filesAPI{g: g, maxBody: maxBody}, nil
}

// fileQuery 解析查询串。显式 ParseQuery 而非 r.URL.Query()：
// 后者遇到非法查询串会静默丢参数继续跑，校验就被绕过了（与 logs 端点同一处坑）。
func fileQuery(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "查询串非法")
		return nil, false
	}
	return q, true
}

// rootIndex 解析 ?root=（缺省 0）。越界在这里就拦下，fsapi 里那道是最后防线。
func (a *filesAPI) rootIndex(q url.Values) (int, error) {
	raw := q.Get("root")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n >= a.g.Roots() {
		return 0, fmt.Errorf("根序号 %q 非法（可用范围 0..%d）", raw, a.g.Roots()-1)
	}
	return n, nil
}

// depthParam 解析 ?depth=（缺省 1，只列直接子项）。
func depthParam(q url.Values) (int, error) {
	raw := q.Get("depth")
	if raw == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New("depth 需为非负整数")
	}
	return n, nil
}

// filesErr 把 fsapi 的哨兵错误映射成状态码与错误码。
//
// 分类依据（与 fsapi 的注释同一套口径）：
//   - 词法越界 / 过深 / 用错端点 → 400
//   - 解引用出白名单、命中黑名单、软链目标 → 403
//   - 不存在 → 404；已存在 / 目录非空 → 409；超上限 → 413
//   - 其余一律 500 且只回通用文案：底层 os 错误里带宿主机绝对路径，不能外泄。
func filesErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fsapi.ErrPathEscape), errors.Is(err, fsapi.ErrTooDeep),
		errors.Is(err, fsapi.ErrTooManyEntries), errors.Is(err, fsapi.ErrIsDir),
		errors.Is(err, fsapi.ErrNotDir):
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
	case errors.Is(err, fsapi.ErrPathDenied):
		httpx.WriteError(w, http.StatusForbidden, protocol.CodePathDenied, err.Error())
	case errors.Is(err, fsapi.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, err.Error())
	case errors.Is(err, fsapi.ErrExists), errors.Is(err, fsapi.ErrNotEmpty):
		httpx.WriteError(w, http.StatusConflict, protocol.CodeConflict, err.Error())
	case errors.Is(err, fsapi.ErrTooLarge):
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge, err.Error())
	default:
		log.Printf("agent: 文件操作失败: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "文件操作失败")
	}
}

// list 实现 GET /api/v1/files?root=&path=&depth=。
func (a *filesAPI) list(w http.ResponseWriter, r *http.Request) {
	q, ok := fileQuery(w, r)
	if !ok {
		return
	}
	idx, err := a.rootIndex(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	depth, err := depthParam(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	path := q.Get("path")
	entries, truncated, err := a.g.List(idx, path, depth)
	if err != nil {
		filesErr(w, err)
		return
	}
	// 空清单的 []（不是 null）由 fsapi.List 保证：它任何成功路径都返回非 nil 切片。
	// 这里刻意不再兜一层——兜了也没有测试能覆盖，反而会掩盖契约被破坏。
	httpx.WriteJSON(w, http.StatusOK, protocol.FilesList{
		Path:      path,
		Entries:   entries,
		Truncated: truncated,
	})
}

// stat 实现 GET /api/v1/files/stat?path=。
func (a *filesAPI) stat(w http.ResponseWriter, r *http.Request) {
	q, ok := fileQuery(w, r)
	if !ok {
		return
	}
	idx, err := a.rootIndex(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	meta, err := a.g.Stat(idx, q.Get("path"))
	if err != nil {
		filesErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, meta)
}

// content 实现 GET /api/v1/files/content?path=：流式正文。
//
// 刻意不套 JSON 信封：正文可能是任意二进制，套信封要么 base64 膨胀 33%，
// 要么把整个文件读进内存再序列化——两条路都与「流式」相悖。
// 元信息因此走响应头（X-DD-File-*），未截断时额外给 Content-Length。
func (a *filesAPI) content(w http.ResponseWriter, r *http.Request) {
	q, ok := fileQuery(w, r)
	if !ok {
		return
	}
	idx, err := a.rootIndex(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	rd, err := a.g.Open(idx, q.Get("path"))
	if err != nil {
		filesErr(w, err)
		return
	}
	defer rd.Close()

	h := w.Header()
	h.Set("Content-Type", rd.Meta.ContentType)
	h.Set("X-DD-File-Path", rd.Meta.Path)
	h.Set("X-DD-File-Size", strconv.FormatInt(rd.Meta.Size, 10))
	if rd.Meta.Truncated {
		// 截断标记必须让调用方看得见：否则「文件就这么长」与「被我们截了」
		// 在客户端看来一模一样。
		h.Set("X-DD-Truncated", "1")
	} else {
		h.Set("Content-Length", strconv.FormatInt(rd.Meta.Size, 10))
	}

	flusher, _ := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(flushWriter{w, flusher}, rd.Body); err != nil {
		// 响应头已发出，改不了状态码，只能记日志：客户端会看到被截断的流。
		log.Printf("agent: 文件正文发送中断 (%s): %v", rd.Meta.Path, err)
	}
}

// contentPut 实现 PUT /api/v1/files/content?path=：流式上传。
//
// 请求体就是文件内容（不套信封）；写前自动备份，备份的相对路径在响应里回传。
func (a *filesAPI) contentPut(w http.ResponseWriter, r *http.Request) {
	q, ok := fileQuery(w, r)
	if !ok {
		return
	}
	idx, err := a.rootIndex(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	res, err := a.g.Write(idx, q.Get("path"), r.Body)
	if err != nil {
		filesErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// action 实现 POST /api/v1/files/{action}，body {"action","path","dest"}。
//
// 动作名有两个来源（路径段与请求体），必须一致：不一致说明调用方自己没想清楚，
// 这时选任何一个都可能做错事——直接 400，不猜。
func (a *filesAPI) action(w http.ResponseWriter, r *http.Request) {
	q, ok := fileQuery(w, r)
	if !ok {
		return
	}
	idx, err := a.rootIndex(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}

	var body protocol.FileAction
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体非法")
		return
	}
	seg := r.PathValue("action")
	if body.Action != seg {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest,
			fmt.Sprintf("动作名不一致：路径为 %q，请求体为 %q", seg, body.Action))
		return
	}
	if !protocol.ValidFileAction(seg) {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "不支持的动作")
		return
	}

	var out protocol.FileActionDone
	switch seg {
	case protocol.ActionMkdir:
		out, err = a.g.Mkdir(idx, body.Path)
	case protocol.ActionDelete:
		out, err = a.g.Delete(idx, body.Path)
	case protocol.ActionRename:
		if body.Dest == "" {
			httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "rename 需要 dest")
			return
		}
		out, err = a.g.Rename(idx, body.Path, body.Dest)
	}
	if err != nil {
		filesErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
