// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S2.7：监控查询端点。handler 只做参数解析与状态码映射，SQL 全在 store。

package main

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// overviewWindow 是 overview 的默认与最大查询窗口。
const (
	overviewDefault = time.Hour
	overviewMax     = 8 * 24 * time.Hour // 略大于 7 天保留期，查满全史
)

// parseServerID 解析并校验 {id} 路径参数。
func parseServerID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("store: 非法服务器 ID")
	}
	return id, nil
}

// metricsLatest 实现 GET /api/v1/servers/{id}/metrics。
// 返回实时快照；服务器不存在或还没有任何快照时 404——「没有数据」与
// 「查错对象」在监控语义里都该让调用方明确知道，而不是给一张空图。
func (s *server) metricsLatest(w http.ResponseWriter, r *http.Request) {
	id, err := parseServerID(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "非法服务器 ID")
		return
	}
	ok, err := s.st.ServerExists(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "查询服务器失败")
		return
	}
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "服务器不存在")
		return
	}
	m, containers, ts, err := s.st.LatestMetrics(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "尚无指标快照（轮询器未采集或 Agent 从未上线）")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "读取实时快照失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"server_id":  id,
		"ts":         ts.UTC().Format(time.RFC3339),
		"metrics":    m,
		"containers": containers, // nil 会编码为 null：前端据此区分「无容器指标」与「空列表」
	})
}

// metricsOverview 实现 GET /api/v1/servers/{id}/metrics/overview。
// 查询参数：from/to（RFC3339，可选，默认最近 1 小时，窗口上限 8 天）、limit（点数，默认 720）。
func (s *server) metricsOverview(w http.ResponseWriter, r *http.Request) {
	id, err := parseServerID(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "非法服务器 ID")
		return
	}
	ok, err := s.st.ServerExists(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "查询服务器失败")
		return
	}
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "服务器不存在")
		return
	}

	now := time.Now()
	to, from := now, now.Add(-overviewDefault)
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "from 不是合法的 RFC3339 时间")
			return
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "to 不是合法的 RFC3339 时间")
			return
		}
	}
	if to.Before(from) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "to 早于 from")
		return
	}
	if to.Sub(from) > overviewMax {
		from = to.Add(-overviewMax)
	}
	limit := 720
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit <= 0 {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "limit 必须是正整数")
			return
		}
	}

	points, err := s.st.MetricsHostRange(r.Context(), id, from, to, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "读取历史曲线失败")
		return
	}
	cpoints, err := s.st.MetricsContainerRange(r.Context(), id, from, to, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "读取容器历史失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"server_id":  id,
		"from":       from.UTC().Format(time.RFC3339),
		"to":         to.UTC().Format(time.RFC3339),
		"points":     points,
		"containers": cpoints,
	})
}
