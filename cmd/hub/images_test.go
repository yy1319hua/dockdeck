// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.1 镜像转发层的端到端测试（与 S3.4 容器侧同规格）：真库、真会话、
// 真 Token、真 TLS 假 Agent（验签中间件不 mock）——Hub→agentcli→Agent
// 整条链路连同签名头一起跑。断言落在三处：前端看到的响应、Agent 实际
// 收到的请求（路径/请求体）、以及「该拒的请求根本没出门」（本地校验
// 先行的证据）。scope 复用容器 scope：清单归 read:containers，三个写
// 操作归 write:containers（ARCHITECTURE.md 不为镜像新增 scope）。

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

// TestHubImageListForwardsToAgent 覆盖 GET images：Engine 投影原样到达
// 前端；all 显式进查询串（签名路径的一部分）；dangling=true 是 prune
// dry-run 两段式的第一段，Hub 侧只认 true/false（或 1/0），拼错响亮
// 失败且不出门。
func TestHubImageListForwardsToAgent(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetImageList([]protocol.ImageSummary{
		{ID: "sha256:abc123", RepoTags: []string{"alpine:3.20"}, Size: 7770000, Architecture: "amd64", Os: "linux"},
		{ID: "sha256:def456", RepoTags: []string{}, Size: 12000000}, // 悬空：无 tag
	})
	tok := h.token(t, "ro", apitoken.ScopeReadContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/images"

	resp, body := h.get(t, path, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data []protocol.ImageSummary `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if len(env.Data) != 2 || env.Data[0].ID != "sha256:abc123" || env.Data[1].ID != "sha256:def456" {
		t.Errorf("Engine 投影应原样到达前端：%+v", env.Data)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/images?all=false" {
		t.Errorf("应带 all=false 打到 Agent，得 %v", calls)
	}

	// dangling=true：dry-run 两段式的第一段数据源，到 Agent 侧归一为 "1"。
	if resp, _ := h.get(t, path+"?all=true&dangling=true", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("dangling=true 应 200，得 %d", resp.StatusCode)
	}
	if calls := fa.Calls(); len(calls) != 2 || calls[1] != "/api/v1/images?all=true&dangling=1" {
		t.Errorf("dangling 应以 1 打到 Agent，得 %v", calls)
	}

	// 拼错要响亮失败：dangling/all 的坏值都 400 且不出门。
	for _, q := range []string{"?dangling=yes", "?dangling=2", "?all=yes"} {
		resp, body := h.get(t, path+q, tok, "")
		if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
			t.Errorf("%s 应 400 bad_request，得 %d｜%s", q, resp.StatusCode, trim(body))
		}
	}
	if calls := fa.Calls(); len(calls) != 2 {
		t.Errorf("坏参数不应打到 Agent，得 %v", calls)
	}
}

// TestHubImagePullForwards 覆盖 POST images/pull：body {"ref":...} 原样
// 抵达 Agent；汇总式响应（最终状态/层清单）透传给前端；空 ref 与坏 ref
// 都在本地 400 不出门；Engine 流内业务失败（Agent 已映射 424（v0.2.2 #3，502 会被 CF 吃掉））原样
// 透传；read scope 不可拉取。
func TestHubImagePullForwards(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetPullResult(protocol.PullResult{
		Status: "Status: Downloaded newer image for alpine:3.20",
		Layers: []string{"sha256:deadbeef", "sha256:cafebabe"},
	})
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/images/pull"

	resp, body := h.do(t, http.MethodPost, path, `{"ref":"alpine:3.20"}`, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data protocol.PullResult `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if env.Data.Status != "Status: Downloaded newer image for alpine:3.20" || len(env.Data.Layers) != 2 {
		t.Errorf("汇总结果应透传：%+v", env.Data)
	}
	if bodies := fa.WriteBodies(); len(bodies) != 1 || bodies[0] != `{"ref":"alpine:3.20"}` {
		t.Errorf("ref 应经请求体抵达 Agent，得 %v", bodies)
	}

	// 空 ref 与坏 ref：本地 400，不出门。空 ref（显式空串与完全空体两种
	// 形态）单独钉文案——Hub 显式拒绝（「ref 不能为空」）先于 agentcli
	// 的字符集拒绝发生，两层防御各有其名，撤掉任一层另一层仍兜底，
	// 但文案能证明是哪层拒的。
	for _, b := range []string{`{"ref":""}`, ``} {
		if resp, body := h.do(t, http.MethodPost, path, b, tok, ""); resp.StatusCode != http.StatusBadRequest ||
			errorCode(t, body) != protocol.CodeBadRequest || !strings.Contains(string(body), "ref 不能为空") {
			t.Errorf("空 ref（body %q）应 400 且文案指明原因，得 %d｜%s", b, resp.StatusCode, trim(body))
		}
	}
	for _, b := range []string{`{"ref":"a b"}`, `{"ref":".."}`} {
		resp, body := h.do(t, http.MethodPost, path, b, tok, "")
		if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != protocol.CodeBadRequest {
			t.Errorf("body %q 应 400 bad_request，得 %d｜%s", b, resp.StatusCode, trim(body))
		}
	}
	if calls := fa.Calls(); len(calls) != 1 {
		t.Errorf("坏 ref 不应打到 Agent，得 %v", calls)
	}

	// 流内业务失败：Engine 把错误放在进度流里（HTTP 200），Agent 映射
	// 424 带原话，Hub 原样透传——调用方要能区分「仓库不存在」与「网络不通」。
	fa.SetPullResult(protocol.PullResult{Err: "pull access denied for nope/nope, repository does not exist"})
	resp, body = h.do(t, http.MethodPost, path, `{"ref":"nope/nope:latest"}`, tok, "")
	if resp.StatusCode != http.StatusFailedDependency || errorCode(t, body) != protocol.CodeUpstreamError {
		t.Fatalf("流内失败应透传 424 upstream_error，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if !strings.Contains(string(body), "repository does not exist") {
		t.Errorf("Engine 原话应带到，得 %s", trim(body))
	}

	// read:containers 不可拉取（写归 write:containers）。
	ro := h.token(t, "ro2", apitoken.ScopeReadContainers)
	if resp, _ := h.do(t, http.MethodPost, path, `{"ref":"alpine:3.20"}`, ro, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read scope 不可 pull，得 %d", resp.StatusCode)
	}
}

// TestHubImageDeleteForwards 覆盖 DELETE images/{ref}：ref 直拼路径，
// 含 "/" 的多段引用（library/nginx:latest）必须完整抵达 Agent（尾通配符
// 路由靠它）；read scope 不可删除。
func TestHubImageDeleteForwards(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	base := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/images/"

	resp, body := h.do(t, http.MethodDelete, base+"alpine:3.20", "", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/images/alpine:3.20" {
		t.Errorf("转发路径不符，得 %v", calls)
	}

	// 多段引用：路径一字不差抵达（Agent 侧 {ref...} 尾通配符匹配）。
	if resp, _ := h.do(t, http.MethodDelete, base+"library/nginx:latest", "", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("多段 ref 应 200，得 %d", resp.StatusCode)
	}
	if calls := fa.Calls(); len(calls) != 2 || calls[1] != "/api/v1/images/library/nginx:latest" {
		t.Errorf("多段 ref 转发路径不符，得 %v", calls)
	}

	// read:containers 不可删除。
	ro := h.token(t, "ro2", apitoken.ScopeReadContainers)
	if resp, _ := h.do(t, http.MethodDelete, base+"alpine:3.20", "", ro, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read scope 不可删除，得 %d", resp.StatusCode)
	}
}

// TestHubImagePruneForwards 覆盖 POST images/prune：dry-run 两段式的
// 第二段（真删），Engine 的清理清单（Deleted/Untagged 二形态）与回收
// 空间原样透传；read scope 不可清理。
func TestHubImagePruneForwards(t *testing.T) {
	h, sid, fa := hubWithAgent(t, true)
	fa.SetPruneResult(protocol.PruneResult{
		ImagesDeleted: []protocol.PrunedImage{
			{Deleted: "sha256:deadbeef"},
			{Untagged: "alpine:3.19"},
		},
		SpaceReclaimed: 98765432,
	})
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	path := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/images/prune"

	resp, body := h.do(t, http.MethodPost, path, "", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data protocol.PruneResult `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if env.Data.SpaceReclaimed != 98765432 {
		t.Errorf("回收空间应透传，got %d", env.Data.SpaceReclaimed)
	}
	if len(env.Data.ImagesDeleted) != 2 || env.Data.ImagesDeleted[0].Deleted != "sha256:deadbeef" ||
		env.Data.ImagesDeleted[1].Untagged != "alpine:3.19" {
		t.Errorf("清理清单二形态应分别保留：%+v", env.Data.ImagesDeleted)
	}
	if calls := fa.Calls(); len(calls) != 1 || calls[0] != "/api/v1/images/prune" {
		t.Errorf("转发路径不符，得 %v", calls)
	}

	// read:containers 不可清理。
	ro := h.token(t, "ro2", apitoken.ScopeReadContainers)
	if resp, _ := h.do(t, http.MethodPost, path, "", ro, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read scope 不可 prune，得 %d", resp.StatusCode)
	}
}

// TestHubImageWritesAudit 验证镜像四端点全量落审计：动作按语义分开
// （image.list/pull/delete/prune，不是笼统的 image）、server_id 从 {id}
// 自动带上、身份取自觉银主体（Token 创建者）、request_id 非空。
func TestHubImageWritesAudit(t *testing.T) {
	h, sid, _ := hubWithAgent(t, true)
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers, apitoken.ScopeReadContainers)
	spath := "/api/v1/servers/" + strconv.FormatInt(sid, 10)

	if resp, body := h.get(t, spath+"/images", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("list 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if resp, body := h.do(t, http.MethodPost, spath+"/images/pull", `{"ref":"alpine:3.20"}`, tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("pull 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if resp, body := h.do(t, http.MethodDelete, spath+"/images/alpine:3.20", "", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	if resp, body := h.do(t, http.MethodPost, spath+"/images/prune", "", tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("prune 应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}

	rows := auditRows(t, h)
	if len(rows) != 4 {
		t.Fatalf("应落 4 条审计，得 %d：%+v", len(rows), rows)
	}
	// 新的在前：prune → delete → pull → list。
	want := []auditRow{
		{Action: "image.prune", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/images/prune"},
		{Action: "image.delete", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/images/alpine:3.20"},
		{Action: "image.pull", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/images/pull"},
		{Action: "image.list", Result: "ok", ServerID: sid, Username: "admin", Target: spath + "/images"},
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
