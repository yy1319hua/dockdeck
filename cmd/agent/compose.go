// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.2：Compose 端点（Agent 侧）。只做三件事：list 列举白名单内项目、
// action 执行白名单写动作、logs 流日志。所有宿主侧的 exec 都收在
// dockercli.ComposeOperator 接口后面（真实是 ComposeClient，测试换假实现）。
//
// 白名单由两个关卡守住：① ListProjects 只在配置的目录内递归发现；②
// ResolveProject 把项目名映射到白名单内真实路径，白名单外的名字（含 ../ 穿越）
// 一律 404。Agent 持有 ComposeDirs 配置，所以白名单判定发生在 Agent 这一层，
// 而非 Hub——Hub 只转发项目名，拿不到白名单外的路径去 exec。

package main

import (
	"io"
	"log"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

type composeAPI struct {
	op dockercli.ComposeOperator
}

// newComposeAPI 用配置好的白名单目录装配真实客户端。
func newComposeAPI(dirs []string) *composeAPI {
	return &composeAPI{op: dockercli.NewComposeClient(dirs)}
}

// list 实现 GET /api/v1/compose。
func (a *composeAPI) list(w http.ResponseWriter, r *http.Request) {
	projects, err := a.op.ListProjects(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeUpstreamError, "列举 compose 项目失败: "+err.Error())
		return
	}
	if projects == nil {
		projects = []protocol.ComposeProject{}
	}
	httpx.WriteJSON(w, http.StatusOK, projects)
}

// action 实现 POST /api/v1/compose/{project}/{action}。
func (a *composeAPI) action(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	action := r.PathValue("action")
	if !protocol.ValidComposeAction(action) {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "不支持的 compose 动作: "+action)
		return
	}
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	var (
		out string
		err error
	)
	switch protocol.ComposeAction(action) {
	case protocol.ComposeUp:
		out, err = a.op.Up(r.Context(), p.ConfigFile)
	case protocol.ComposeDown:
		out, err = a.op.Down(r.Context(), p.ConfigFile)
	case protocol.ComposeRestart:
		out, err = a.op.Restart(r.Context(), p.ConfigFile)
	case protocol.ComposePull:
		out, err = a.op.Pull(r.Context(), p.ConfigFile)
	}
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "compose "+action+" 失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeActionResult{Output: out})
}

// logs 实现 GET /api/v1/compose/{project}/logs?follow=1。
// 沿用容器日志的流通道：逐帧 Flush，客户端断开即结束。
func (a *composeAPI) logs(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	follow := r.URL.Query().Get("follow") == "1"
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	rc, err := a.op.Logs(r.Context(), p.ConfigFile, follow)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "打开 compose 日志流失败: "+err.Error())
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	if _, err := io.Copy(flushWriter{w, flusher}, rc); err != nil {
		// 流中途断掉：状态码已发出，只能记日志。前端以连接关闭为结束信号。
		log.Printf("agent: compose 日志流中断 (%s=%q): %v", protocol.HeaderRequestID, r.Header.Get(protocol.HeaderRequestID), err)
	}
}
