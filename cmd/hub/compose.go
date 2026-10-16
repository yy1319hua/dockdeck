// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.7：Compose 接口的 Hub 转发层。Agent 侧端点已在 S4.2（list/action/logs）
// 与 S4.3（YAML 读写）落地，Hub 这一层只做鉴权、审计与转发——与镜像转发同款：
// handler 只解析参数与映射错误，与 Agent 的通信全部走 agentcli，本文件不碰 SQL
// （唯一的库访问是 agentFor 取连接信息）。
//
// 白名单判定在 Agent 侧（ComposeDirs）：Hub 只转发项目名，拿不到也猜不到宿主机
// 路径，所以这里**不复制一份白名单**——复制就等于给「两侧口径漂移」开口子。

package main

import (
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// composeList 实现 GET /api/v1/servers/{id}/compose。
// 空清单由 Agent 侧保证是 [] 非 null，Hub 原样透传不加工。
func (s *server) composeList(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	list, err := cl.ComposeList(r.Context())
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// composeAction 实现 POST /api/v1/servers/{id}/compose/{project}/{action}。
// 非法动词由 agentcli 在出门前拦下（400，不出门）；未知项目由 Agent 404。
func (s *server) composeAction(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	res, err := cl.ComposeAction(r.Context(), r.PathValue("project"), r.PathValue("action"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// composeLogs 实现 GET /api/v1/servers/{id}/compose/{project}/logs?follow=。
// 裸流，与容器日志同款三处处理：内容类型从 Agent 响应透传、不设 Hub 侧读超时
// （流的寿命归调用方 ctx，断开即取消对 Agent 的请求）、逐块 Flush（follow 才滚动）。
func (s *server) composeLogs(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	st, err := cl.ComposeLogs(r.Context(), r.PathValue("project"), r.URL.Query().Get("follow") == "1")
	if err != nil {
		writeAgentError(w, err)
		return
	}
	defer st.Body.Close()

	h := w.Header()
	if st.ContentType != "" {
		h.Set("Content-Type", st.ContentType)
	} else {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := st.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 调用方断开：写不出去即结束，状态码已发无从补救
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			// 流中途断掉（Agent 重启、网络抖动）：响应头已发出，无法再用状态码
			// 表达，记日志，前端以连接关闭为结束信号。
			if !errors.Is(rerr, io.EOF) {
				log.Printf("hub: compose 日志流异常中断 %s: %v", r.URL.Path, rerr)
			}
			return
		}
	}
}

// composeFileGet 实现 GET /api/v1/servers/{id}/compose/{project}/file。
// 返回 compose 文件的绝对路径与原文；只读，写走下面的 diff 预览 + PUT。
func (s *server) composeFileGet(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	res, err := cl.ComposeFileGet(r.Context(), r.PathValue("project"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// composeFileDiff 实现 POST /api/v1/servers/{id}/compose/{project}/diff。
// 「先看 diff → 再确认写」两段式的第一段：只算不写，不落盘、不产生备份。
func (s *server) composeFileDiff(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	var body protocol.ComposeFileUpdate
	if err := decodeOptionalBody(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	res, err := cl.ComposeFileDiff(r.Context(), r.PathValue("project"), body.Content)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// composeFilePut 实现 PUT /api/v1/servers/{id}/compose/{project}/file。
// 写前备份由 Agent 侧完成（<file>.bak.<RFC3339>，备份落盘成功才动原文件），
// 备份路径随响应回传供回滚——Hub 不复制这段语义，避免两侧漂移。
func (s *server) composeFilePut(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	var body protocol.ComposeFileUpdate
	if err := decodeOptionalBody(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	res, err := cl.ComposeFilePut(r.Context(), r.PathValue("project"), body.Content)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// composeBackupsList 实现 GET /servers/{id}/compose/{project}/backups：备份清单（只读）。
// 预览不另开端点——前端拿备份正文后调既有的 composeFileDiff 算 diff，与
// 现有两段式写入完全同款（Q1-A）。
func (s *server) composeBackupsList(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	list, err := cl.ComposeBackups(r.Context(), r.PathValue("project"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeBackupsList{Backups: list})
}

// composeBackupsContent 实现 GET .../backups/content?file=<备份名>：读备份正文。
func (s *server) composeBackupsContent(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "缺少 file 参数（备份文件名）")
		return
	}
	res, err := cl.ComposeBackupContent(r.Context(), r.PathValue("project"), name)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// composeBackupsRestore 实现 POST .../backups/restore：用备份覆盖当前 compose 文件。
// 恢复是覆盖写，故挂 write:containers 而非 read（与写 YAML 同款）。
func (s *server) composeBackupsRestore(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	var body protocol.ComposeBackupUpdate
	if err := decodeOptionalBody(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	if body.File == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "缺少 file 字段（备份文件名）")
		return
	}
	res, err := cl.ComposeRestore(r.Context(), r.PathValue("project"), body.File)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
