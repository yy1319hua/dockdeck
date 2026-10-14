// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentcli

import (
	"context"
	"encoding/json"
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
