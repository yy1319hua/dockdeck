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
