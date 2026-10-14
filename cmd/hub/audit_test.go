// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 审计覆盖面回归（容器全线之后的补全）：指标读端点与 Token 写端点。
// auth 的 login/logout/setup/password 是 handler 内自审计（internal/auth
// 的 m.audit），不在此重复；这里锁住的是曾经漏掉的那批端点——
// S2.7 指标（读）与 Token 签发/撤销（写），口径与容器 5 端点一致：
// 读也审（「谁看过哪台机器」同样是安全审计对象），失败也留痕。

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
)

// TestHubMetricsWritesAudit 验证指标读端点纳入全量审计：latest 与
// overview 分别落 metrics.latest / metrics.overview，server_id 从 {id}
// 自动带上，身份取自觉银主体（Token 创建者），失败（服务器不存在 404）
// 也留痕。
func TestHubMetricsWritesAudit(t *testing.T) {
	h := newHub(t)
	tok := h.token(t, "ro", apitoken.ScopeReadMetrics)
	id := h.seedServer(t)
	h.seedSnapshot(t, id, time.Now())
	spath := "/api/v1/servers/" + itoa(id)

	if resp, body := h.get(t, spath+"/metrics", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("latest 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	if resp, body := h.get(t, spath+"/metrics/overview", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("overview 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	// 失败也留痕：服务器不存在 404（探针行为同样值得留痕）。
	if resp, _ := h.get(t, "/api/v1/servers/9999/metrics", tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在应 404，得 %d", resp.StatusCode)
	}

	rows := auditRows(t, h)
	if len(rows) != 3 {
		t.Fatalf("应落 3 条审计，得 %d：%+v", len(rows), rows)
	}
	// 新的在前：latest(404 error) → overview(ok) → latest(ok)。
	want := []auditRow{
		{Action: "metrics.latest", Result: "error", ServerID: 9999, Username: "admin", Target: "/api/v1/servers/9999/metrics"},
		{Action: "metrics.overview", Result: "ok", ServerID: id, Username: "admin", Target: spath + "/metrics/overview"},
		{Action: "metrics.latest", Result: "ok", ServerID: id, Username: "admin", Target: spath + "/metrics"},
	}
	for i, w := range want {
		g := rows[i]
		if g.Action != w.Action || g.Result != w.Result || g.ServerID != w.ServerID ||
			g.Username != w.Username || g.Target != w.Target {
			t.Errorf("审计行 %d 不符：\ngot  %+v\nwant %+v", i, g, w)
		}
		if g.RequestID == "" {
			t.Errorf("审计行 %d 缺 request_id（与响应头 X-Request-Id 同源，排障要对得上）", i)
		}
	}
}

// TestHubTokenWritesAudit 验证 Token 写端点纳入全量审计：签发与撤销分别落
// token.create / token.revoke，身份是会话主体（这两个端点仅会话可调），
// 失败（重复撤销 404）也留痕。
//
// server_id 必须全部是 0：{id} 在这两个路径里是 Token ID 不是服务器 ID，
// serverIDFromPath 只认 /api/v1/servers/ 前缀（middleware 包前缀守卫）——
// 否则审计行会显示「动了 3 号服务器」而 3 其实是一枚 Token。Token ID
// 不丢：Target 列带着完整路径。
func TestHubTokenWritesAudit(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)

	resp, body := h.do(t, http.MethodPost, "/api/v1/api-tokens",
		`{"name":"audited","scopes":["read:metrics"],"ttl":"30d"}`, "", cookie)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("签发应 201，得 %d｜%s", resp.StatusCode, body)
	}
	// 明文只出现这一次，但 ID 在响应里——撤销要用。
	// 响应是信封：{"data":{"token":{...},"plaintext":"..."}}。
	var created struct {
		Data struct {
			Token struct {
				ID int64 `json:"id"`
			} `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("解析签发响应: %v", err)
	}
	if created.Data.Token.ID <= 0 {
		t.Fatalf("签发响应里拿不到 Token ID：%s", body)
	}
	tpath := "/api/v1/api-tokens/" + strconv.FormatInt(created.Data.Token.ID, 10)

	if resp, body := h.do(t, http.MethodDelete, tpath, "", "", cookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("撤销应 200，得 %d｜%s", resp.StatusCode, body)
	}
	// 失败也留痕：同一 ID 再撤一次即 404。
	if resp, _ := h.do(t, http.MethodDelete, tpath, "", "", cookie); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复撤销应 404，得 %d", resp.StatusCode)
	}

	rows := auditRows(t, h)
	// 4 行而不是 3 行：h.session() 走真实登录流程，auth.login 的自审计
	// （internal/auth 的 m.audit）也会落一行——排在最前（最新）。这条
	// 意外的行是好事：它证明会话建立本身就在审计里，链路没断。
	if len(rows) != 4 {
		t.Fatalf("应落 4 条审计（3 条 Token + 1 条登录），得 %d：%+v", len(rows), rows)
	}
	// 新的在前：revoke(404 error) → revoke(ok) → create(ok) → login(ok)。
	want := []auditRow{
		{Action: "token.revoke", Result: "error", ServerID: 0, Username: "admin", Target: tpath},
		{Action: "token.revoke", Result: "ok", ServerID: 0, Username: "admin", Target: tpath},
		{Action: "token.create", Result: "ok", ServerID: 0, Username: "admin", Target: "/api/v1/api-tokens"},
		{Action: "auth.login", Result: "ok", ServerID: 0, Username: "admin"},
	}
	for i, w := range want {
		g := rows[i]
		if g.Action != w.Action || g.Result != w.Result || g.ServerID != w.ServerID ||
			g.Username != w.Username || g.Target != w.Target {
			t.Errorf("审计行 %d 不符：\ngot  %+v\nwant %+v", i, g, w)
		}
		if g.RequestID == "" {
			t.Errorf("审计行 %d 缺 request_id", i)
		}
	}
}
