// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// TestClient_ImageList 验证清单读链路：all 显式进查询串、dangling 以 "1"
// 传达（Agent 侧只认 "1"，缺省即 false）——查询串是签名路径的一部分，
// 两侧必须字字一致；信封内层解码为协议类型，空清单是合法结果。
func TestClient_ImageList(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, []protocol.ImageSummary{
			{ID: "sha256:abc123", RepoTags: []string{"alpine:3.20"}, Size: 1234567, Architecture: "amd64", Os: "linux"},
		})
	}))
	defer srv.Close()
	c := quickClient(srv)
	ctx := context.Background()

	list, err := c.ImageList(ctx, false, false)
	if err != nil {
		t.Fatalf("拉取镜像清单失败：%v", err)
	}
	if gotPath != "/api/v1/images?all=false" {
		t.Errorf("all=false 应显式进查询串且不带 dangling，got %q", gotPath)
	}
	if len(list) != 1 || list[0].ID != "sha256:abc123" || list[0].Size != 1234567 {
		t.Errorf("镜像投影解码不符：%+v", list)
	}
	if len(list[0].RepoTags) != 1 || list[0].RepoTags[0] != "alpine:3.20" {
		t.Errorf("RepoTags 应原样保留（数组不裁剪）：%+v", list[0].RepoTags)
	}

	// dangling=true 是 prune dry-run 两段式的第一段数据源。
	if _, err := c.ImageList(ctx, true, true); err != nil {
		t.Fatalf("悬空清单失败：%v", err)
	}
	if gotPath != "/api/v1/images?all=true&dangling=1" {
		t.Errorf("dangling 应以 1 传达，got %q", gotPath)
	}

	// 空清单：合法结果，不是错误。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusOK, []protocol.ImageSummary{})
	}))
	defer srv2.Close()
	list, err = quickClient(srv2).ImageList(ctx, false, false)
	if err != nil {
		t.Fatalf("空清单应是合法结果：%v", err)
	}
	if list == nil || len(list) != 0 {
		t.Errorf("空清单应为非 nil 空切片，got %+v", list)
	}
}

// TestClient_ImagePull 验证拉取链路：POST /images/pull、body {"ref":...}、
// 信封内层是 Agent 的汇总结果（最终状态/层清单）——汇总式呈现是方案
// 决策点 1，实时进度透传留 S7。
func TestClient_ImagePull(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RequestURI()
		raw := make([]byte, 4096)
		n, _ := r.Body.Read(raw)
		gotBody = string(raw[:n])
		writeEnvelope(w, http.StatusOK, protocol.PullResult{
			Status: "Status: Downloaded newer image for alpine:3.20",
			Layers: []string{"sha256:deadbeef", "sha256:cafebabe"},
		})
	}))
	defer srv.Close()

	res, err := quickClient(srv).ImagePull(context.Background(), "alpine:3.20")
	if err != nil {
		t.Fatalf("拉取失败：%v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/images/pull" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotPath)
	}
	if gotBody != `{"ref":"alpine:3.20"}` {
		t.Errorf("请求体应是 {\"ref\":...}，got %q", gotBody)
	}
	if res.Status != "Status: Downloaded newer image for alpine:3.20" {
		t.Errorf("最终状态应透传，got %q", res.Status)
	}
	if len(res.Layers) != 2 || res.Layers[0] != "sha256:deadbeef" {
		t.Errorf("层清单应透传，got %+v", res.Layers)
	}
	if res.Err != "" {
		t.Errorf("成功时 Err 应为空，got %q", res.Err)
	}
}

// TestClient_ImagePullStreamError 验证流内业务失败的透传口径：Engine 把
// 拉取错误放在 NDJSON 流里（HTTP 200），Agent 已映射成 502 信封错误，
// agentcli 原样透传 *protocol.Error，且写语义绝不重试。
func TestClient_ImagePullStreamError(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    protocol.CodeUpstreamError,
				"message": "拉取失败: pull access denied for nope/nope, repository does not exist",
			},
		})
	}))
	defer srv.Close()

	_, err := quickClient(srv).ImagePull(context.Background(), "nope/nope:latest")
	perr, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("应透传 protocol.Error，got %v", err)
	}
	if perr.Code != protocol.CodeUpstreamError {
		t.Errorf("流内失败应归一为 upstream_error，got %q", perr.Code)
	}
	if !strings.Contains(perr.Message, "repository does not exist") {
		t.Errorf("Engine 原话应带到，got %q", perr.Message)
	}
	if attempts != 1 {
		t.Errorf("写请求绝不重试，尝试次数 = %d", attempts)
	}
}

// TestClient_ImageBadRefLocalReject 验证本地校验先行：ref 会拼进 URL 路径
// （delete）或请求体（pull），字符集校验在出门前完成——该拒的请求不该
// 打到 Agent。pull 与 delete 共用同一套坏 ref 集。
func TestClient_ImageBadRefLocalReject(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		writeEnvelope(w, http.StatusOK, protocol.PullResult{})
	}))
	defer srv.Close()
	c := quickClient(srv)
	ctx := context.Background()

	bad := []string{
		"", "..", "a b", "x?y=1", "../etc/passwd", "a" + strings.Repeat("b", 255),
		"-bad", "a..b", "a//b", "a:", "a/", "a@",
		"a@md5:abc", "a@sha256:xyz", "a@b@c",
	}
	for _, ref := range bad {
		if _, err := c.ImagePull(ctx, ref); err == nil {
			t.Errorf("pull 坏 ref %q 应被本地拒绝", ref)
		}
		if err := c.ImageDelete(ctx, ref); err == nil {
			t.Errorf("delete 坏 ref %q 应被本地拒绝", ref)
		}
	}
	if hit {
		t.Error("坏 ref 不应发出任何 HTTP 请求")
	}
}

// TestClient_ImagePullTimeoutPolicy 钉住 pull 的超时策略：常量按大镜像
// 经内网拉取的量级取 10 分钟（与 dockercli.PullTimeout 同量级，两跳各管
// 一段）；Client.PullTimeout=0 用默认，自定义值经 once 生效。
func TestClient_ImagePullTimeoutPolicy(t *testing.T) {
	if ImagePullTimeout != 10*time.Minute {
		t.Fatalf("ImagePullTimeout 量级应为 10 分钟，got %v", ImagePullTimeout)
	}

	// 自定义 50ms：server 拖 300ms 才响应，应触发超时且归一为
	// upstream_unavailable（网络类失败，不是业务错误）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
		writeEnvelope(w, http.StatusOK, protocol.PullResult{})
	}))
	defer srv.Close()

	c := quickClient(srv)
	c.PullTimeout = 50 * time.Millisecond
	_, err := c.ImagePull(context.Background(), "alpine:3.20")
	perr, ok := err.(*protocol.Error)
	if !ok || perr.Code != protocol.CodeUpstreamUnavailable {
		t.Fatalf("自定义 PullTimeout 应生效并归一为 upstream_unavailable，got %v", err)
	}
}

// TestClient_ImageDelete 验证删除路径：ref 直拼（ValidImageRef 字符集在
// URL path 里全合法，转义反让两侧签名口径劈叉）；含 "/" 的多段引用
// （library/nginx:latest）路径必须完整抵达——Agent 侧尾通配符靠它。
func TestClient_ImageDelete(t *testing.T) {
	for _, ref := range []string{"alpine:3.20", "library/nginx:latest", "sha256:abc123"} {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.RequestURI()
			writeEnvelope(w, http.StatusOK, map[string]any{"ok": true})
		}))
		defer srv.Close()

		if err := quickClient(srv).ImageDelete(context.Background(), ref); err != nil {
			t.Fatalf("删除 %q 失败：%v", ref, err)
		}
		want := "/api/v1/images/" + ref
		if gotMethod != http.MethodDelete || gotPath != want {
			t.Errorf("删除 %q 请求不符：\ngot  %s %s\nwant DELETE %s", ref, gotMethod, gotPath, want)
		}
	}
}

// TestClient_ImagePrune 验证清理链路：POST /images/prune 无 body；响应是
// Engine 的清理清单（Deleted/Untagged 二形态对象数组）与回收空间，
// 解码后原样交给调用方——「删镜像」与「解 tag」不是一回事。
func TestClient_ImagePrune(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RequestURI()
		raw := make([]byte, 4096)
		n, _ := r.Body.Read(raw)
		gotBody = string(raw[:n])
		writeEnvelope(w, http.StatusOK, protocol.PruneResult{
			ImagesDeleted: []protocol.PrunedImage{
				{Deleted: "sha256:deadbeef"},
				{Untagged: "alpine:3.19"},
			},
			SpaceReclaimed: 98765432,
		})
	}))
	defer srv.Close()

	res, err := quickClient(srv).ImagePrune(context.Background())
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/images/prune" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotPath)
	}
	if gotBody != "" {
		t.Errorf("prune 不应带请求体，got %q", gotBody)
	}
	if res.SpaceReclaimed != 98765432 {
		t.Errorf("回收空间应透传，got %d", res.SpaceReclaimed)
	}
	if len(res.ImagesDeleted) != 2 || res.ImagesDeleted[0].Deleted != "sha256:deadbeef" ||
		res.ImagesDeleted[1].Untagged != "alpine:3.19" {
		t.Errorf("清理清单二形态应分别保留：%+v", res.ImagesDeleted)
	}
}
