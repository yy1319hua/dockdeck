// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S3.4 转发层的端到端测试：真库、真会话、真 Token、真 TLS 假 Agent
// （验签中间件不 mock）——Hub→agentcli→Agent 整条链路连同签名头一起跑。
// 断言落在三处：前端看到的响应、Agent 实际收到的请求（路径/请求体）、
// 以及「该拒的请求根本没出门」（本地校验先行的证据）。

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/fakeagent"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// hubWithAgent 装配一套「Hub + 假 Agent」：Hub 用 newHub 的真库真会话，
// 假 Agent 跑真 TLS 与真验签，Hub 经 agentcli 真转发过去。
// enabled=false 种禁用的服务器行（实时转发应 403）。
func hubWithAgent(t *testing.T, enabled bool) (*hub, int64, *fakeagent.Fake) {
	t.Helper()
	h := newHub(t)
	fa := fakeagent.New("agent-sekret")
	as := fa.Server()
	t.Cleanup(as.Close)

	cipher, err := crypto.SealSecret(h.st.Key, []byte("agent-sekret"))
	if err != nil {
		t.Fatalf("封存 Agent 令牌: %v", err)
	}
	on := 1
	if !enabled {
		on = 0
	}
	res, err := h.st.DB.ExecContext(context.Background(),
		`INSERT INTO servers(name, base_url, token_cipher, enabled) VALUES(?,?,?,?)`,
		"node1", as.URL, cipher, on)
	if err != nil {
		t.Fatalf("种服务器行: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取服务器 id: %v", err)
	}
	return h, id, fa
}

// jsonEqual 比较两段 JSON 的语义相等（忽略空白与键序差异）。
func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatalf("前者不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatalf("后者不是合法 JSON: %v", err)
	}
	return reflect.DeepEqual(x, y)
}

func TestHubContainerListForwardsToAgent(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetContainerList([]protocol.ContainerSummary{
		{ID: "abc123def456", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 3 hours"},
	})
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/containers"

	resp, body := h.get(t, path, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data []protocol.ContainerSummary `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if len(env.Data) != 1 || env.Data[0].ID != "abc123def456" || env.Data[0].Names[0] != "/web" {
		t.Errorf("Engine 投影应原样到达前端：%+v", env.Data)
	}
	// all 显式进查询串——它是签名路径的一部分，两侧必须字字一致。
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/containers?all=false" {
		t.Errorf("应带 all=false 打到 Agent，得 %v", calls)
	}

	if resp, _ := h.get(t, path+"?all=true", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("all=true 应 200，得 %d", resp.StatusCode)
	}
	if calls := fa.Calls(); len(calls) != 2 || calls[1] != "/api/v1/containers?all=true" {
		t.Errorf("all=true 应透传，得 %v", calls)
	}

	// 拼错要响亮失败：all=yes 拒且不出门。
	resp, body = h.get(t, path+"?all=yes", tok, "")
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
		t.Errorf("all=yes 应 400 bad_request，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if calls := fa.Calls(); len(calls) != 2 {
		t.Errorf("坏参数不应打到 Agent，得 %v", calls)
	}
}

func TestHubContainerInspectForwardsEngineJSON(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	engine := `{"Id":"abc123def456","Name":"/web","State":{"Running":true},"Config":{"Image":"nginx:1.27","Env":["A=B"]}}`
	fa.SetContainerInspect(map[string]json.RawMessage{"abc123def456": json.RawMessage(engine)})
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	base := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/containers/"

	resp, body := h.get(t, base+"abc123def456", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if !jsonEqual(t, engine, string(env.Data)) {
		t.Errorf("Engine JSON 应一个字段不改：\ngot  %s\nwant %s", env.Data, engine)
	}

	// 容器不存在：Agent 的 404 原样透传（not_found，不重试不伪装）。
	resp, body = h.get(t, base+"deadbeef", tok, "")
	if resp.StatusCode != http.StatusNotFound || errorCode(t, body) != protocol.CodeNotFound {
		t.Errorf("不存在的容器应 404 not_found，得 %d｜%s", resp.StatusCode, trim(body))
	}

	// 非法 cid 本地就拒，请求不出门（取能穿过 mux 路径清洗的形态：
	// 空格与超长；dot-segment 到不了 handler，见下一个断言）。
	for _, bad := range []string{"a%20b", strings.Repeat("a", 129)} {
		resp, body := h.get(t, base+bad, tok, "")
		if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
			t.Errorf("非法容器标识 %q 应 400，得 %d｜%s", bad, resp.StatusCode, trim(body))
		}
	}
	// dot-segment 在 HTTP 边界就被 mux 路径清洗掉（301 到清洗后路径），
	// 转发层永远看不到它们；agentcli 的 ValidContainerID 首字符规则是
	// 第二道（Hub 拼转发路径时再拦一次）。两层各自成立，互不依赖。
	if resp, _ := h.get(t, base+"..", tok, ""); resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("dot-segment 应被 mux 清洗（301），得 %d", resp.StatusCode)
	}
	if calls := fa.Calls(); len(calls) != 2 {
		t.Errorf("前两次之外不应再有请求打到 Agent，得 %v", calls)
	}
}

func TestHubContainerActionForwards(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	base := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/containers/abc123/"

	// 无选项动作：路径与 Agent 路由一字不差，不带请求体。
	resp, body := h.do(t, http.MethodPost, base+"restart", "", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/containers/abc123/restart" {
		t.Errorf("转发路径不符，得 %v", calls)
	}
	if bodies := fa.WriteBodies(); len(bodies) != 1 || bodies[0] != "" {
		t.Errorf("无选项动作不应带请求体，得 %v", bodies)
	}

	// stop 的 timeout_sec 走请求体。
	if resp, _ := h.do(t, http.MethodPost, base+"stop", `{"timeout_sec":7}`, tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("stop 应 200，得 %d", resp.StatusCode)
	}
	if bodies := fa.WriteBodies(); bodies[len(bodies)-1] != `{"timeout_sec":7}` {
		t.Errorf("timeout_sec 应透传到 Agent，得 %q", bodies[len(bodies)-1])
	}

	// 显式 0 = 立即停止（Docker 语义），必须原样抵达 Agent，不能被当成
	// 「没有选项」静默丢弃——那条路上它会悄悄变成引擎默认 10s。
	if resp, _ := h.do(t, http.MethodPost, base+"stop", `{"timeout_sec":0}`, tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("stop(0) 应 200，得 %d", resp.StatusCode)
	}
	if bodies := fa.WriteBodies(); bodies[len(bodies)-1] != `{"timeout_sec":0}` {
		t.Errorf("timeout_sec=0 应原样透传，得 %q", bodies[len(bodies)-1])
	}

	// kill 的信号规范化后走请求体（sigkill → SIGKILL；白名单在 protocol 单点）。
	if resp, _ := h.do(t, http.MethodPost, base+"kill", `{"signal":"sigkill"}`, tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("kill 应 200，得 %d", resp.StatusCode)
	}
	if bodies := fa.WriteBodies(); bodies[len(bodies)-1] != `{"signal":"SIGKILL"}` {
		t.Errorf("signal 应规范化后透传，得 %q", bodies[len(bodies)-1])
	}

	// 白名单外信号 400，不出门。
	before := len(fa.Calls())
	resp, body = h.do(t, http.MethodPost, base+"kill", `{"signal":"SIGEVIL"}`, tok, "")
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
		t.Errorf("非法信号应 400，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if len(fa.Calls()) != before {
		t.Error("非法信号不应打到 Agent")
	}

	// resize 的行列走查询串（Engine 原生约定）。
	if resp, _ := h.do(t, http.MethodPost, base+"resize?h=40&w=120", "", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("resize 应 200，得 %d", resp.StatusCode)
	}
	if calls := fa.Calls(); calls[len(calls)-1] != "/api/v1/containers/abc123/resize?h=40&w=120" {
		t.Errorf("resize 查询串不符，得 %q", calls[len(calls)-1])
	}
	resp, body = h.do(t, http.MethodPost, base+"resize?h=40", "", tok, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("resize 缺 w 应 400，得 %d｜%s", resp.StatusCode, trim(body))
	}

	// 未知动作 404（与 Agent 侧同款：没有这个子资源）。
	resp, body = h.do(t, http.MethodPost, base+"launch", "", tok, "")
	if resp.StatusCode != http.StatusNotFound || errorCode(t, body) != protocol.CodeNotFound {
		t.Errorf("未知动作应 404 not_found，得 %d｜%s", resp.StatusCode, trim(body))
	}

	// 请求体多字段 400：typo 不静默吞成默认行为。
	resp, body = h.do(t, http.MethodPost, base+"stop", `{"timeout_sec":7,"grace":true}`, tok, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("未知字段应 400，得 %d｜%s", resp.StatusCode, trim(body))
	}

	// Agent 的自我保护守卫 403 原样到达（不碰自身红线透传给调用方）。
	fa.SetSelfID("abc123")
	resp, body = h.do(t, http.MethodPost, base+"restart", "", tok, "")
	if resp.StatusCode != http.StatusForbidden || errorCode(t, body) != protocol.CodeForbidden {
		t.Errorf("自身容器守卫应透传 403 forbidden，得 %d｜%s", resp.StatusCode, trim(body))
	}
}

func TestHubContainerRemoveForwards(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	base := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/containers/abc123"

	resp, body := h.do(t, http.MethodDelete, base, "", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/containers/abc123" {
		t.Errorf("转发路径不符，得 %v", calls)
	}

	// force/v 查询串透传（url.Values 编码后进签名路径）。
	if resp, _ := h.do(t, http.MethodDelete, base+"?force=1&v=1", "", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("force+v 应 200，得 %d", resp.StatusCode)
	}
	if calls := fa.Calls(); calls[len(calls)-1] != "/api/v1/containers/abc123?force=1&v=1" {
		t.Errorf("force/v 查询串不符，得 %q", calls[len(calls)-1])
	}

	// 请求体 force/v 透传（并集口径：body 与 query 任一即生效）。
	// 注意到达 Agent 时统一走查询串——agentcli 的 remove 只发查询串，
	// Hub 侧请求体只是并入查询串的输入通道之一。
	if resp, _ := h.do(t, http.MethodDelete, base, `{"force":true,"volumes":true}`, tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("body remove 应 200，得 %d", resp.StatusCode)
	}
	if calls := fa.Calls(); calls[len(calls)-1] != "/api/v1/containers/abc123?force=1&v=1" {
		t.Errorf("body 里的 force/v 应并集进查询串，得 %q", calls[len(calls)-1])
	}

	// 自身守卫 403。
	fa.SetSelfID("abc123")
	resp, body = h.do(t, http.MethodDelete, base, "", tok, "")
	if resp.StatusCode != http.StatusForbidden || errorCode(t, body) != protocol.CodeForbidden {
		t.Errorf("自身容器守卫应透传 403，得 %d｜%s", resp.StatusCode, trim(body))
	}
}

func TestHubContainerLogsStreamsThrough(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetLogs("line-1\nline-2\n")
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	base := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/containers/abc123/logs"

	// 非 follow：合流文本原样过 Hub，内容类型透传。
	resp, body := h.get(t, base+"?tail=200", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("内容类型应透传 text/plain，得 %q", ct)
	}
	if string(body) != "line-1\nline-2\n" {
		t.Errorf("日志体应原样透传，得 %q", body)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/containers/abc123/logs?tail=200" {
		t.Errorf("tail 应显式透传，得 %v", calls)
	}

	// follow：长连接不被 Hub 掐断——agentcli 不对流施加读超时，
	// 5 帧心跳应全部到达。这是「流式代理免超时」的端到端证据。
	resp, body = h.get(t, base+"?follow=1", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	for i := 1; i <= 5; i++ {
		want := "--follow-tick-" + strconv.Itoa(i) + "--"
		if !strings.Contains(string(body), want) {
			t.Errorf("第 %d 帧心跳应到达（Hub 不得对流施加读超时），体=%q", i, body)
		}
	}

	// export：octet-stream + 下载文件名透传（docker CLI 可直接消费）。
	resp, body = h.get(t, base+"?export=1", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export 应 200，得 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("export 内容类型应透传，得 %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="abc123.log"` {
		t.Errorf("下载文件名应透传，得 %q", cd)
	}
	if string(body) != "line-1\nline-2\n" {
		t.Errorf("export 体应原样透传，得 %q", body)
	}

	// tail 非法：白名单两侧同源，400。
	resp, body = h.get(t, base+"?tail=abc", tok, "")
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
		t.Errorf("tail=abc 应 400，得 %d｜%s", resp.StatusCode, trim(body))
	}
}

// TestHubContainerAuthz 覆盖 scope 分级：读归 read:containers、
// 写归 write:containers，会话全权限，坏 Token 不得回落会话。
func TestHubContainerAuthz(t *testing.T) {
	h, sid, _ := hubWithAgent(t, true)
	suf := strconv.FormatInt(sid, 10)
	base := "/api/v1/servers/" + suf + "/containers"
	act := base + "/abc123/restart"
	del := base + "/abc123"
	cookie := h.session(t)
	read := h.token(t, "ro", apitoken.ScopeReadContainers)
	write := h.token(t, "rw", apitoken.ScopeWriteContainers)

	cases := []struct {
		why    string
		method string
		path   string
		bearer string
		cookie string
		want   int
	}{
		{"匿名 401", http.MethodGet, base, "", "", http.StatusUnauthorized},
		{"read:containers 可读清单", http.MethodGet, base, read, "", http.StatusOK},
		{"read:containers 不可操作", http.MethodPost, act, read, "", http.StatusForbidden},
		{"read:containers 不可删除", http.MethodDelete, del, read, "", http.StatusForbidden},
		{"write:containers 不可读清单", http.MethodGet, base, write, "", http.StatusForbidden},
		{"write:containers 可操作", http.MethodPost, act, write, "", http.StatusOK},
		{"会话全权限（读）", http.MethodGet, base, "", cookie, http.StatusOK},
		{"会话全权限（写）", http.MethodPost, act, "", cookie, http.StatusOK},
		{"坏 Token 不得回落会话", http.MethodGet, base, "ddk_bogus", cookie, http.StatusUnauthorized},
	}
	for _, c := range cases {
		resp, body := h.do(t, c.method, c.path, "", c.bearer, c.cookie)
		if resp.StatusCode != c.want {
			t.Errorf("%s：%s %s 得 %d，期望 %d｜%s", c.why, c.method, c.path, resp.StatusCode, c.want, trim(body))
		}
	}
}

// TestHubContainerServerStates 覆盖服务器三态：禁用 403（实时转发一律拒绝，
// 历史指标仍可查是 ServerExists 的另一回事）、不存在 404、ID 非数字 400。
func TestHubContainerServerStates(t *testing.T) {
	h, sid, _ := hubWithAgent(t, false)
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	resp, body := h.get(t, "/api/v1/servers/"+strconv.FormatInt(sid, 10)+"/containers", tok, "")
	if resp.StatusCode != http.StatusForbidden || errorCode(t, body) != protocol.CodeForbidden {
		t.Errorf("禁用服务器应 403 forbidden，得 %d｜%s", resp.StatusCode, trim(body))
	}

	h2 := newHub(t)
	tok2 := h2.token(t, "ro", apitoken.ScopeReadContainers)
	resp, body = h2.get(t, "/api/v1/servers/999/containers", tok2, "")
	if resp.StatusCode != http.StatusNotFound || errorCode(t, body) != protocol.CodeNotFound {
		t.Errorf("不存在的服务器应 404，得 %d｜%s", resp.StatusCode, trim(body))
	}
	resp, body = h2.get(t, "/api/v1/servers/abc/containers", tok2, "")
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
		t.Errorf("非数字 ID 应 400，得 %d｜%s", resp.StatusCode, trim(body))
	}
}

// ---- 审计（S3.4 第 6 步：读+写全量，server_id 自动带上）----

// auditRow 是审计断言行（只取关心的列）。
type auditRow struct {
	Action    string
	Result    string
	ServerID  int64
	Username  string
	Target    string
	RequestID string
}

// auditRows 读出全部审计行（新的在前）。
func auditRows(t *testing.T, h *hub) []auditRow {
	t.Helper()
	rows, err := h.st.DB.Query(
		`SELECT action, result, COALESCE(server_id,0), username, target, request_id
		   FROM audit_logs ORDER BY id DESC`)
	if err != nil {
		t.Fatalf("读审计表: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Action, &r.Result, &r.ServerID, &r.Username, &r.Target, &r.RequestID); err != nil {
			t.Fatalf("扫描审计行: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestHubContainerWritesAudit 验证全量审计：读与写都落库、动作按语义
// 分开（restart 不是笼统的 action）、server_id 从 {id} 自动带上、
// 身份取自觉银主体（Token 创建者）、失败也留痕、request_id 与响应头同源。
func TestHubContainerWritesAudit(t *testing.T) {
	h, sid, _ := hubWithAgent(t, true)
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers, apitoken.ScopeReadContainers)
	spath := "/api/v1/servers/" + strconv.FormatInt(sid, 10)
	base := spath + "/containers/abc123"

	if resp, body := h.do(t, http.MethodPost, base+"/restart", "", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("restart 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if resp, _ := h.get(t, spath+"/containers", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatal("清单应 200")
	}
	// 失败也落库：未知动作 404（探针行为同样值得留痕）。
	h.do(t, http.MethodPost, base+"/launch", "", tok, "")

	rows := auditRows(t, h)
	if len(rows) != 3 {
		t.Fatalf("应落 3 条审计（写/读/失败），得 %d：%+v", len(rows), rows)
	}
	// 新的在前：launch(error) → list(ok) → restart(ok)。
	want := []auditRow{
		{Action: "container.launch", Result: "error", ServerID: sid, Username: "admin", Target: base + "/launch"},
		{Action: "container.list", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/containers"},
		{Action: "container.restart", Result: "ok", ServerID: sid, Username: "admin", Target: base + "/restart"},
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

// TestHubContainerLogsThroughAudit 是审计包装不破坏流式转发的冒烟测试：
// follow 心跳经审计包装后全部到达，且日志也落审计（流结束才落库）。
// 注意它证明不了「Flush 真的透传了」——httptest recorder 不缓冲，
// 缺透传也照单全收；真正的回归测试在 middleware 包
// （TestStatusRecorderForwardsFlush，用 Flush 前不可见的假 writer）。
func TestHubContainerLogsThroughAudit(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetLogs("line-1\n")
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/containers/abc123/logs?follow=1"

	resp, body := h.get(t, path, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	for i := 1; i <= 5; i++ {
		want := "--follow-tick-" + strconv.Itoa(i) + "--"
		if !strings.Contains(string(body), want) {
			t.Errorf("第 %d 帧心跳应经审计包装后到达，体=%q", i, body)
		}
	}
	rows := auditRows(t, h)
	if len(rows) != 1 || rows[0].Action != "container.logs" || rows[0].Result != "ok" {
		t.Errorf("日志也应落审计（流结束才落库）：%+v", rows)
	}
}

// doHeader 与 do 相同，额外带自定义请求头（幂等键 e2e 用）。
func (h *hub) doHeader(t *testing.T, method, path, body, bearer string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	resp := w.Result()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// TestHubContainerRequestIDKey 覆盖幂等键 X-DD-Request-Id 的端到端契约
// （ARCHITECTURE.md「写操作全部幂等键落审计」）：
//   - 客户端带的键：响应头回显、原样转发给 Agent、落进审计行——三处同一份值；
//   - 不带的键：Hub 现场生成，三处仍是同一份值（对账不断链）；
//   - 非法键：边缘 400，请求不出门（Audit 之前的拒绝，不留审计行）。
func TestHubContainerRequestIDKey(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	spath := "/api/v1/servers/" + strconv.FormatInt(sid, 10)
	base := spath + "/containers/abc123"

	// 带键而来：三处同一份值。
	resp, body := h.doHeader(t, http.MethodPost, base+"/stop", "", tok,
		map[string]string{protocol.HeaderRequestID: "e2e-key-42"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if got := resp.Header.Get(protocol.HeaderRequestID); got != "e2e-key-42" {
		t.Errorf("响应头应回显客户端幂等键，得 %q", got)
	}
	if ids := fa.RequestIDs(); len(ids) == 0 || ids[len(ids)-1] != "e2e-key-42" {
		t.Errorf("Agent 应收到原样转发的幂等键，得 %v", ids)
	}
	if rows := auditRows(t, h); len(rows) != 1 || rows[0].RequestID != "e2e-key-42" {
		t.Errorf("审计行应记录客户端幂等键，得 %+v", rows)
	}

	// 不带键：Hub 生成，响应头/Agent/审计三处一致。
	resp, body = h.doHeader(t, http.MethodPost, base+"/restart", "", tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	gen := resp.Header.Get(protocol.HeaderRequestID)
	if !protocol.ValidRequestID(gen) {
		t.Errorf("缺省生成的键应合法，得 %q", gen)
	}
	if ids := fa.RequestIDs(); ids[len(ids)-1] != gen {
		t.Errorf("Agent 应收到 Hub 生成的键，得 %q｜want %q", ids[len(ids)-1], gen)
	}
	if rows := auditRows(t, h); len(rows) != 2 || rows[0].RequestID != gen {
		t.Errorf("审计行应记录生成的键，得 %+v", rows)
	}

	// 非法键：边缘 400 且不出门（该拒的请求不该打到 Agent）。
	before := len(fa.Calls())
	resp, body = h.doHeader(t, http.MethodPost, base+"/stop", "", tok,
		map[string]string{protocol.HeaderRequestID: "bad key!"})
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
		t.Errorf("非法键应 400，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if len(fa.Calls()) != before {
		t.Error("非法键不应打到 Agent")
	}
	if rows := auditRows(t, h); len(rows) != 2 {
		t.Errorf("边缘拒绝不留审计行，应仍 2 条，得 %d", len(rows))
	}
}
