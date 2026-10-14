// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentcli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// TestClient_ComposeList 验证列举链路：GET /api/v1/compose、信封内层解码为
// 协议类型（name/path/status/services 投影正确）、空清单是合法结果（[] 非
// null）。Agent 侧 serveComposeList 在 nil 时也补 []，这里同样确认客户端
// 收到非 nil 空切片。
func TestClient_ComposeList(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, []protocol.ComposeProject{
			{
				Name:       "watchtower",
				Path:       "/opt/watchtower",
				ConfigFile: "/opt/watchtower/docker-compose.yml",
				Status:     "running",
				Services: []protocol.ComposeService{
					{Name: "watchtower", State: "running", Image: "containrrr/watchtower:latest"},
				},
			},
		})
	}))
	defer srv.Close()

	list, err := quickClient(srv).ComposeList(context.Background())
	if err != nil {
		t.Fatalf("列举失败：%v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/compose" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotPath)
	}
	if len(list) != 1 {
		t.Fatalf("应解码出 1 个项目，got %d", len(list))
	}
	p := list[0]
	if p.Name != "watchtower" || p.Path != "/opt/watchtower" || p.ConfigFile != "/opt/watchtower/docker-compose.yml" {
		t.Errorf("项目投影字段解码不符：%+v", p)
	}
	if p.Status != "running" {
		t.Errorf("status 应透传，got %q", p.Status)
	}
	if len(p.Services) != 1 || p.Services[0].Name != "watchtower" || p.Services[0].Image != "containrrr/watchtower:latest" {
		t.Errorf("Services 子结构应保留：%+v", p.Services)
	}

	// 空清单：合法结果，不是错误，也不是 nil。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusOK, []protocol.ComposeProject{})
	}))
	defer srv2.Close()
	list, err = quickClient(srv2).ComposeList(context.Background())
	if err != nil {
		t.Fatalf("空清单应是合法结果：%v", err)
	}
	if list == nil || len(list) != 0 {
		t.Errorf("空清单应为非 nil 空切片，got %+v", list)
	}
}

// TestClient_ComposeAction 验证写动作链路：POST /api/v1/compose/{project}/{action}、
// 四个白名单动词路径正确抵达、信封内层 Output 透传 compose 原生输出。Output
// 不另造文案，成败写在里面——这是与镜像汇总式呈现不同的地方。
func TestClient_ComposeAction(t *testing.T) {
	for _, action := range []string{"up", "down", "restart", "pull"} {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.RequestURI()
			writeEnvelope(w, http.StatusOK, protocol.ComposeActionResult{
				Output: " Container watchtower  Recreate\n + watchtower Started",
			})
		}))
		defer srv.Close()

		res, err := quickClient(srv).ComposeAction(context.Background(), "watchtower", action)
		if err != nil {
			t.Fatalf("动作 %q 失败：%v", action, err)
		}
		if gotMethod != http.MethodPost || gotPath != "/api/v1/compose/watchtower/"+action {
			t.Errorf("动作 %q 请求不符：\ngot  %s %s\nwant POST /api/v1/compose/watchtower/%s", action, gotMethod, gotPath, action)
		}
		if !strings.Contains(res.Output, "watchtower") {
			t.Errorf("Output 应透传 compose 原生输出，got %q", res.Output)
		}
	}
}

// TestClient_ComposeActionBadActionLocalReject 验证本地校验先行：动作会拼进
// URL 路径（/{project}/{action}），白名单在出门前完成——该拒的请求不该打到
// Agent。撤掉 ComposeAction 的白名单拦截会让 rm 之类抵达服务器，测试变红。
func TestClient_ComposeActionBadActionLocalReject(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		writeEnvelope(w, http.StatusOK, protocol.ComposeActionResult{})
	}))
	defer srv.Close()
	c := quickClient(srv)
	ctx := context.Background()

	bad := []string{"", "rm", "exec", "restart-all", "UP", " down", "../up"}
	for _, action := range bad {
		if _, err := c.ComposeAction(ctx, "watchtower", action); err == nil {
			t.Errorf("非法动作 %q 应被本地拒绝", action)
		}
	}
	if hit {
		t.Error("非法动作不应发出任何 HTTP 请求")
	}
}

// TestClient_ComposeLogs 验证日志流链路：GET /api/v1/compose/{project}/logs、
// 不经信封、复用 LogsStream 裸流（text/plain）；follow 进查询串。空项目名
// 本地拦不出门（撤掉该拦截会让空名抵达 /api/v1/compose//logs，测试变红）。
func TestClient_ComposeLogs(t *testing.T) {
	// follow=true：查询串应带 follow=1。
	var gotPathFollow string
	srvFollow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPathFollow = r.URL.RequestURI()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "watchtower  | time=\"2024-01-01T00:00:00Z\" level=info msg=ping\n")
	}))
	defer srvFollow.Close()

	ls, err := quickClient(srvFollow).ComposeLogs(context.Background(), "watchtower", true)
	if err != nil {
		t.Fatalf("打开 follow 流失败：%v", err)
	}
	if gotPathFollow != "/api/v1/compose/watchtower/logs?follow=1" {
		t.Errorf("follow 应进查询串，got %q", gotPathFollow)
	}
	if ls.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("ContentType 应原样透传 text/plain，got %q", ls.ContentType)
	}
	raw, _ := io.ReadAll(ls.Body)
	_ = ls.Body.Close()
	if !strings.Contains(string(raw), "level=info msg=ping") {
		t.Errorf("裸流内容应透传，got %q", string(raw))
	}

	// follow=false：不应带 follow 查询串。
	var gotPathNoFollow string
	srvNo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPathNoFollow = r.URL.RequestURI()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "snapshot\n")
	}))
	defer srvNo.Close()
	if _, err := quickClient(srvNo).ComposeLogs(context.Background(), "watchtower", false); err != nil {
		t.Fatalf("打开非 follow 流失败：%v", err)
	}
	if gotPathNoFollow != "/api/v1/compose/watchtower/logs" {
		t.Errorf("非 follow 不应带查询串，got %q", gotPathNoFollow)
	}

	// 空项目名：本地拒绝，不出门。
	var hitEmpty bool
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitEmpty = true
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "nope\n")
	}))
	defer srvEmpty.Close()
	if _, err := quickClient(srvEmpty).ComposeLogs(context.Background(), "", false); err == nil {
		t.Error("空项目名应被本地拒绝")
	}
	if hitEmpty {
		t.Error("空项目名不应发出任何 HTTP 请求")
	}
}
