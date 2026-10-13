// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// writeEnvelope 按 Agent 侧 httpx 的信封格式写响应。
func writeEnvelope(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
}

// quickClient 构造一个重试等待为零的客户端，避免测试拖慢。
func quickClient(srv *httptest.Server) *Client {
	c := New(srv.URL, "tok", nil)
	c.RetryBackoff = time.Nanosecond
	return c
}

// TestClient_TestConnectivity 验证 /test：连通、耗时被记录、协议兼容性正确上报。
func TestClient_TestConnectivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/info" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeEnvelope(w, http.StatusOK, protocol.AgentInfo{Hostname: "h1", ProtocolVersion: protocol.ProtocolVersion, Cores: 4})
	}))
	defer srv.Close()

	res, err := quickClient(srv).Test(context.Background())
	if err != nil {
		t.Fatalf("连通检查失败：%v", err)
	}
	if res.Info.Hostname != "h1" || res.Info.Cores != 4 {
		t.Errorf("Info 不完整：%+v", res.Info)
	}
	if !res.ProtocolCompatible {
		t.Error("版本一致时应报告兼容")
	}
	if res.Latency <= 0 {
		t.Error("Latency 应被记录")
	}
}

// TestClient_ProtocolMismatch 验证版本不一致时只标记不报错——
// 「兼不兼容」是上层的纳管决策，客户端只如实报告。
func TestClient_ProtocolMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusOK, protocol.AgentInfo{ProtocolVersion: "0.0.0"})
	}))
	defer srv.Close()

	res, err := quickClient(srv).Test(context.Background())
	if err != nil {
		t.Fatalf("不应因版本不同而报错：%v", err)
	}
	if res.ProtocolCompatible {
		t.Error("版本不一致应标记不兼容")
	}
}

// TestClient_ReadRetryOnTransient 验证读请求对 5xx 重试并最终成功。
func TestClient_ReadRetryOnTransient(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeEnvelope(w, http.StatusOK, protocol.HostMetrics{})
	}))
	defer srv.Close()

	c := quickClient(srv)
	if _, err := c.Metrics(context.Background()); err != nil {
		t.Fatalf("两次瞬态失败后应成功：%v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("尝试次数 = %d，want 3", got)
	}
}

// TestClient_ReadRetryExhausted 验证重试耗尽后错误归一为 upstream_error。
func TestClient_ReadRetryExhausted(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := quickClient(srv)
	_, err := c.Metrics(context.Background())
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.CodeUpstreamError {
		t.Fatalf("耗尽后应归一为 upstream_error，got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(c.MaxReadTries) {
		t.Errorf("尝试次数 = %d，want %d", got, c.MaxReadTries)
	}
}

// TestClient_ReadNoRetryOnDeterministic 验证非 5xx/429 的状态（如 401）不重试——
// 那是凭证或请求本身的问题，重试只会白打三倍流量。
func TestClient_ReadNoRetryOnDeterministic(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := quickClient(srv).Info(context.Background())
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.CodeUpstreamError {
		t.Fatalf("401 应归一为 upstream_error，got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("确定性失败不应重试，尝试次数 = %d", got)
	}
}

// TestClient_BusinessErrorPassthrough 验证信封业务错误原样透传（码与文案）。
func TestClient_BusinessErrorPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"code": protocol.CodeConflict, "message": "容器未运行"},
		})
	}))
	defer srv.Close()

	_, err := quickClient(srv).Info(context.Background())
	var perr *protocol.Error
	if !errors.As(err, &perr) {
		t.Fatalf("应透传为 protocol.Error，got %v", err)
	}
	if perr.Code != protocol.CodeConflict || perr.Message != "容器未运行" {
		t.Errorf("业务错误应原样透传，got %+v", perr)
	}
}

// TestClient_WriteNeverRetried 验证写请求遇到 5xx 也只打一次——
// Agent 侧写操作尚无幂等去重，重试可能把一次 stop 变成两次。
func TestClient_WriteNeverRetried(t *testing.T) {
	var attempts int32
	var sig, stamp string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		sig, stamp = r.Header.Get("X-DD-Signature"), r.Header.Get("X-DD-Timestamp")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := quickClient(srv)
	c.Now = func() time.Time { return time.Unix(1700000000, 0) }
	_, err := c.do(context.Background(), http.MethodPost, "/api/v1/future-write", []byte("{}"), nil)
	var perr *protocol.Error
	if !errors.As(err, &perr) {
		t.Fatalf("写失败应归一为 protocol.Error，got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("写请求绝不重试，实际尝试 %d 次", got)
	}
	if sig == "" || stamp != "1700000000" {
		t.Errorf("写操作应附带签名与时间戳，got sig=%q stamp=%q", sig, stamp)
	}
}

// TestClient_ReadTimeout 验证读超时生效并归一为 upstream_unavailable。
func TestClient_ReadTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		writeEnvelope(w, http.StatusOK, protocol.HostMetrics{})
	}))
	defer srv.Close()

	c := quickClient(srv)
	c.ReadTimeout = 5 * time.Millisecond
	_, err := c.Metrics(context.Background())
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.CodeUpstreamUnavailable {
		t.Fatalf("超时应归一为 upstream_unavailable，got %v", err)
	}
}

// TestClient_ContainerStats 验证容器快照读链路（含空列表也是合法结果）。
func TestClient_ContainerStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusOK, []protocol.ContainerStat{{ID: "a", Name: "web", CPUPercent: 3.5}})
	}))
	defer srv.Close()

	stats, err := quickClient(srv).ContainerStats(context.Background())
	if err != nil {
		t.Fatalf("拉取失败：%v", err)
	}
	if len(stats) != 1 || stats[0].ID != "a" || stats[0].CPUPercent != 3.5 {
		t.Errorf("数据不一致：%+v", stats)
	}
}

// TestRetryableStatus 表驱动覆盖状态码分级，防止有人改判定条件时漏掉 429。
func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		400: false, 401: false, 404: false, 409: false,
		429: true, 500: true, 502: true, 503: true, 504: true,
	}
	for code, want := range cases {
		if got := retryableStatus(code); got != want {
			t.Errorf("retryableStatus(%d) = %v，want %v", code, got, want)
		}
	}
}
