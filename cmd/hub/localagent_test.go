// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"context"
	"net"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/agentcli"
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
