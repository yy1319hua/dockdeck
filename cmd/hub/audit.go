// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S7.7：审计查询端点。audit_logs 自 S1 起只写不读（store 仅有 WriteAudit、
// 路由表无查询端点，read:audit scope 早已预留而无端点消费），本期补「可查」。
// 本文件只做参数解析与状态码映射，SQL 全在 store——与 metrics 历史同分工。

package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/store"
)

const (
	auditDefaultWindow = 7 * 24 * time.Hour // Q2.A：默认最近 7 天
	auditDefaultLimit  = 100                // Q1.A
	auditMaxLimit      = 1000
	// auditTSLayout 与表 DEFAULT（strftime('%Y-%m-%dT%H:%M:%SZ','now')）同构：
	// ts 是 TEXT，字典序即时间序，但只有同格式才可比——带毫秒或时区偏移的
	// RFC3339 与秒级 Z 后缀混比会错位（'.' < 'Z'），故解析后统一秒级 UTC。
	auditTSLayout = "2006-01-02T15:04:05Z"
)

// validActionPrefix 限定 action 前缀字符集：命名规范是「资源.动作」
// （小写字母/数字/点/连字符，见 middleware.Audit 各调用点）。顺带杜绝
// LIKE 通配符（%/_）被带进查询——否则 "container%" 会匹配到意外动作。
func validActionPrefix(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// auditLogs 实现 GET /api/v1/audit_logs（方案 Q1–Q4 全 A：倒序 + before_id
// 游标分页；from/to/server_id/action 前缀/result 四维过滤；查询自身不写审计）。
//
// 刻意不包 middleware.Audit（Q4）：查询/翻页/改过滤都是高频读，每次留痕
// 会用审计噪声淹掉真正的操作记录——「谁看过审计」这件事本身不审计。
func (s *server) auditLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var err error

	now := time.Now().UTC()
	to, from := now, now.Add(-auditDefaultWindow)
	if v := q.Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "from 不是合法的 RFC3339 时间")
			return
		}
	}
	if v := q.Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "to 不是合法的 RFC3339 时间")
			return
		}
	}
	if to.Before(from) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "to 早于 from")
		return
	}

	var serverID, beforeID int64
	if v := q.Get("server_id"); v != "" {
		if serverID, err = strconv.ParseInt(v, 10, 64); err != nil || serverID <= 0 {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "server_id 必须是正整数")
			return
		}
	}
	if v := q.Get("before_id"); v != "" {
		if beforeID, err = strconv.ParseInt(v, 10, 64); err != nil || beforeID < 0 {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "before_id 必须是非负整数")
			return
		}
	}

	action := q.Get("action")
	if !validActionPrefix(action) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request",
			"action 只能含小写字母、数字、点、连字符（前缀匹配）")
		return
	}
	result := q.Get("result")
	if result != "" && result != "ok" && result != "error" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "result 只能是 ok 或 error")
		return
	}

	limit := auditDefaultLimit
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit <= 0 {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "limit 必须是正整数")
			return
		}
		if limit > auditMaxLimit {
			limit = auditMaxLimit
		}
	}

	entries, hasMore, err := s.st.ListAuditLogs(r.Context(), store.AuditFilter{
		From:     from.UTC().Format(auditTSLayout),
		To:       to.UTC().Format(auditTSLayout),
		ServerID: serverID,
		Action:   action,
		Result:   result,
		BeforeID: beforeID,
		Limit:    limit,
	})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "读取审计日志失败")
		return
	}

	// 空清单编码为 [] 而非 null（protocol 约定，S3.5 实测教训）；
	// 有更多时游标取本页最后一条的 id（列表已按 id 倒序）。
	logs := make([]protocol.AuditLog, 0, len(entries))
	var next int64
	for _, e := range entries {
		logs = append(logs, protocol.AuditLog{
			ID:        e.ID,
			Ts:        e.Ts,
			UserID:    e.UserID,
			Username:  e.Username,
			ServerID:  e.ServerID,
			Action:    e.Action,
			Target:    e.Target,
			Detail:    e.Detail,
			Result:    e.Result,
			Error:     e.Error,
			IP:        e.IP,
			RequestID: e.RequestID,
		})
		next = e.ID
	}
	if !hasMore {
		next = 0
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.AuditLogsPage{Logs: logs, NextBefore: next})
}
