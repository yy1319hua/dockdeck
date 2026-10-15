// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S5.5(3/5)：文件管理接口的 Hub 转发层（Agent 侧 S5.1–S5.4/S5.6 已落地）。
// Hub 这一层只做鉴权、审计与转发——与容器/镜像/Compose 转发同款：
// handler 只解析参数与映射错误，与 Agent 的通信全部走 agentcli，本文件不碰 SQL。
//
// 与 Agent 侧的分工：路径白名单、realpath 解引用、写前备份、敏感黑名单全在 Agent 侧，
// Hub 只转发 root/path/action/dest 与文件内容，拿不到也猜不到宿主机路径，
// 所以这里**不复制任何白名单或路径校验**——复制就等于给「两侧口径漂移」开口子。
//
// 上传正文的上限必须用 protocol.MaxFileUploadBytes：Hub 要把 body 读进内存才能对
// 整份验签，上限即单请求内存占用；与 Agent 侧同源，避免「Hub 放行、Agent 413」。

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// fileRootOf 解析 ?root=（缺省 0）。越界由 Agent 侧最后防线拦截，这里只做基本形态校验。
func fileRootOf(q url.Values) (int, error) {
	raw := q.Get("root")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New("root 需为非负整数")
	}
	return n, nil
}

// filePathOf 取 ?path=。允许空（表示根自身，合法查询而非缺参数）。
func filePathOf(q url.Values) string {
	return q.Get("path")
}

// fileDepthOf 解析 ?depth=（缺省 1，只列直接子项）。
func fileDepthOf(q url.Values) (int, error) {
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

// fileList 实现 GET /api/v1/servers/{id}/files?root=&path=&depth=。
func (s *server) fileList(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "查询串非法")
		return
	}
	root, err := fileRootOf(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	depth, err := fileDepthOf(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	list, err := cl.FileList(r.Context(), root, filePathOf(q), depth)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// fileStat 实现 GET /api/v1/servers/{id}/files/stat?root=&path=。
func (s *server) fileStat(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "查询串非法")
		return
	}
	root, err := fileRootOf(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	meta, err := cl.FileStat(r.Context(), root, filePathOf(q))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, meta)
}

// fileContent 实现 GET /api/v1/servers/{id}/files/content?root=&path=。
// 裸流透传，与容器日志同款三处处理：Content-Type 从 Agent 响应透传、不设 Hub 侧读
// 超时（下载大文件同样不能掐）、逐块 Flush（让前端边下边渲染）。
func (s *server) fileContent(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "查询串非法")
		return
	}
	root, err := fileRootOf(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	st, err := cl.FileContent(r.Context(), root, filePathOf(q))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	defer st.Body.Close()

	h := w.Header()
	if st.ContentType != "" {
		h.Set("Content-Type", st.ContentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	if st.Path != "" {
		h.Set("X-DD-File-Path", st.Path)
	}
	if st.Size > 0 {
		h.Set("X-DD-File-Size", strconv.FormatInt(st.Size, 10))
	}
	if st.Truncated {
		h.Set("X-DD-Truncated", "1")
	}
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := st.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				log.Printf("hub: 文件正文流异常中断 %s: %v", r.URL.Path, rerr)
			}
			return
		}
	}
}

// fileContentPut 实现 PUT /api/v1/servers/{id}/files/content?root=&path=。
// 正文即文件内容（不经信封）；Hub 先把 body 读进内存并以共享上限 MaxFileUploadBytes
// 拦下超限——与 Agent 侧同源（验签要持完整 body），上限漂移会冒出「Hub 放行、Agent 413」。
func (s *server) fileContentPut(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "查询串非法")
		return
	}
	root, err := fileRootOf(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	content, err := readCappedBody(r, protocol.MaxFileUploadBytes)
	if err != nil {
		// 超限是出门前的本地拒绝：用协议错误码，与 Agent 侧同口径（413）。
		var te *protocol.Error
		if errors.As(err, &te) {
			httpx.WriteError(w, protocol.StatusForCode(te.Code), te.Code, te.Message)
		} else {
			httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		}
		return
	}
	res, err := cl.FileWrite(r.Context(), root, filePathOf(q), content)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// fileAction 实现 POST /api/v1/servers/{id}/files/{action}?root=。
// 动作名取 URL 段（与 Agent 侧路由一致），body 携带 {action,path,dest}；Agent 侧要求
// 二者一致，故这里把 body.Action 统一填成 URL 段，避免「路径动作」与「体动作」分歧。
// 非法动作由 agentcli 在出门前校验（400，不出门）。
func (s *server) fileAction(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "查询串非法")
		return
	}
	root, err := fileRootOf(q)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	var body protocol.FileAction
	if err := decodeOptionalBody(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	body.Action = action // 与 URL 段对齐，满足 Agent 侧「动作名一致」校验
	done, err := cl.FileAction(r.Context(), root, action, body.Path, body.Dest)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, done)
}

// readCappedBody 把请求体读进内存，超过 max 字节返回 413 协议错误。
// 用于文件上传：正文必须整体验签，故上限即单请求内存上界。
func readCappedBody(r *http.Request, max int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, protocol.NewError(protocol.CodeTooLarge,
			fmt.Sprintf("上传内容 %d 字节超过单文件上限 %d 字节", len(raw), max))
	}
	return raw, nil
}
