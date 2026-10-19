// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.7：Compose 转发层（Hub 侧）的端到端测试——与镜像转发同规格：真库、真会话、
// 真 Token、真 TLS 假 Agent（验签中间件不 mock）。断言落在三处：前端看到的响应、
// Agent 实际收到的请求（路径/请求体）、以及「该拒的请求根本没出门」。
//
// scope 沿用容器 scope：list/logs/file 读归 read:containers，写动作与 YAML
// 写（含 diff 预览）归 write:containers（ARCHITECTURE.md 不为 Compose 新增 scope）。

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

func TestHubComposeList(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetComposeList([]protocol.ComposeProject{
		{Name: "watchtower", Path: "/opt/watchtower", ConfigFile: "/opt/watchtower/docker-compose.yml", Status: "running"},
	})
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/compose"

	resp, body := h.get(t, path, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data []protocol.ComposeProject `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if len(env.Data) != 1 || env.Data[0].Name != "watchtower" || env.Data[0].Status != "running" {
		t.Errorf("项目应原样到达前端：%+v", env.Data)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/compose" {
		t.Errorf("应打到 Agent 的 /api/v1/compose，得 %v", calls)
	}
}

func TestHubComposeAction(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetComposeAction(protocol.ComposeActionResult{Output: "Container watchtower  Started\n"})
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	base := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/compose/watchtower"

	resp, body := h.do(t, http.MethodPost, base+"/up", "", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("up 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data protocol.ComposeActionResult `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if env.Data.Output != "Container watchtower  Started\n" {
		t.Errorf("compose 原生输出应原样透传：%q", env.Data.Output)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/compose/watchtower/up" {
		t.Errorf("转发路径不符，得 %v", calls)
	}

	// 非法动作：agentcli 在出门前拦下——400 且 Agent 收不到任何请求。
	resp, body = h.do(t, http.MethodPost, base+"/rm", "", tok, "")
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
		t.Errorf("非法动作应 400，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if calls := fa.Calls(); len(calls) != 1 {
		t.Errorf("非法动作不应出门，但 Agent 收到 %v", calls)
	}
}

func TestHubComposeLogs(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetComposeLogs("watchtower-1  | hello\nwatchtower-1  | world\n")
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	base := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/compose/watchtower/logs"

	resp, body := h.get(t, base, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if string(body) != "watchtower-1  | hello\nwatchtower-1  | world\n" {
		t.Errorf("裸流应原样透传：%q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("内容类型应为 text/plain，得 %q", ct)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/compose/watchtower/logs" {
		t.Errorf("转发路径不符，得 %v", calls)
	}

	// follow=1 进查询串（签名路径的一部分）。
	resp2, _ := h.get(t, base+"?follow=1", tok, "")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("follow 应 200，得 %d", resp2.StatusCode)
	}
	if calls := fa.Calls(); len(calls) != 2 || calls[1] != "/api/v1/compose/watchtower/logs?follow=1" {
		t.Errorf("follow 应进查询串，得 %v", calls)
	}
}

func TestHubComposeFileGet(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetComposeFile(protocol.ComposeFile{
		ConfigFile: "/opt/watchtower/docker-compose.yml",
		Content:    "services:\n  watchtower:\n",
	})
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/compose/watchtower/file"

	resp, body := h.get(t, path, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data protocol.ComposeFile `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if env.Data.ConfigFile != "/opt/watchtower/docker-compose.yml" || env.Data.Content != "services:\n  watchtower:\n" {
		t.Errorf("文件内容应原样到达前端：%+v", env.Data)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/compose/watchtower/file" {
		t.Errorf("转发路径不符，得 %v", calls)
	}
}

func TestHubComposeFileDiff(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetComposeFileDiff(protocol.ComposeFileDiff{Diff: "@@ -1,1 +1,1 @@\n-a\n+b\n"})
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/compose/watchtower/diff"

	resp, body := h.do(t, http.MethodPost, path, `{"content":"`+`services:\n  watchtower:\n    image: containrrr/watchtower:latest\n`+`"}`, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data protocol.ComposeFileDiff `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if env.Data.Diff != "@@ -1,1 +1,1 @@\n-a\n+b\n" {
		t.Errorf("diff 应原样透传：%q", env.Data.Diff)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/compose/watchtower/diff" {
		t.Errorf("转发路径不符，得 %v", calls)
	}
	// 新内容真的到了 Agent（不是空壳转发）：断言具体值，而非仅有 content 键。
	if bodies := fa.WriteBodies(); len(bodies) != 1 || !strings.Contains(bodies[0], "containrrr/watchtower:latest") {
		t.Errorf("新内容应原样透传到 Agent，得 %v", bodies)
	}

	// 坏请求体 → 400 且不出门（decodeOptionalBody 的 DisallowUnknownFields）。
	resp, body = h.do(t, http.MethodPost, path, `{"bogus":1}`, tok, "")
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
		t.Errorf("坏请求体应 400，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if calls := fa.Calls(); len(calls) != 1 {
		t.Errorf("坏请求体不应出门，但 Agent 收到 %v", calls)
	}
}

func TestHubComposeFilePut(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetComposeFileWritten(protocol.ComposeFileWritten{
		BackupFile: "/opt/watchtower/docker-compose.yml.bak.2026-10-15T07:00:00+08:00",
	})
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/compose/watchtower/file"

	resp, body := h.do(t, http.MethodPut, path, `{"content":"`+`services:\n  watchtower:\n    image: containrrr/watchtower:latest\n`+`"}`, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data protocol.ComposeFileWritten `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if env.Data.BackupFile != "/opt/watchtower/docker-compose.yml.bak.2026-10-15T07:00:00+08:00" {
		t.Errorf("备份路径应回传前端供回滚：%q", env.Data.BackupFile)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/compose/watchtower/file" {
		t.Errorf("转发路径不符，得 %v", calls)
	}
	// 新内容真的到了 Agent（不是空壳转发）：断言具体值，而非仅有 content 键。
	if bodies := fa.WriteBodies(); len(bodies) != 1 || !strings.Contains(bodies[0], "containrrr/watchtower:latest") {
		t.Errorf("新内容应原样透传到 Agent，得 %v", bodies)
	}
}

// TestHubComposeFilePut_LargeYAML 回归 v0.2.2 #4：compose 请求体上限独立于
// 选项体的 4KiB——>5KiB 的 YAML 必须能写进（真机 7.2KiB compose 曾报
// 「请求体不合法: unexpected EOF」）。
func TestHubComposeFilePut_LargeYAML(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetComposeFileWritten(protocol.ComposeFileWritten{BackupFile: "/tmp/x.bak"})
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/compose/watchtower/file"

	// 构造 >5KiB 的 content（JSON 里逐字符安全）：若干行注释垫底。
	line := "services:\\n  watchtower:\\n    image: containrrr/watchtower:latest\\n"
	content := strings.Repeat("# pad line to exceed 4KiB body limit for compose yaml\\n", 130)
	content += line
	payload := `{"content":"` + content + `"}`
	if len(payload) < 5<<10 {
		t.Fatalf("用例自身没超过 4KiB，失去回归意义: %d", len(payload))
	}
	resp, body := h.do(t, http.MethodPut, path, payload, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf(">5KiB YAML 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if bodies := fa.WriteBodies(); len(bodies) != 1 || !strings.Contains(bodies[0], "watchtower") {
		t.Errorf("大 YAML 应原样透传到 Agent")
	}
}
