// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.1：镜像接口的 Hub 转发层。handler 只做参数解析与状态码映射，
// 与 Agent 的通信全部走 agentcli，本文件不碰 SQL——唯一的库访问是
// ServerConn 取连接信息（agentFor，与容器转发共用）。
//
// 端点清单（ARCHITECTURE.md「镜像」行）：GET images / POST images/pull /
// DELETE images/{ref} / POST images/prune，scope 复用 read/write:containers
// （ARCHITECTURE.md 已定义「容器/镜像/Compose 清单只读」归 read:containers，
// 不为镜像新增 scope）。

package main

import (
	"fmt"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// parseDangling 解析 ?dangling=。严格口径同 parseAll：只认 ""/false/0 与
// true/1——拼错响亮失败，而不是静默按 false（少给一个悬空镜像比报错难排查）。
func parseDangling(r *http.Request) (bool, error) {
	switch v := r.URL.Query().Get("dangling"); v {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	default:
		return false, fmt.Errorf("dangling 必须是 true/false（或 1/0），也可省略，got %q", v)
	}
}

// imageList 实现 GET /api/v1/servers/{id}/images?all=&dangling=。
// dangling=true 是 prune dry-run 两段式的第一段：前端先拉悬空清单展示，
// 用户确认后才发 POST prune（dry-run 的「展示」在这，「确认」在 UI）。
func (s *server) imageList(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	all, err := parseAll(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	dangling, err := parseDangling(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	list, err := cl.ImageList(r.Context(), all, dangling)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// imagePull 实现 POST /api/v1/servers/{id}/images/pull，body {"ref":...}。
// ref 走 body（含 "/" ":" "@" 的引用不放路径段）；agentcli 出门前校验
// 字符集，非法值 400 不出门。Agent 侧汇总式响应原样透传——
// {"Status":...,"Layers":[...],"Err":""}，流内业务失败已被 Agent 映射 502。
func (s *server) imagePull(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Ref string `json:"ref"`
	}
	if err := decodeOptionalBody(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	if body.Ref == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "ref 不能为空")
		return
	}
	res, err := cl.ImagePull(r.Context(), body.Ref)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// imageDelete 实现 DELETE /api/v1/servers/{id}/images/{ref}。
// {ref...} 尾通配符与 Agent 侧一字不差（含 "/" 的引用，如 library/nginx:latest）。
// 被容器占用的镜像 Engine 回 409，原样透传——强删是调用方决策链。
func (s *server) imageDelete(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	if err := cl.ImageDelete(r.Context(), r.PathValue("ref")); err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// imagePrune 实现 POST /api/v1/servers/{id}/images/prune。
// dry-run 两段式的第二段（真删）；响应是 Engine 的清理清单+回收空间，
// 原样透传——UI 展示「删了哪些、回收多少」。
func (s *server) imagePrune(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	res, err := cl.ImagePrune(r.Context())
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
