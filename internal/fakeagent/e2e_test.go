// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 端到端链路测试（读链路）：agentcli.Client ↔ fakeagent（真实 agenthttp 鉴权 + 自签 TLS）。
// 写签名链路的用例在 internal/agentcli 包内（需访问未导出的 do）。

package fakeagent

import (
	"context"
	"encoding/json"
	"io"
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

// TestE2E_ContainerWriteChain 验证写链路（S3.4）经真实验签中间件：
// 签名覆盖请求体与查询串——stop 的 timeout_sec、kill 的 signal、remove 的
// force/v 都与签名一起到达，Agent 回 {"ok":true} 信封。
func TestE2E_ContainerWriteChain(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	ctx := context.Background()

	if err := c.ContainerStart(ctx, "c1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	timeout5 := 5
	if err := c.ContainerStop(ctx, "c1", &timeout5); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.ContainerKill(ctx, "c1", "sigterm"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := c.ContainerResize(ctx, "c1", 24, 80); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := c.ContainerRemove(ctx, "c1", true, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	want := []string{
		"/api/v1/containers/c1/start",
		"/api/v1/containers/c1/stop",
		"/api/v1/containers/c1/kill",
		"/api/v1/containers/c1/resize?h=24&w=80",
		"/api/v1/containers/c1?force=1&v=1",
	}
	got := f.Calls()
	if len(got) != len(want) {
		t.Fatalf("调用次数 = %d，want %d：%v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("第 %d 次调用 = %q，want %q", i+1, got[i], w)
		}
	}
}

// TestE2E_ContainerWriteSelfGuard 验证自我保护守卫在写链路上生效：
// 命中自身容器（前缀匹配）→ 403 forbidden 原样透传给调用方。
func TestE2E_ContainerWriteSelfGuard(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	f.SetSelfID("self123")
	ctx := context.Background()

	// 全 ID 与前缀都算命中（与真 Agent 同口径）。
	for _, id := range []string{"self123", "self"} {
		err := c.ContainerStart(ctx, id)
		perr, ok := err.(*protocol.Error)
		if !ok || perr.Code != protocol.CodeForbidden {
			t.Fatalf("ContainerStart(%q) 应透传 forbidden，got %v", id, err)
		}
	}
	err := c.ContainerRemove(ctx, "self123", false, false)
	perr, ok := err.(*protocol.Error)
	if !ok || perr.Code != protocol.CodeForbidden {
		t.Fatalf("Remove 自身应透传 forbidden，got %v", err)
	}
}

// TestE2E_ContainerWriteNoRetry 验证写请求在真实 HTTP 往返下也绝不重试：
// 假 Agent 连吐两次 503，客户端只打一次——Agent 侧写操作尚无幂等去重，
// 重试可能把一次 stop 变成两次（红线）。
func TestE2E_ContainerWriteNoRetry(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	c.RetryBackoff = time.Nanosecond
	f.FailNext(2, 503)

	if err := c.ContainerStart(context.Background(), "c1"); err == nil {
		t.Fatal("两次 503 后应报错")
	}
	got := 0
	for _, p := range f.Calls() {
		if strings.HasPrefix(p, "/api/v1/containers/c1/start") {
			got++
		}
	}
	if got != 1 {
		t.Errorf("写请求绝不重试，实际请求 %d 次：%v", got, f.Calls())
	}
}

// TestE2E_LogsPlain 验证非 follow 日志：裸流原样到达（不走信封），
// 查询串（tail/timestamps）随签名一起过真实验签中间件。
func TestE2E_LogsPlain(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	f.SetLogs("2024-01-01T00:00:00Z line one\nline two\n")

	s, err := c.Logs(context.Background(), "c1", protocol.LogsOptions{Tail: "100", Timestamps: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	got, err := io.ReadAll(s.Body)
	if err != nil {
		t.Fatalf("读流失败：%v", err)
	}
	if err := s.Body.Close(); err != nil {
		t.Fatalf("关流失败：%v", err)
	}
	if string(got) != "2024-01-01T00:00:00Z line one\nline two\n" {
		t.Errorf("裸流应原样到达，got %q", got)
	}
	if !strings.Contains(s.ContentType, "text/plain") {
		t.Errorf("Content-Type 应透传，got %q", s.ContentType)
	}
	if calls := f.Calls(); len(calls) != 1 || !strings.Contains(calls[0], "tail=100") || !strings.Contains(calls[0], "timestamps=1") {
		t.Errorf("调用路径应含查询串：%v", calls)
	}
}

// TestE2E_LogsFollowNoTimeout 验证 follow 长连接免客户端超时：
// 客户端 ReadTimeout 设为 10ms，而假 Agent 每 30ms 吐一帧、共 5 帧——
// 若客户端把读超时施加在流上，第一帧后就被掐断。收到多帧即证明免超时生效。
func TestE2E_LogsFollowNoTimeout(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	c.ReadTimeout = 10 * time.Millisecond // 故意小于心跳间隔
	f.SetLogs("first frame\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := c.Logs(ctx, "c1", protocol.LogsOptions{Follow: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	got, err := io.ReadAll(s.Body) // 阻塞到假Agent吐完 5 帧收流
	if err != nil {
		t.Fatalf("读流失败（超时被误施加了？）：%v", err)
	}
	_ = s.Body.Close()
	body := string(got)
	if !strings.HasPrefix(body, "first frame\n") {
		t.Errorf("首帧应原样到达，got %q", body)
	}
	ticks := strings.Count(body, "--follow-tick-")
	if ticks < 2 {
		t.Errorf("follow 流应在读超时（10ms）之后继续存活，实收心跳 %d 帧：%q", ticks, body)
	}
}

// TestE2E_LogsExport 验证 export=1：octet-stream + 下载文件名，
// Engine 原始字节一个不改。
func TestE2E_LogsExport(t *testing.T) {
	f, c := e2eFixture(t, "secret")
	f.SetLogs("raw\x00engine\x01bytes")

	s, err := c.Logs(context.Background(), "c1", protocol.LogsOptions{Export: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	got, err := io.ReadAll(s.Body)
	if err != nil {
		t.Fatalf("读流失败：%v", err)
	}
	_ = s.Body.Close()
	if string(got) != "raw\x00engine\x01bytes" {
		t.Errorf("export 应原样透传，got %q", got)
	}
	if s.ContentType != "application/octet-stream" || s.Filename != "c1.log" {
		t.Errorf("export 响应头不符：%+v", s)
	}
}
