// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentcli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// TestClient_ContainerList 验证清单读链路：all 参数显式进查询串
// （查询串是签名路径的一部分，Agent 侧收到的必须字字一致），
// 信封内层解码为协议类型。
func TestClient_ContainerList(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, []protocol.ContainerSummary{
			{ID: "abc123def456", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 3 hours"},
		})
	}))
	defer srv.Close()

	list, err := quickClient(srv).ContainerList(context.Background(), true)
	if err != nil {
		t.Fatalf("拉取清单失败：%v", err)
	}
	if gotPath != "/api/v1/containers?all=true" {
		t.Errorf("查询串应显式到达，got %q", gotPath)
	}
	if len(list) != 1 || list[0].ID != "abc123def456" || list[0].State != "running" {
		t.Errorf("清单解码不符：%+v", list)
	}
	if len(list[0].Names) != 1 || list[0].Names[0] != "/web" {
		t.Errorf("Names 应原样保留（数组不裁剪）：%+v", list[0].Names)
	}
}

// TestClient_ContainerListAllFalse 验证 all=false 也显式传值——
// Agent 侧只认字面 "true"，不依赖「不带参数算 false」的隐式约定。
func TestClient_ContainerListAllFalse(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, []protocol.ContainerSummary{})
	}))
	defer srv.Close()

	list, err := quickClient(srv).ContainerList(context.Background(), false)
	if err != nil {
		t.Fatalf("拉取清单失败：%v", err)
	}
	if gotPath != "/api/v1/containers?all=false" {
		t.Errorf("all=false 应显式进查询串，got %q", gotPath)
	}
	if len(list) != 0 {
		t.Errorf("空清单应是合法结果，got %+v", list)
	}
}

// TestClient_ContainerInspect 验证详情原样透传：信封内层的 Engine JSON
// 一个字节不改地交给调用方——Hub 不猜字段语义。
func TestClient_ContainerInspect(t *testing.T) {
	engine := `{"Id":"abc123def456","Name":"/web","State":{"Status":"running"},"Config":{"Image":"nginx:1.27"}}`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, json.RawMessage(engine))
	}))
	defer srv.Close()

	raw, err := quickClient(srv).ContainerInspect(context.Background(), "abc123def456")
	if err != nil {
		t.Fatalf("拉取详情失败：%v", err)
	}
	if gotPath != "/api/v1/containers/abc123def456" {
		t.Errorf("路径应与 Agent 路由一字不差，got %q", gotPath)
	}
	if string(raw) != engine {
		t.Errorf("详情应原样透传：\ngot  %s\nwant %s", raw, engine)
	}
}

// TestClient_ContainerInspectBadID 验证非法 ID 在本地就被拒、请求根本不出门——
// ID 会拼进 URL 路径，Hub 侧第一道校验不该指望网络往返。
func TestClient_ContainerInspectBadID(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		writeEnvelope(w, http.StatusOK, json.RawMessage(`{}`))
	}))
	defer srv.Close()

	for _, id := range []string{"", "..", "a/b", "a b", "x?y=1", "../etc/passwd", strings.Repeat("a", 129)} {
		_, err := quickClient(srv).ContainerInspect(context.Background(), id)
		if err == nil {
			t.Errorf("非法 ID %q 应被本地拒绝", id)
			continue
		}
		if !strings.Contains(err.Error(), "非法容器标识") {
			t.Errorf("错误文案应指明原因，got %v", err)
		}
	}
	if hit {
		t.Error("非法 ID 不应发出任何 HTTP 请求")
	}
}

// TestClient_ContainerInspectNotFound 验证容器不存在时业务错误原样透传
// （not_found 码与文案），不重试、不伪装成网络故障。
func TestClient_ContainerInspectNotFound(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"code": protocol.CodeNotFound, "message": "容器不存在"},
		})
	}))
	defer srv.Close()

	_, err := quickClient(srv).ContainerInspect(context.Background(), "abc123def456")
	perr, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("应透传 protocol.Error，got %v", err)
	}
	if perr.Code != protocol.CodeNotFound || perr.Message != "容器不存在" {
		t.Errorf("业务错误应原样透传，got %+v", perr)
	}
	if attempts != 1 {
		t.Errorf("确定性 404 不应重试，尝试次数 = %d", attempts)
	}
}

// TestClient_ContainerWriteOps 表驱动覆盖 8 个写方法：HTTP 方法、路径、
// 请求体逐项与 Agent 侧路由口径对齐（stop 的 timeout 走请求体、
// resize 的行列走查询串、remove 的 force/v 走查询串）。
func TestClient_ContainerWriteOps(t *testing.T) {
	timeout5 := 5
	timeout0 := 0
	cases := []struct {
		name       string
		call       func(context.Context, *Client) error
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{"start", func(ctx context.Context, c *Client) error { return c.ContainerStart(ctx, "c1") },
			"POST", "/api/v1/containers/c1/start", ""},
		{"stop-no-timeout", func(ctx context.Context, c *Client) error { return c.ContainerStop(ctx, "c1", nil) },
			"POST", "/api/v1/containers/c1/stop", ""},
		{"stop-with-timeout", func(ctx context.Context, c *Client) error { return c.ContainerStop(ctx, "c1", &timeout5) },
			"POST", "/api/v1/containers/c1/stop", `{"timeout_sec":5}`},
		// 显式 0 = SIGTERM 后立即停止（Docker 语义），必须如实抵达 Agent；
		// int+omitempty 时代它被当噪音丢掉，悄悄变成引擎默认 10s。
		{"stop-timeout-zero", func(ctx context.Context, c *Client) error { return c.ContainerStop(ctx, "c1", &timeout0) },
			"POST", "/api/v1/containers/c1/stop", `{"timeout_sec":0}`},
		{"restart", func(ctx context.Context, c *Client) error { return c.ContainerRestart(ctx, "c1") },
			"POST", "/api/v1/containers/c1/restart", ""},
		{"pause", func(ctx context.Context, c *Client) error { return c.ContainerPause(ctx, "c1") },
			"POST", "/api/v1/containers/c1/pause", ""},
		{"unpause", func(ctx context.Context, c *Client) error { return c.ContainerUnpause(ctx, "c1") },
			"POST", "/api/v1/containers/c1/unpause", ""},
		{"kill-default", func(ctx context.Context, c *Client) error { return c.ContainerKill(ctx, "c1", "") },
			"POST", "/api/v1/containers/c1/kill", ""},
		{"kill-signal", func(ctx context.Context, c *Client) error { return c.ContainerKill(ctx, "c1", "sigterm") },
			"POST", "/api/v1/containers/c1/kill", `{"signal":"SIGTERM"}`},
		{"resize", func(ctx context.Context, c *Client) error { return c.ContainerResize(ctx, "c1", 24, 80) },
			"POST", "/api/v1/containers/c1/resize?h=24&w=80", ""},
		{"remove-bare", func(ctx context.Context, c *Client) error { return c.ContainerRemove(ctx, "c1", false, false, "") },
			"DELETE", "/api/v1/containers/c1", ""},
		{"remove-force-v", func(ctx context.Context, c *Client) error { return c.ContainerRemove(ctx, "c1", true, true, "") },
			"DELETE", "/api/v1/containers/c1?force=1&v=1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.RequestURI()
				raw, _ := io.ReadAll(r.Body)
				gotBody = string(raw)
				writeEnvelope(w, http.StatusOK, map[string]any{"ok": true})
			}))
			defer srv.Close()

			if err := tc.call(context.Background(), quickClient(srv)); err != nil {
				t.Fatalf("%s 应成功：%v", tc.name, err)
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath || gotBody != tc.wantBody {
				t.Errorf("%s 请求不符：\ngot  %s %s body=%q\nwant %s %s body=%q",
					tc.name, gotMethod, gotPath, gotBody, tc.wantMethod, tc.wantPath, tc.wantBody)
			}
		})
	}
}

// TestClient_ContainerWriteLocalRejects 验证本地校验先行：非法 ID、非法信号、
// 非正行列数——请求根本不出门（该拒的流量不该打到 Agent）。
func TestClient_ContainerWriteLocalRejects(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		writeEnvelope(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer srv.Close()
	c := quickClient(srv)
	ctx := context.Background()

	badID := "..%2Fetc"
	checks := []struct {
		name string
		err  error
	}{
		{"start-bad-id", c.ContainerStart(ctx, badID)},
		{"remove-bad-id", c.ContainerRemove(ctx, badID, false, false, badID)},
		{"kill-bad-signal", c.ContainerKill(ctx, "c1", "SIGEVIL")},
		{"resize-zero", c.ContainerResize(ctx, "c1", 0, 80)},
		{"resize-negative", c.ContainerResize(ctx, "c1", 24, -1)},
	}
	for _, ck := range checks {
		if ck.err == nil {
			t.Errorf("%s 应被本地拒绝", ck.name)
		}
	}
	if hit {
		t.Error("本地校验失败的请求不应发出任何 HTTP 请求")
	}
}

// TestClient_ContainerWriteForbidden 验证 Agent 的自我保护守卫（403 forbidden）
// 原样到达调用方——Hub 需要区分「被守卫拒」与「网络不通」。
func TestClient_ContainerWriteForbidden(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"code": protocol.CodeForbidden, "message": "拒绝操作 Agent 自身容器（不碰自身红线）"},
		})
	}))
	defer srv.Close()

	err := quickClient(srv).ContainerStart(context.Background(), "self1")
	perr, ok := err.(*protocol.Error)
	if !ok || perr.Code != protocol.CodeForbidden {
		t.Fatalf("应透传 forbidden，got %v", err)
	}
	if attempts != 1 {
		t.Errorf("写请求绝不重试，尝试次数 = %d", attempts)
	}
}
