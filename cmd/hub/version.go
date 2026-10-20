// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/version"
)

// systemVersion 回显 Hub 构建元数据（version / commit / date）。
//
// 值由 Makefile -ldflags 注入 internal/version；未注入的本地构建回退占位值
// （version="dev"、commit/date="unknown"），制品镜像由 CI 注入 tag 真值。
//
// **不写审计**：版本查询是设置页每次进入都会拉的只读信息，记审计只会产生
// 噪音（与 audit_logs 查询端点同口径——查询自身不写审计）。
func (s *server) systemVersion(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"version": version.Version,
		"commit":  version.Commit,
		"date":    version.Date,
	})
}
