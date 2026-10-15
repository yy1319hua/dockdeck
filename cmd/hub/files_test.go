// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S5.5(4/5)：文件管理 Hub 转发层的端到端测试。
// 用 fakeagent（真验签）暴露 /api/v1/files* 五端点，整条 Hub→agentcli→Agent
// 链路连同签名头一起跑；断言落在三处：前端响应、Agent 实际收到的请求
// （路径/请求体透传）、以及审计落库（file.list/stat/content/write/action 五类）。
//
// 与 fakeagent 未实现文件端点前的容器/镜像/Compose 测试同款范式：
// hubWithAgent 装配「Hub + 真验签假 Agent」，Calls()/WriteBodies() 看转发，
// auditRows() 看审计。

package main

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// TestHubFileWritesAudit 验证文件五端点全量落审计：动作按语义分开
// （file.list/stat/content/write/action，不是笼统的 file）、server_id 从 {id}
// 自动带上、身份取自觉银主体（Token 创建者 admin）、request_id 非空。
func TestHubFileWritesAudit(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetFileList(protocol.FilesList{
		Path:    "app",
		Entries: []protocol.FileEntry{{Name: "config.yaml", Path: "app/config.yaml", IsDir: false, Size: 123, Mode: "0644"}},
	})
	fa.SetFileMeta(protocol.FileMeta{Path: "app/config.yaml", Size: 123, ContentType: "text/yaml"})
	fa.SetFileContent("hello: world\n", "text/yaml")
	fa.SetFileWritten(protocol.FileWritten{Path: "app/config.yaml", Size: 123, Created: true})
	fa.SetFileActionDone(protocol.FileActionDone{Action: "mkdir", Path: "app/data"})

	tok := h.token(t, "rw", apitoken.ScopeReadFiles, apitoken.ScopeWriteFiles)
	spath := "/api/v1/servers/" + strconv.FormatInt(sid, 10)

	if resp, body := h.get(t, spath+"/files?root=0&path=app&depth=1", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("list 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if resp, body := h.get(t, spath+"/files/stat?root=0&path=app/config.yaml", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("stat 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if resp, body := h.get(t, spath+"/files/content?root=0&path=app/config.yaml", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("content 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if resp, body := h.do(t, http.MethodPut, spath+"/files/content?root=0&path=app/config.yaml", "new content here", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("write 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if resp, body := h.do(t, http.MethodPost, spath+"/files/mkdir?root=0", `{"action":"mkdir","path":"app/data"}`, tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("action 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}

	rows := auditRows(t, h)
	if len(rows) != 5 {
		t.Fatalf("应落 5 条审计，得 %d：%+v", len(rows), rows)
	}
	// 新的在前（ORDER BY id DESC）：action → write → content → stat → list。
	want := []auditRow{
		{Action: "file.action", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/files/mkdir"},
		{Action: "file.write", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/files/content"},
		{Action: "file.content", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/files/content"},
		{Action: "file.stat", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/files/stat"},
		{Action: "file.list", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/files"},
	}
	for i, w := range want {
		g := rows[i]
		if g.Action != w.Action || g.Result != w.Result || g.ServerID != w.ServerID ||
			g.Username != w.Username || g.Target != w.Target {
			t.Errorf("审计行 %d 不符：\ngot  %+v\nwant %+v", i, g, w)
		}
		if g.RequestID == "" {
			t.Errorf("审计行 %d 缺 request_id（与响应头同源，排障要对得上）", i)
		}
	}
}

// TestHubFileScopeSeparation 防止「read/write 两 scope 退化成一个」：
// read:files 只能读（list 200，写/动作 403）、write:files 只能写
// （写/动作 200，读 403）。scope 拦截必须发生在转发 Agent 之前。
func TestHubFileScopeSeparation(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetFileWritten(protocol.FileWritten{Path: "a", Created: true})
	fa.SetFileActionDone(protocol.FileActionDone{Action: "mkdir", Path: "a"})
	read := h.token(t, "ro", apitoken.ScopeReadFiles)
	write := h.token(t, "wo", apitoken.ScopeWriteFiles)
	spath := "/api/v1/servers/" + strconv.FormatInt(sid, 10)

	// read:files 可读不可写
	if resp, _ := h.get(t, spath+"/files", read, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("read:files 应可读 list，得 %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPut, spath+"/files/content?root=0&path=a", "x", read, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read:files 不可写 content，得 %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, spath+"/files/mkdir?root=0", `{"action":"mkdir","path":"a"}`, read, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read:files 不可 action，得 %d", resp.StatusCode)
	}

	// write:files 可写不可读
	if resp, _ := h.get(t, spath+"/files", write, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("write:files 不可读 list，得 %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPut, spath+"/files/content?root=0&path=a", "x", write, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("write:files 应可写 content，得 %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, spath+"/files/mkdir?root=0", `{"action":"mkdir","path":"a"}`, write, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("write:files 应可 action，得 %d", resp.StatusCode)
	}
}

// TestHubFileBadParams 验证 Hub 侧入参校验：root/depth 负数或非整数、
// 查询串非法，一律本地 400，不出门（scope 已过的请求也拦得住）。
func TestHubFileBadParams(t *testing.T) {
	h, sid, _ := hubWithAgent(t, true)
	tok := h.token(t, "ro", apitoken.ScopeReadFiles)
	spath := "/api/v1/servers/" + strconv.FormatInt(sid, 10)

	cases := []struct {
		why, query string
	}{
		{"root 负数", "?root=-1&path=app"},
		{"root 非整数", "?root=abc&path=app"},
		{"查询串非法", "?root=%ZZ"},
		{"depth 负数", "?root=0&path=app&depth=-2"},
	}
	for _, c := range cases {
		resp, body := h.get(t, spath+"/files"+c.query, tok, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400，得 %d｜%s", c.why, resp.StatusCode, trim(body))
		}
	}
}

// TestHubFileForwardsToAgent 断言 Hub 把五类请求按正确路径与请求体转发给 Agent：
// list/stat/content 读、content 写、{action} 写动作，且写入正文与动作体原样透传。
func TestHubFileForwardsToAgent(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetFileList(protocol.FilesList{Path: "app", Entries: []protocol.FileEntry{{Name: "c.yaml", Path: "app/c.yaml"}}})
	fa.SetFileMeta(protocol.FileMeta{Path: "app/c.yaml", Size: 3})
	fa.SetFileContent("xyz", "text/plain")
	fa.SetFileWritten(protocol.FileWritten{Path: "app/c.yaml", Size: 3, Created: true})
	fa.SetFileActionDone(protocol.FileActionDone{Action: "mkdir", Path: "app/d"})

	tok := h.token(t, "rw", apitoken.ScopeReadFiles, apitoken.ScopeWriteFiles)
	spath := "/api/v1/servers/" + strconv.FormatInt(sid, 10)

	h.get(t, spath+"/files?root=0&path=app&depth=1", tok, "")
	h.get(t, spath+"/files/stat?root=0&path=app/c.yaml", tok, "")
	h.get(t, spath+"/files/content?root=0&path=app/c.yaml", tok, "")
	h.do(t, http.MethodPut, spath+"/files/content?root=0&path=app/c.yaml", "xyz", tok, "")
	h.do(t, http.MethodPost, spath+"/files/mkdir?root=0", `{"action":"mkdir","path":"app/d"}`, tok, "")

	calls := fa.Calls()
	if len(calls) != 5 {
		t.Fatalf("Agent 应收到 5 次转发，得 %d：%v", len(calls), calls)
	}
	has := func(prefix string) bool {
		for _, c := range calls {
			if strings.HasPrefix(c, prefix) {
				return true
			}
		}
		return false
	}
	if !has("/api/v1/files?") {
		t.Errorf("Agent 未收到 list 转发：%v", calls)
	}
	if !has("/api/v1/files/stat?") {
		t.Errorf("Agent 未收到 stat 转发：%v", calls)
	}
	contentCount := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "/api/v1/files/content?") {
			contentCount++
		}
	}
	if contentCount != 2 {
		t.Errorf("content 应被转发 2 次（读+写），得 %d：%v", contentCount, calls)
	}
	if !has("/api/v1/files/mkdir") {
		t.Errorf("Agent 未收到 action 转发：%v", calls)
	}

	// 写透传：PUT 正文与 action 请求体应原样抵达 Agent。
	bodies := fa.WriteBodies()
	if len(bodies) != 2 {
		t.Fatalf("应记录 2 条写体，得 %d：%v", len(bodies), bodies)
	}
	if bodies[0] != "xyz" {
		t.Errorf("PUT 正文未透传，得 %q", bodies[0])
	}
	if !strings.Contains(bodies[1], `"action":"mkdir"`) || !strings.Contains(bodies[1], `"path":"app/d"`) {
		t.Errorf("action 请求体未透传：%q", bodies[1])
	}
}

// TestHubFileUploadTooLarge 验证上传超限由 Hub 在出门前本地拦下（413），
// 与 Agent 侧同源 MaxFileUploadBytes，避免「Hub 放行、Agent 413」的错位。
// 超限请求根本不应转发给 Agent。
func TestHubFileUploadTooLarge(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetFileWritten(protocol.FileWritten{Path: "app/big.bin", Size: 1})
	tok := h.token(t, "w", apitoken.ScopeWriteFiles)
	spath := "/api/v1/servers/" + strconv.FormatInt(sid, 10)

	big := bytes.Repeat([]byte{'a'}, int(protocol.MaxFileUploadBytes)+1)
	resp, body := h.do(t, http.MethodPut, spath+"/files/content?root=0&path=app/big.bin", string(big), tok, "")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限上传应 413，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if code := errorCode(t, body); code != "too_large" {
		t.Errorf("错误码应为 too_large，得 %q", code)
	}
	for _, c := range fa.Calls() {
		if strings.Contains(c, "/api/v1/files/content") {
			t.Errorf("超限请求竟转发到了 Agent：%s", c)
		}
	}
}
