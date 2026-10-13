// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// seedServer 建一台受管服务器，返回 ID。
func (h *hub) seedServer(t *testing.T) int64 {
	t.Helper()
	res, err := h.st.DB.Exec(`INSERT INTO servers(name,base_url,token_cipher) VALUES("srv","https://a:9527","x")`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// seedSnapshot 写一份可断言的历史+实时快照。
func (h *hub) seedSnapshot(t *testing.T, id int64, ts time.Time) {
	t.Helper()
	m := &protocol.HostMetrics{
		CPU:    protocol.CPUStat{Cores: 2, UsagePercent: 33.3},
		Memory: protocol.MemoryStat{Total: 8192, Used: 2048},
	}
	containers := []protocol.ContainerStat{{ID: "c1", Name: "web", CPUPercent: 5.5, MemUsage: 64, MemLimit: 256, MemPercent: 25, PIDS: 2}}
	if err := h.st.SaveMetricsSnapshot(context.Background(), id, m, containers, ts); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertMetricsLatest(context.Background(), id, m, containers, ts); err != nil {
		t.Fatal(err)
	}
}

// itoa 便于拼路径。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestMetricsLatest 验证实时快照端点的五条路径：
// 正常 200、无采集 404、无 scope 403、服务器不存在 404、未认证 401。
func TestMetricsLatest(t *testing.T) {
	h := newHub(t)
	ro := h.token(t, "ro", apitoken.ScopeReadMetrics)
	id := h.seedServer(t)
	path := "/api/v1/servers/" + itoa(id) + "/metrics"

	check := func(name string, want int) {
		t.Helper()
		resp, body := h.get(t, path, ro, "")
		if resp.StatusCode != want {
			t.Fatalf("%s：got %d，want %d｜%s", name, resp.StatusCode, want, body)
		}
	}

	// 未采集过 → 明确 404，不留空图给前端猜。
	check("无快照", http.StatusNotFound)
	h.seedSnapshot(t, id, time.Now())
	check("正常", http.StatusOK)

	if resp, _ := h.get(t, path, h.token(t, "weak", apitoken.ScopeReadAudit), ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("无 scope 应 403，got %d", resp.StatusCode)
	}
	if resp, _ := h.get(t, "/api/v1/servers/9999/metrics", ro, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在应 404，got %d", resp.StatusCode)
	}
	if resp, _ := h.get(t, path, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未认证应 401，got %d", resp.StatusCode)
	}
}

// TestMetricsLatest_DataShape 验证成功响应的数据结构与快照一致。
func TestMetricsLatest_DataShape(t *testing.T) {
	h := newHub(t)
	ro := h.token(t, "ro", apitoken.ScopeReadMetrics)
	id := h.seedServer(t)
	h.seedSnapshot(t, id, time.Unix(1700000000, 0).UTC())

	resp, body := h.get(t, "/api/v1/servers/"+itoa(id)+"/metrics", ro, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，got %d %s", resp.StatusCode, body)
	}
	var env struct {
		Data struct {
			ServerID   int64            `json:"server_id"`
			Ts         string           `json:"ts"`
			Containers []map[string]any `json:"containers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v %s", err, body)
	}
	out := env.Data
	if out.ServerID != id {
		t.Errorf("server_id = %d，want %d", out.ServerID, id)
	}
	if out.Ts != "2023-11-14T22:13:20Z" {
		t.Errorf("ts = %q，want 2023-11-14T22:13:20Z", out.Ts)
	}
	if len(out.Containers) != 1 || out.Containers[0]["name"] != "web" {
		t.Errorf("containers 不符：%+v", out.Containers)
	}
}

// TestMetricsOverview 验证历史查询：窗口过滤、升序、容器点。
func TestMetricsOverview(t *testing.T) {
	h := newHub(t)
	ro := h.token(t, "ro", apitoken.ScopeReadMetrics)
	id := h.seedServer(t)

	now := time.Now()
	h.seedSnapshot(t, id, now.Add(-2*time.Hour))    // 默认窗口外
	h.seedSnapshot(t, id, now.Add(-30*time.Minute)) // 默认窗口内

	base := "/api/v1/servers/" + itoa(id) + "/metrics/overview"
	resp, body := h.get(t, base, ro, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，got %d %s", resp.StatusCode, body)
	}
	var env struct {
		Data struct {
			Points     []map[string]any `json:"points"`
			Containers []map[string]any `json:"containers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	out := env.Data
	// 默认 1 小时窗口：只有 30 分钟前那一份（含 1 行容器）。
	if len(out.Points) != 1 || len(out.Containers) != 1 {
		t.Fatalf("默认窗口应 1 host 点 + 1 容器点，got %d/%d：%s", len(out.Points), len(out.Containers), body)
	}

	// 显式 3h 窗口盖住两份，验证过滤与升序。
	q := "?from=" + now.Add(-3*time.Hour).UTC().Format(time.RFC3339) + "&to=" + now.UTC().Format(time.RFC3339)
	resp, body = h.get(t, base+q, ro, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，got %d %s", resp.StatusCode, body)
	}
	var env2 struct {
		Data struct {
			Points []struct {
				Ts string `json:"ts"`
			} `json:"points"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env2); err != nil {
		t.Fatal(err)
	}
	out2 := env2.Data
	if len(out2.Points) != 2 {
		t.Fatalf("3h 窗口应 2 点，got %d：%s", len(out2.Points), body)
	}
	if out2.Points[0].Ts >= out2.Points[1].Ts {
		t.Errorf("应为升序，got %v then %v", out2.Points[0].Ts, out2.Points[1].Ts)
	}
}

// TestMetricsOverview_BadParams 表驱动覆盖参数校验。
func TestMetricsOverview_BadParams(t *testing.T) {
	h := newHub(t)
	ro := h.token(t, "ro", apitoken.ScopeReadMetrics)
	id := h.seedServer(t)
	base := "/api/v1/servers/" + itoa(id) + "/metrics/overview"

	cases := []struct {
		name, q string
		want    int
	}{
		{"from 非法", "?from=yesterday", 400},
		{"to 非法", "?to=%2B1h", 400},
		{"to 早于 from", "?from=2026-10-14T08:00:00Z&to=2026-10-14T00:00:00Z", 400},
		{"limit 非法", "?limit=abc", 400},
		{"limit 非正", "?limit=0", 400},
	}
	for _, c := range cases {
		resp, body := h.get(t, base+c.q, ro, "")
		if resp.StatusCode != c.want {
			t.Errorf("%s：got %d，want %d｜%s", c.name, resp.StatusCode, c.want, body)
		}
	}
	if resp, _ := h.get(t, "/api/v1/servers/9999/metrics/overview", ro, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在应 404，got %d", resp.StatusCode)
	}
}
