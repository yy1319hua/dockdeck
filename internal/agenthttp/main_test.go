// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agenthttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/collector"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

const smokeToken = "smoke-token"

// smokeCollector 用真实 /proc，但只统计临时目录，避免依赖宿主机挂载点。
func smokeCollector(t *testing.T) *collector.Collector {
	return &collector.Collector{ProcRoot: "/proc", Mounts: []string{t.TempDir()}, Interval: 0}
}

// authed 给请求加上 Bearer + 时间戳 + 签名。
func authed(t *testing.T, method, url, target string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Authorization", "Bearer "+smokeToken)
	req.Header.Set("X-DD-Timestamp", stamp)
	req.Header.Set("X-DD-Signature",
		crypto.SignRequest([]byte(smokeToken), stamp, method, target, body))
	return req
}

// TestRun_RejectsMissingToken 覆盖启动失败路径：
// 没有 token 必须启动失败，绝不能退化成无鉴权连接。
func TestRun_RejectsMissingToken(t *testing.T) {
	t.Setenv("DD_AGENT_TOKEN", "")
	if err := Run(nil); err == nil {
		t.Fatal("缺少 token 时 run 应返回错误")
	}
}

func TestBuildHandler_Unauthenticated(t *testing.T) {
	srv := httptest.NewServer(BuildHandler(smokeToken, smokeCollector(t), nil, NewContainersAPI("", "", nil, nil, nil), NewImagesAPI(""), NewComposeAPI(nil, "", ""), nil, NewResourcesAPI("")))
	defer srv.Close()
	for _, path := range []string{"/api/v1/info", "/api/v1/metrics"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("无凭证访问 %s 应返回 401，got %d", path, resp.StatusCode)
		}
	}
}

func TestBuildHandler_InfoAuthenticated(t *testing.T) {
	srv := httptest.NewServer(BuildHandler(smokeToken, smokeCollector(t), nil, NewContainersAPI("", "", nil, nil, nil), NewImagesAPI(""), NewComposeAPI(nil, "", ""), nil, NewResourcesAPI("")))
	defer srv.Close()

	resp, err := srv.Client().Do(authed(t, http.MethodGet, srv.URL+"/api/v1/info", "/api/v1/info", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正确签名应返回 200，got %d", resp.StatusCode)
	}
	var env struct {
		Data protocol.AgentInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if env.Data.Cores <= 0 || env.Data.OS == "" || env.Data.Arch == "" {
		t.Errorf("/info 返回内容异常：%+v", env.Data)
	}
	if env.Data.ProtocolVersion != protocol.ProtocolVersion {
		t.Errorf("协议版本 = %q", env.Data.ProtocolVersion)
	}
}

// TestBuildHandler_MetricsAuthenticated 端到端验证采集链路：
// 真读 /proc，确认返回的指标结构完整可用。
func TestBuildHandler_MetricsAuthenticated(t *testing.T) {
	srv := httptest.NewServer(BuildHandler(smokeToken, smokeCollector(t), nil, NewContainersAPI("", "", nil, nil, nil), NewImagesAPI(""), NewComposeAPI(nil, "", ""), nil, NewResourcesAPI("")))
	defer srv.Close()

	resp, err := srv.Client().Do(authed(t, http.MethodGet, srv.URL+"/api/v1/metrics", "/api/v1/metrics", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正确签名应返回 200，got %d", resp.StatusCode)
	}
	var env struct {
		Data protocol.HostMetrics `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	m := env.Data
	if m.CollectedAt.IsZero() {
		t.Error("CollectedAt 不应为零值")
	}
	if m.CPU.Cores <= 0 {
		t.Errorf("CPU 核数 = %d", m.CPU.Cores)
	}
	if m.Memory.Total == 0 {
		t.Error("内存总量不应为 0")
	}
	if m.UptimeSec == 0 {
		t.Error("在线时长不应为 0")
	}
	if m.Processes <= 0 {
		t.Errorf("进程数 = %d", m.Processes)
	}
	if len(m.Disks) != 1 {
		t.Errorf("磁盘项 = %d，want 1", len(m.Disks))
	}
	// CPU 占用率必须在 0–100 之间，不能是 NaN。
	if m.CPU.UsagePercent < 0 || m.CPU.UsagePercent > 100 {
		t.Errorf("CPU 占用率越界：%v", m.CPU.UsagePercent)
	}
}

// fakeContainerSource 是容器指标源的测试替身。
type fakeContainerSource struct {
	stats []protocol.ContainerStat
	err   error
}

func (f fakeContainerSource) ContainerStats() ([]protocol.ContainerStat, error) {
	return f.stats, f.err
}

// TestBuildHandler_ContainersStats 覆盖三条路径：
// 正常数据透传、源出错降级为 200+空列表、默认源（nil）返回空列表。
// 容器曲线缺失绝不能演变成 5xx——那会让 Hub 误判机器离线。
func TestBuildHandler_ContainersStats(t *testing.T) {
	want := []protocol.ContainerStat{{ID: "abc", Name: "web", CPUPercent: 12.5, MemUsage: 1024, MemLimit: 4096, MemPercent: 25, PIDS: 3}}
	cases := []struct {
		name    string
		src     collector.ContainerStatsSource
		wantLen int
	}{
		{"数据透传", fakeContainerSource{stats: want}, 1},
		{"源出错降级", fakeContainerSource{err: errors.New("boom")}, 0},
		{"默认源为空", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(BuildHandler(smokeToken, smokeCollector(t), tc.src, NewContainersAPI("", "", nil, nil, nil), NewImagesAPI(""), NewComposeAPI(nil, "", ""), nil, NewResourcesAPI("")))
			defer srv.Close()
			resp, err := srv.Client().Do(authed(t, http.MethodGet, srv.URL+"/api/v1/containers/stats", "/api/v1/containers/stats", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s 应返回 200，got %d", tc.name, resp.StatusCode)
			}
			var env struct {
				Data []protocol.ContainerStat `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("解析响应失败：%v", err)
			}
			if len(env.Data) != tc.wantLen {
				t.Fatalf("%s：容器数 = %d，want %d", tc.name, len(env.Data), tc.wantLen)
			}
			if tc.wantLen == 1 && env.Data[0] != want[0] {
				t.Errorf("数据应原样透传，got %+v", env.Data[0])
			}
		})
	}
}
