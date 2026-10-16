// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/httpx"
)

// S7.9：Hub 侧卷与网络的转发。与容器/镜像转发同款——只做鉴权、审计、
// 取连接与转发，不重新实现任何 Docker 语义（白名单与规则都在 Agent 侧）。
//
// 两条都是读：scope 复用 `read:containers`（GAP-2 已定 Compose 无独立 scope，
// 卷与网络同族沿用），不新增 scope——加一个 scope 要连带改 Token 签发 UI
// 与 scope 字典，收益不抵成本。

// volumeList 实现 GET /api/v1/servers/{id}/volumes：数据卷清单（只读）。
func (s *server) volumeList(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	list, err := cl.VolumeList(r.Context())
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// networkList 实现 GET /api/v1/servers/{id}/networks：网络清单（只读）。
// 每项自带接入容器名单，前端不必再发请求去补。
func (s *server) networkList(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	list, err := cl.NetworkList(r.Context())
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}
