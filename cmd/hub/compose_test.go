// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.7：Compose 转发层（Hub 侧）的端到端测试——与镜像转发同规格：真库、真会话、
// 真 Token、真 TLS 假 Agent（验签中间件不 mock）。断言落在三处：前端看到的响应、
// Agent 实际收到的请求（路径/请求体）、以及「该拒的请求根本没出门」。
//
// scope 沿用容器 scope：list/logs 归 read:containers，写动作归 write:containers
// （ARCHITECTURE.md 不为 Compose 新增 scope）。

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
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
