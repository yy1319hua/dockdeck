// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/joinauth"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/store"
)

const (
	defaultInviteTTL = 30 * time.Minute
	maxInviteTTL     = 24 * time.Hour
)

// serverInvite 签发一枚一次性 join_secret，供 Agent 容器启动时自注册。
// 仅浏览器会话可调（与 Token 管理同口径：API Token 不得自签邀请，否则权限收不回来）。
func (s *server) serverInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NameHint string `json:"name_hint"`
		TTLMin   int    `json:"ttl_min"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体解析失败")
		return
	}
	ttl := defaultInviteTTL
	if body.TTLMin > 0 {
		ttl = time.Duration(body.TTLMin) * time.Minute
		if ttl > maxInviteTTL {
			ttl = maxInviteTTL
		}
	}
	plain, err := s.st.CreateInvite(r.Context(), body.NameHint, ttl)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "签发邀请失败")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"join_secret": plain,
		"expires_in":  int(ttl.Seconds()),
		"warning":     "join_secret 仅此一次可见；将其作为 DD_AGENT_JOIN_SECRET / DD_AGENT_TOKEN 注入 Agent 容器即可完成纳管。",
	})
}

// serverEnroll 由 Agent 启动时调用：认领 join_secret、写入 servers 行。幂等。
// 请求体可选带 arch / docker_ver（Agent 上报的 GOARCH 与 Engine 版本），写入 servers 行
// 供服务器列表展示；缺失时留空，不阻断纳管。
// 审计在本端点内完成（要记录生成的 server_id，middleware.Audit 从路径取不到），故不套审计中间件。
func (s *server) serverEnroll(w http.ResponseWriter, r *http.Request) {
	secret, ok := joinauth.Secret(r)
	if !ok {
		// 理论不达：AuthJoin 中间件已保证； defensive。
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "需 join_secret")
		return
	}
	var body struct {
		Name      string `json:"name"`
		BaseURL   string `json:"base_url"`
		Arch      string `json:"arch"`
		DockerVer string `json:"docker_ver"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体解析失败")
		return
	}
	id, err := s.st.EnrollServer(r.Context(), secret, body.Name, body.BaseURL, body.Arch, body.DockerVer)
	s.writeEnrollAudit(r, id, body.Name, err)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInviteExpired):
			httpx.WriteError(w, http.StatusGone, "invite_expired", "邀请已过期或已不可用，请重新签发")
		case errors.Is(err, store.ErrInviteNotFound):
			httpx.WriteError(w, http.StatusUnauthorized, "invalid_join_secret", "join_secret 无法识别")
		case errors.Is(err, store.ErrServerNameTaken):
			httpx.WriteError(w, http.StatusConflict, protocol.CodeConflict, "同名服务器已存在")
		default:
			httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "纳管失败")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "name": body.Name})
}

func (s *server) writeEnrollAudit(r *http.Request, id int64, name string, err error) {
	res := "ok"
	var es string
	if err != nil {
		res = "error"
		es = err.Error()
	}
	_ = s.st.WriteAudit(r.Context(), store.AuditEntry{
		ServerID: id, Action: "server.enroll", Result: res, Error: es,
		Target: name, IP: httpx.ClientIP(r), RequestID: httpx.RequestID(r.Context()),
	})
}
