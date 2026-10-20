// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/config"
)

// fakeHub 起一个只会处理 /api/v1/servers/enroll 的假 Hub。
// respond 返回本次要回的状态码；调用次数通过 hits 观察。
func fakeHub(t *testing.T, respond func(hit int32) (int, string)) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/servers/enroll" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&hits, 1)
		code, body := respond(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func cfgFor(url string) config.AgentConfig {
	return config.AgentConfig{
		HubURL:     url,
		JoinSecret: "test-secret",
	}
}

// TestEnrollRetryTransientThenOK 守「Hub 还没起来时能自愈」：
// 先 503 两次、第三次成功，重试不得放弃。
func TestEnrollRetryTransientThenOK(t *testing.T) {
	srv, hits := fakeHub(t, func(n int32) (int, string) {
		if n < 3 {
			return http.StatusServiceUnavailable, `{"error":{"code":"upstream_unavailable","message":"hub 未就绪"}}`
		}
		return http.StatusCreated, `{"data":{"id":7,"name":"node-x"}}`
	})

	err := enrollRetryEvery(context.Background(), cfgFor(srv.URL), 5, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("重试后应成功，得 %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 3 {
		t.Errorf("调用次数 %d，期望 3", got)
	}
}

// TestEnrollRetryPermanentStops 守「凭据错就别重试」：
// 401 属永久失败，必须第一次就返回，不能刷 12 次无用日志。
func TestEnrollRetryPermanentStops(t *testing.T) {
	srv, hits := fakeHub(t, func(int32) (int, string) {
		return http.StatusUnauthorized, `{"error":{"code":"unauthenticated","message":"join_secret 无效"}}`
	})

	err := enrollRetryEvery(context.Background(), cfgFor(srv.URL), 5, time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	var ee *enrollError
	if !errors.As(err, &ee) || !ee.permanent {
		t.Fatalf("应判为永久失败，得 %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("永久失败应只试 1 次，实际 %d 次", got)
	}
}

// TestEnrollRetryGivesUp 守「有限次数后放弃」：调用方显式给了上限就必须尊重，
// 不能无限刷屏。
func TestEnrollRetryGivesUp(t *testing.T) {
	srv, hits := fakeHub(t, func(int32) (int, string) {
		return http.StatusServiceUnavailable, `{"error":{"code":"internal","message":"暂时不可用"}}`
	})

	err := enrollRetryEvery(context.Background(), cfgFor(srv.URL), 3, time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("连续失败应返回错误")
	}
	if got := atomic.LoadInt32(hits); got != 3 {
		t.Errorf("调用次数 %d，期望 3（上限即停）", got)
	}
}

// TestEnrollRetryContextCancel 守「可被取消」：无限重试（maxAttempts<=0）
// 必须能被 ctx 叫停，否则关不掉 Agent 进程。
func TestEnrollRetryContextCancel(t *testing.T) {
	srv, _ := fakeHub(t, func(int32) (int, string) {
		return http.StatusServiceUnavailable, `{"error":{"code":"internal","message":"暂时不可用"}}`
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := enrollRetryEvery(ctx, cfgFor(srv.URL), 0, time.Millisecond, 2*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 ctx 超时，得 %v", err)
	}
}

// TestPermanentEnrollStatus 把分级规则钉死：只有明确的临时态才重试。
func TestPermanentEnrollStatus(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusConflict, true},
		{http.StatusGone, true}, // 邀请过期
		{http.StatusBadRequest, true},
		{http.StatusRequestTimeout, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	}
	for _, c := range cases {
		if got := permanentEnrollStatus(c.code); got != c.want {
			t.Errorf("permanentEnrollStatus(%d) = %v，期望 %v", c.code, got, c.want)
		}
	}
}

// TestEnrollPayloadCarriesArchAndDockerVer 守「enroll 请求体带上报字段」：
// arch 必须是本进程 GOARCH，docker_ver 字段必须存在（值可为空——Engine 不可达时不卡纳管）。
func TestEnrollPayloadCarriesArchAndDockerVer(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":1,"name":"n"}}`))
	}))
	t.Cleanup(srv.Close)

	if err := enrollToHub(cfgFor(srv.URL)); err != nil {
		t.Fatalf("enrollToHub: %v", err)
	}
	if got["arch"] != runtime.GOARCH {
		t.Fatalf("arch 应为 %q，得 %q", runtime.GOARCH, got["arch"])
	}
	if _, ok := got["docker_ver"]; !ok {
		t.Fatal("请求体应含 docker_ver 字段（可为空串）")
	}
}

// TestAgentDockerVersion 守「上报的 docker_ver 取自本机 Engine /version」：
// 假 Engine 架在真 unix socket 上回 29.1.3；socket 不存在时 best-effort 回空串。
func TestAgentDockerVersion(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" || r.URL.Path == "/v1.44/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"29.1.3","ApiVersion":"1.44"}`))
			return
		}
		http.NotFound(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	if got := agentDockerVersion(config.AgentConfig{DockerSocket: sock}); got != "29.1.3" {
		t.Fatalf("应取到 Engine 版本，得 %q", got)
	}

	missing := filepath.Join(t.TempDir(), "missing.sock")
	if got := agentDockerVersion(config.AgentConfig{DockerSocket: missing}); got != "" {
		t.Fatalf("Engine 不可达应回空串，得 %q", got)
	}
}
