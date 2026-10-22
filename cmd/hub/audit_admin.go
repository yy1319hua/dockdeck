// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

// 审计日志手动清理（#89）：POST /api/v1/admin/audit/prune，仅会话可调。
// 自动清理在 metricsync 轮里按 --audit-retention-days 跑；本端点给用户
// 「现在就要空间」的手动入口。动作本身由 middleware.Audit 记 admin.audit_prune。

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// auditPruneMinDays 是 before_days 的下限：清理审计是低频运维动作，
// 太小的窗口更像手滑（真要清光请走整库恢复换空库）。
const auditPruneMinDays = 7

func (s *server) serverAuditPrune(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BeforeDays int `json:"before_days"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不合法")
		return
	}
	if body.BeforeDays < auditPruneMinDays {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest,
			"before_days 至少为 "+strconv.Itoa(auditPruneMinDays)+"（防手滑）")
		return
	}
	n, err := s.st.PruneAuditLogs(r.Context(), time.Duration(body.BeforeDays)*24*time.Hour, time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "目标不存在")
		return
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "清理审计日志失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": n, "before_days": body.BeforeDays})
}
