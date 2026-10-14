// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 端到端链路测试（读链路）：agentcli.Client ↔ fakeagent（真实 agenthttp 鉴权 + 自签 TLS）。
// 写签名链路的用例在 internal/agentcli 包内（需访问未导出的 do）。

package fakeagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/agentcli"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// e2eFixture 起一套「假 Agent + 真客户端」，返回假 Agent（取 TLS 地址用）与客户端。
func e2eFixture(t *testing.T, token string) (*Fake, *agentcli.Client) {
	t.Helper()
	f := New(token)
	f.SetInfo(protocol.AgentInfo{Hostname: "e2e", ProtocolVersion: protocol.ProtocolVersion, Cores: 4})
	m := &protocol.HostMetrics{CPU: protocol.CPUStat{Cores: 4, UsagePercent: 42.0}}
	f.SetMetrics(m)
	f.SetContainers([]protocol.ContainerStat{{ID: "c1", Name: "web", CPUPercent: 1.5, MemUsage: 10, MemLimit: 100, MemPercent: 10, PIDS: 1}})
	srv := f.Server()
	t.Cleanup(srv.Close)
	return f, agentcli.New(srv.URL, token, nil)
}

// TestE2E_FullReadChain 验证完整读链路：TLS 自签 → Bearer → 信封解码。
// 三个读端点一次走通，响应值与假 Agent 配置逐字段对上。
func TestE2E_FullReadChain(t *testing.T) {
	_, c := e2eFixture(t, "secret")

	res, err := c.Test(context.Background())
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.Info.Hostname != "e2e" || !res.ProtocolCompatible || res.Latency <= 0 {
		t.Errorf("Test 结果不符：%+v", res)
	}

	m, err := c.Metrics(context.Background())
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.CPU.Cores != 4 || m.CPU.UsagePercent != 42.0 {
		t.Errorf("Metrics 不符：%+v", m)
	}

	cs, err := c.ContainerStats(context.Background())
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	if len(cs) != 1 || cs[0].Name != "web" || cs[0].MemPercent != 10 {
		t.Errorf("ContainerStats 不符：%+v", cs)
	}
}

// TestE2E_WrongToken 验证凭证错误时 Agent 的 401 业务错误被客户端透传——
// 链路测试要能区分「Agent 拒绝」（unauthorized）和「网络不通」（upstream_unavailable）。
func TestE2E_WrongToken(t *testing.T) {
	_, c := e2eFixture(t, "secret")
	bad := agentcli.New(c.BaseURL, "wrong-token", nil)

	_, err := bad.Metrics(context.Background())
	perr, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("应透传 protocol.Error，got %v", err)
	}
	if perr.Code != protocol.CodeUnauthorized {
		t.Errorf("错误码 = %s，want %s", perr.Code, protocol.CodeUnauthorized)
	}
}

// TestE2E_ContainerReadChain 验证容器读链路（S3.4）经真实验签中间件：
// 清单的查询串（all）与 inspect 的路径都过 agenthttp.Auth——
// 查询串进签名口径（CanonicalPath 含查询串）在这里被真中间件把着，
// Calls 里能看到实际打到的路径字面。
func TestE2E_ContainerReadChain(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	f.SetContainerList([]protocol.ContainerSummary{
		{ID: "c1", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 3 hours"},
	})
	f.SetContainerInspect(map[string]json.RawMessage{
		"c1": json.RawMessage(`{"Id":"c1","Name":"/web","Config":{"Image":"nginx:1.27"}}`),
	})

	list, err := c.ContainerList(context.Background(), true)
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	if len(list) != 1 || list[0].Image != "nginx:1.27" || list[0].Names[0] != "/web" {
		t.Errorf("清单不符：%+v", list)
	}

	raw, err := c.ContainerInspect(context.Background(), "c1")
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if string(raw) != `{"Id":"c1","Name":"/web","Config":{"Image":"nginx:1.27"}}` {
		t.Errorf("详情应原样透传，got %s", raw)
	}

	var sawList, sawInspect bool
	for _, p := range f.Calls() {
		switch p {
		case "/api/v1/containers?all=true":
			sawList = true
		case "/api/v1/containers/c1":
			sawInspect = true
		}
	}
	if !sawList || !sawInspect {
		t.Errorf("调用路径不符：%v", f.Calls())
	}
}

// TestE2E_ContainerInspectMissing 验证不存在的容器：真 Agent 口径的 404
// 业务错误被客户端透传，不重试也不伪装成网络故障。
func TestE2E_ContainerInspectMissing(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	f.SetContainerInspect(map[string]json.RawMessage{})
	c.RetryBackoff = time.Nanosecond

	_, err := c.ContainerInspect(context.Background(), "nope")
	perr, ok := err.(*protocol.Error)
	if !ok || perr.Code != protocol.CodeNotFound {
		t.Fatalf("应透传 not_found，got %v", err)
	}
	got := 0
	for _, p := range f.Calls() {
		if strings.HasPrefix(p, "/api/v1/containers/nope") {
			got++
		}
	}
	if got != 1 {
		t.Errorf("确定性 404 不应重试，实际请求 %d 次", got)
	}
}

// TestE2E_ReadRetryAgainstRealServer 验证重试策略在真实 HTTP 往返下成立：
// 假 Agent 先吐两次 503，客户端第 3 次成功，总请求数 = 3。
func TestE2E_ReadRetryAgainstRealServer(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	c.RetryBackoff = time.Nanosecond

	f.FailNext(2, 503)
	if _, err := c.Metrics(context.Background()); err != nil {
		t.Fatalf("重试后应成功：%v", err)
	}
	got := 0
	for _, p := range f.Calls() {
		if strings.HasPrefix(p, "/api/v1/metrics") {
			got++
		}
	}
	if got != 3 {
		t.Errorf("应共 3 次请求，got %d：%v", got, f.Calls())
	}
}
