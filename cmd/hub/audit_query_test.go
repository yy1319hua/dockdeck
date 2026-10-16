// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S7.7 审计查询端点（方案 Q1–Q4 全 A）的分页/过滤/参数/鉴权/不落痕五类验证。
//
// 与 audit_test.go 的分工：那个锁「写」的覆盖面（哪些端点必须落审计行），
// 本文件锁「读」的链路（查询端点自身的行为）。seedAudit 走原生 INSERT——
// 测试要精确控制 ts/id 才能断言窗口与游标，WriteAudit 不暴露这两列。

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
)

// seedAudit 直接插一行审计行，返回自增 id。serverID=0 写 NULL（模拟
// 「与具体服务器无关的动作」，如 token/auth 类），ts 由调用方给定。
func seedAudit(t *testing.T, h *hub, action, ts string, serverID int64, result string) int64 {
	t.Helper()
	var sid any
	if serverID != 0 {
		sid = serverID
	}
	res, err := h.st.DB.Exec(
		`INSERT INTO audit_logs(user_id,username,server_id,action,target,detail,result,error,ip,request_id,ts)
		 VALUES(1,'admin',?,?,?,'',?,?,?,?,?)`,
		sid, action, "/api/v1/servers/"+itoa(serverID)+"/x", result, "", "10.0.0.1", "rid-"+action, ts)
	if err != nil {
		t.Fatalf("插审计行: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// auditPageResp 解查询响应（信封 data）。
type auditPageResp struct {
	Data struct {
		Logs []struct {
			ID       int64  `json:"id"`
			Ts       string `json:"ts"`
			Username string `json:"username"`
			ServerID int64  `json:"server_id"`
			Action   string `json:"action"`
			Result   string `json:"result"`
			Target   string `json:"target"`
		} `json:"logs"`
		NextBefore int64 `json:"next_before"`
	} `json:"data"`
}

func getAuditPage(t *testing.T, h *hub, query, bearer, cookie string) (*http.Response, auditPageResp) {
	t.Helper()
	resp, body := h.get(t, "/api/v1/audit_logs"+query, bearer, cookie)
	var page auditPageResp
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("解析审计响应: %v｜%s", err, body)
		}
	}
	return resp, page
}

// TestAuditLogsPaging 锁 Q1.A：倒序 + before_id 游标分页。
func TestAuditLogsPaging(t *testing.T) {
	h := newHub(t)
	// 先登录再 seed：session 自审计的 auth.login 拿到最小 id，排在 seed
	// 行之前，倒序首页（取最新两条）不会被它挤占。
	cookie := h.session(t)
	base := time.Now().UTC().Add(-3 * time.Hour)
	id1 := seedAudit(t, h, "a.one", base.Format(auditTSLayout), 0, "ok")
	id2 := seedAudit(t, h, "a.two", base.Add(time.Hour).Format(auditTSLayout), 0, "ok")
	id3 := seedAudit(t, h, "a.three", base.Add(2*time.Hour).Format(auditTSLayout), 0, "ok")
	_ = id1
	_ = id3

	// 第一页 limit=2：最新在前 → three, two；游标=two 的 id（本页最后一条）。
	resp, page := getAuditPage(t, h, "?limit=2", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d", resp.StatusCode)
	}
	if len(page.Data.Logs) != 2 {
		t.Fatalf("第一页应 2 条，得 %d", len(page.Data.Logs))
	}
	if page.Data.Logs[0].Action != "a.three" || page.Data.Logs[1].Action != "a.two" {
		t.Fatalf("倒序错：%v", []string{page.Data.Logs[0].Action, page.Data.Logs[1].Action})
	}
	if page.Data.NextBefore != id2 {
		t.Fatalf("游标应指向 two 的 id（%d），得 %d", id2, page.Data.NextBefore)
	}

	// 第二页带游标 limit=2：剩余两条一次取尽——a.one，以及先登录时
	// 自审计的 auth.login（server 无关动作，server_id=0）；next_before=0。
	_, page2 := getAuditPage(t, h, "?limit=2&before_id="+itoa(page.Data.NextBefore), "", cookie)
	if len(page2.Data.Logs) != 2 {
		t.Fatalf("第二页应 2 条，得 %d", len(page2.Data.Logs))
	}
	if page2.Data.Logs[0].Action != "a.one" || page2.Data.Logs[1].Action != "auth.login" {
		t.Fatalf("第二页倒序错：%v", []string{page2.Data.Logs[0].Action, page2.Data.Logs[1].Action})
	}
	if page2.Data.NextBefore != 0 {
		t.Fatalf("没有更多时 next_before 应为 0，得 %d", page2.Data.NextBefore)
	}
}

// TestAuditLogsFilters 锁 Q2.A：server_id / action 前缀 / result / from-to 窗口。
func TestAuditLogsFilters(t *testing.T) {
	h := newHub(t)
	now := time.Now().UTC()
	srv := h.seedServer(t)
	seedAudit(t, h, "container.list", now.Add(-4*time.Hour).Format(auditTSLayout), srv, "ok")
	seedAudit(t, h, "container.remove", now.Add(-3*time.Hour).Format(auditTSLayout), srv, "ok")
	seedAudit(t, h, "image.pull", now.Add(-2*time.Hour).Format(auditTSLayout), srv+1, "error")
	seedAudit(t, h, "auth.login", now.Add(-1*time.Hour).Format(auditTSLayout), 0, "ok")
	cookie := h.session(t)

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"server_id 过滤", "?server_id=" + itoa(srv), 2},
		{"action 前缀", "?action=container", 2},
		{"result 过滤", "?result=error", 1},
		{"窗口排除最早", "?from=" + now.Add(-90*time.Minute).UTC().Format(auditTSLayout), 2},
		{"组合", "?server_id=" + itoa(srv) + "&action=container&result=ok", 2},
		{"前缀不误伤", "?action=container.l", 1},
	}
	for _, c := range cases {
		resp, page := getAuditPage(t, h, c.query, "", cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s：应 200，得 %d", c.name, resp.StatusCode)
		}
		if len(page.Data.Logs) != c.want {
			t.Errorf("%s：应 %d 条，得 %d", c.name, c.want, len(page.Data.Logs))
		}
	}
}

// TestAuditLogsBadParams 锁参数校验：非法一律 400 bad_request（同 metrics 范式）。
func TestAuditLogsBadParams(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)
	bad := []string{
		"?limit=0", "?limit=abc", "?from=notatime", "?to=notatime",
		"?result=maybe", "?action=container%20list", "?action=cont%25ainer",
		"?server_id=0", "?server_id=-1", "?before_id=-1",
	}
	for _, q := range bad {
		resp, body := h.get(t, "/api/v1/audit_logs"+q, "", cookie)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400，得 %d｜%s", q, resp.StatusCode, body)
			continue
		}
		if code := errorCode(t, body); code != "bad_request" {
			t.Errorf("%s 应 bad_request，得 %s", q, code)
		}
	}
	// to 早于 from 也 400。
	resp, _ := h.get(t, "/api/v1/audit_logs?from=2026-10-16T00:00:00Z&to=2026-10-15T00:00:00Z", "", cookie)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("to<from 应 400，得 %d", resp.StatusCode)
	}
}

// TestAuditLogsAuth 锁鉴权：会话用户直行（ServeSession 不查 scope）；
// API Token 必须勾 read:audit，否则 403 scope_denied。
func TestAuditLogsAuth(t *testing.T) {
	h := newHub(t)

	if resp, _ := h.get(t, "/api/v1/audit_logs", "", h.session(t)); resp.StatusCode != http.StatusOK {
		t.Errorf("会话用户应 200，得 %d", resp.StatusCode)
	}
	noScope := h.token(t, "ro", apitoken.ScopeReadMetrics)
	resp, body := h.get(t, "/api/v1/audit_logs", noScope, "")
	if resp.StatusCode != http.StatusForbidden || errorCode(t, body) != "scope_denied" {
		t.Errorf("无 read:audit 的 Token 应 403 scope_denied，得 %d｜%s", resp.StatusCode, body)
	}
	withScope := h.token(t, "aud", apitoken.ScopeReadAudit)
	if resp, _ := h.get(t, "/api/v1/audit_logs", withScope, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("带 read:audit 的 Token 应 200，得 %d", resp.StatusCode)
	}
	// 未认证：既无会话也无 Token → 401。
	if resp, _ := h.get(t, "/api/v1/audit_logs", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未认证应 401，得 %d", resp.StatusCode)
	}
}

// TestAuditQueryWritesNoAudit 锁 Q4：查询端点自身不写审计行——
// 高频读留痕会淹掉真操作记录。查询前后 audit_logs 行数必须不变。
func TestAuditQueryWritesNoAudit(t *testing.T) {
	h := newHub(t)
	seedAudit(t, h, "a.one", time.Now().UTC().Format(auditTSLayout), 0, "ok")
	seedAudit(t, h, "a.two", time.Now().UTC().Format(auditTSLayout), 0, "ok")
	cookie := h.session(t)

	// session() 走真实登录，auth.login 自审计 1 行；seed 2 行 → 共 3 行。
	before := len(auditRows(t, h))
	if before != 3 {
		t.Fatalf("前置行数预期 3，得 %d", before)
	}
	for _, q := range []string{"", "?limit=1", "?action=a&result=ok", "?from=2020-01-01T00:00:00Z"} {
		if resp, _ := h.get(t, "/api/v1/audit_logs"+q, "", cookie); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 应 200，得 %d", q, resp.StatusCode)
		}
	}
	if after := len(auditRows(t, h)); after != before {
		t.Fatalf("查询不应新增审计行：前 %d 行，后 %d 行", before, after)
	}
}
