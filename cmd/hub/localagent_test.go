// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/agentcli"
	"github.com/yy1319hua/dockdeck/internal/agentlink"
	"github.com/yy1319hua/dockdeck/internal/config"
)

// TestStartLocalAgent_ServesAuthenticatedInfo 验证本机 Agent 的闭环：
// 监听 :0 后 base_url 写的是实际端口、同一套令牌鉴权生效、info 端点可用。
func TestStartLocalAgent_ServesAuthenticatedInfo(t *testing.T) {
	h := newHub(t)
	cfg := config.Config{
		LocalAgentAddr:    "127.0.0.1:0",
		LocalDockerSocket: "/nonexistent/docker.sock",
	}
	la, err := startLocalAgent(context.Background(), h.st, cfg)
	if err != nil {
		t.Fatalf("startLocalAgent: %v", err)
	}
	t.Cleanup(func() { _ = la.srv.Close() })

	// base_url 必须是实际监听地址（:0 由内核分配），否则 Hub 调不通。
	conn, err := h.st.ServerConn(context.Background(), la.id)
	if err != nil {
		t.Fatalf("ServerConn: %v", err)
	}
	if want := "http://" + la.addr; conn.BaseURL != want {
		t.Errorf("base_url = %q，期望 %q", conn.BaseURL, want)
	}

	cli := agentcli.New(conn.BaseURL, conn.Token, nil)
	info, err := cli.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ProtocolVersion == "" || info.AgentVersion == "" {
		t.Errorf("info 字段缺失: %+v", info)
	}

	// 鉴权必须与远端 Agent 同款：错误令牌一律拒绝。
	bad := agentcli.New(conn.BaseURL, "wrong-token", nil)
	if _, err := bad.Info(context.Background()); err == nil {
		t.Fatal("错误令牌应被拒绝")
	}
}

// TestAgentFor_LocalUsesLoopbackHTTP 验证分发 seam：本机不查反向连接注册表，
// 直接返回指向 loopback 的 HTTP 客户端。
func TestAgentFor_LocalUsesLoopbackHTTP(t *testing.T) {
	h := newHub(t)
	la, err := startLocalAgent(context.Background(), h.st, config.Config{
		LocalAgentAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("startLocalAgent: %v", err)
	}
	t.Cleanup(func() { _ = la.srv.Close() })

	// 注册表给一个非 nil 实例：本机分支必须在查表之前返回，
	// 不能依赖「links 恰好为 nil」这个测试装配细节。
	h.srv.links = agentlink.NewRegistry(8)

	idStr := strconv.FormatInt(la.id, 10)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+idStr+"/containers", nil)
	req.SetPathValue("id", idStr)
	w := httptest.NewRecorder()
	cli, ok := h.srv.agentFor(w, req)
	if !ok {
		t.Fatalf("agentFor 失败: %d %s", w.Code, w.Body.String())
	}
	if want := "http://" + la.addr; cli.BaseURL != want {
		t.Errorf("BaseURL = %q，期望 %q", cli.BaseURL, want)
	}
	if cli.Link != nil {
		t.Error("本机不应绑定反向长连接")
	}
}

// TestStartLocalAgent_PortConflictReturnsError 验证端口占用时返回错误而不是
// 静默失败或 panic——调用方据此打告警并继续启动 Hub。
func TestStartLocalAgent_PortConflictReturnsError(t *testing.T) {
	h := newHub(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	defer ln.Close()

	if _, err := startLocalAgent(context.Background(), h.st, config.Config{
		LocalAgentAddr: ln.Addr().String(),
	}); err == nil {
		t.Fatal("端口被占用时 startLocalAgent 应返回错误")
	}
}
