// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package fakeagent 提供一个 httptest 可用的假 Agent：真实的鉴权中间件 +
// 可配置的指标响应。用途是端到端测试 Hub→Agent 链路（agentcli ↔ 鉴权 ↔ 端点），
// 不 mock 鉴权本身——链路测试的价值就在于连签名头一起验。
package fakeagent

import (
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// Fake 是假 Agent。零值不可用，用 New 构造。
type Fake struct {
	Token string

	mu         sync.Mutex
	info       protocol.AgentInfo
	metrics    *protocol.HostMetrics
	containers []protocol.ContainerStat
	// calls 记录每次进入业务端点的请求路径（已过鉴权），供断言「实际打到了哪」。
	calls []string
	// failNext 让下 N 次请求先返回给定状态码（测重试），随后恢复正常。
	failNext   int
	failStatus int
}

// New 构造假 Agent，各端点响应为合理的最小样本。
func New(token string) *Fake {
	return &Fake{Token: token}
}

// SetInfo / SetMetrics / SetContainers 配置响应体。
func (f *Fake) SetInfo(i protocol.AgentInfo) { f.mu.Lock(); f.info = i; f.mu.Unlock() }
func (f *Fake) SetMetrics(m *protocol.HostMetrics) {
	f.mu.Lock()
	f.metrics = m
	f.mu.Unlock()
}
func (f *Fake) SetContainers(cs []protocol.ContainerStat) {
	f.mu.Lock()
	f.containers = cs
	f.mu.Unlock()
}

// FailNext 让接下来 n 次请求返回 status（测读重试链路）。
func (f *Fake) FailNext(n, status int) {
	f.mu.Lock()
	f.failNext, f.failStatus = n, status
	f.mu.Unlock()
}

// Calls 返回已过鉴权的请求路径副本。
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// Handler 装配鉴权中间件 + 三个读端点。
// 写端点不在此提供：S2 的 Agent 只有读，写语义随 S3 一并进假 Agent。
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.Handler {
		return agenthttp.Auth(f.Token, nil)(h)
	}
	mux.Handle("/api/v1/info", authed(f.serveInfo))
	mux.Handle("/api/v1/metrics", authed(f.serveMetrics))
	mux.Handle("/api/v1/containers/stats", authed(f.serveContainers))
	return mux
}

// Server 起一个 httptest.Server（TLS），返回服务器与关闭函数。
// TLS 模拟真实部署：Agent 全程自签证书，Hub 侧 InsecureSkipVerify + 上层指纹信任。
func (f *Fake) Server() *httptest.Server {
	srv := httptest.NewTLSServer(f.Handler())
	return srv
}

// maybeFail 返回 true 表示本轮要失败。记录调用发生在失败之前：重试链路要数总请求数。
func (f *Fake) maybeFail(r *http.Request) (bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.RequestURI())
	if f.failNext > 0 {
		f.failNext--
		return true, f.failStatus
	}
	return false, 0
}

func (f *Fake) serveInfo(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	info := f.info
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, info)
}

func (f *Fake) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	m := f.metrics
	f.mu.Unlock()
	if m == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (f *Fake) serveContainers(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	cs := f.containers
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, cs)
}
