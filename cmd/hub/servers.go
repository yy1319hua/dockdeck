// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"errors"
	"log"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// serverDelete 实现 DELETE /api/v1/servers/{id}：物理删除一台受管服务器。
//
// 仅浏览器会话可调（AuthSession，与纳管邀请/整库备份同口径：管理操作不开放
// API Token——Token 面向采集与只读场景，删机器这种不可逆动作必须人盯着）。
// 级联语义见 store.DeleteServer 注释；删除后同名可立即重新 enroll。
func (s *server) serverDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseServerID(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "非法服务器 ID")
		return
	}
	name, err := s.st.DeleteServer(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "服务器不存在")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "删除服务器失败")
		return
	}
	// 反向长连接（若有）随删除一并收掉：否则连接注册表里留一条指向
	// 已不存在服务器的活连接，ws 与读写 goroutine 双双泄漏。
	// 防空：测试装配（newHub）不初始化 links，生产（main.go）必有。
	if s.links != nil {
		if c := s.links.Conn(id); c != nil {
			s.links.Detach(c)
		}
	}
	log.Printf("hub: 已删除受管服务器 #%d（%s）", id, name)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "name": name})
}

// serverRename 实现 PATCH /api/v1/servers/{id}：改服务器显示名（#86）。
//
// 仅浏览器会话可调（与删除同口径：管理操作不开放 API Token）。名字是 Hub 侧
// 展示标识，与 Agent 连接无关；重名 409（servers.name 唯一），404 不存在。
// 审计由 middleware.Audit 记 server.rename。
func (s *server) serverRename(w http.ResponseWriter, r *http.Request) {
	id, err := parseServerID(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "非法服务器 ID")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不合法")
		return
	}
	name, err := s.st.RenameServer(r.Context(), id, body.Name)
	switch {
	case errors.Is(err, store.ErrServerNameTaken):
		httpx.WriteError(w, http.StatusConflict, protocol.CodeConflict, "同名服务器已存在")
		return
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "服务器不存在")
		return
	case err != nil:
		// 校验类错误（空名/超长）与内部错误都走这里：文案区分交给前缀判断太脆，
		// 直接把 store 的校验原文回给调用方（store 错误本身就是中文人话）。
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, humanizeAgentMessage(err.Error()))
		return
	}
	log.Printf("hub: 服务器 #%d 改名为 %q", id, name)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "name": name})
}


// serverDeploy 实现 GET /api/v1/servers/{id}/deploy：返回该服务器原 Agent 令牌
// 与连接信息，供面板「重新生成部署配置」用。
//
// 仅浏览器会话可调（AuthSession，与纳管邀请/删除/改名同口径：管理操作不开放 API Token）。
// 返回的 token 是 Agent 的长期共享令牌（认领邀请后写入 servers.token_cipher 的明文），
// 用它生成的 compose / docker run 重新部署后，Agent 启动时 enroll 会命中既有
// claimed_at + server_id 幂等分支——仍是同一台服务器，不会新增。
//
// 安全：等同于把该机控制权交到浏览器。故加审计 server.deploy_view 追溯谁何时查看；
// token 不进 ServerSummary 列表、也不进其它只读接口。
func (s *server) serverDeploy(w http.ResponseWriter, r *http.Request) {
	id, err := parseServerID(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "非法服务器 ID")
		return
	}
	conn, err := s.st.ServerConn(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "服务器不存在")
		return
	case errors.Is(err, store.ErrServerDisabled):
		httpx.WriteError(w, http.StatusForbidden, protocol.CodeForbidden, "服务器已禁用")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "读取服务器连接信息失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"token":    conn.Token,
		"name":     conn.Name,
		"base_url": conn.BaseURL,
	})
}
