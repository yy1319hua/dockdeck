// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeEngine 是真 unix socket 上的假 Engine API——不用 docker 也能把
// socket 连接、版本前缀、Header 解析整条链路测真。样本按 Engine 29.x 的真实形状裁剪。
type fakeEngine struct {
	listener net.Listener
	server   *http.Server
	mu       sync.Mutex
	paths    []string // 收到的请求 URI（含版本前缀与查询串），供断言
	pingOK   bool     // false 时 /_ping 返回 500，模拟引擎半死
	// images 是 GET /images/json 的响应体（JSON 原样）；nil 返回 []。
	// 收到的查询串（all/filters）随 paths 记录，供断言。
	images []byte
	// pullStream 是 POST /images/create 的响应体（NDJSON 原样）；nil 返回空流。
	// pullStatus 非零时以该状态码响应（模拟 Engine 拒绝拉取）。
	pullStream []byte
	pullStatus int
	// deleteStatus 非零时 DELETE /images/{name} 以该状态码响应；零值回 204。
	// pruneBody 是 POST /images/prune 的响应体（nil 返回空结果 JSON）。
	// pruneStatus 非零时以该状态码响应。
	deleteStatus int
	pruneBody    []byte
	pruneStatus  int
	// volumes / networks 是卷与网络两个只读端点（S7.9）的响应体。
	// 两者形态刻意不同（对象包数组 vs 裸数组），用来钉住「两处解码模板
	// 不通用」——把它们写成同一个样子的测试就抓不到抄错。
	volumes  []byte
	networks []byte
	// networkDetails 是 GET /networks/{id} 的响应体，按网络 ID 索引。
	// Engine 29 起接入容器名单只在详情端点给，清单端点一律 null——
	// 没有它就测不出「名单缺失」这个真实故障。
	networkDetails map[string][]byte
}

func newFakeEngine(t *testing.T, apiVersion string) (*fakeEngine, string) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	f := &fakeEngine{listener: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		if !f.pingOK {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Api-Version", apiVersion)
		w.Header().Set("Server", "Docker/29.1.3 (linux)")
		w.Header().Set("Ostype", "linux")
		w.Header().Set("Docker-Experimental", "false")
		w.Header().Set("Builder-Version", "2")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Version": "29.1.3", "ApiVersion": apiVersion, "MinAPIVersion": "1.24",
			"Os": "linux", "Arch": "arm64", "KernelVersion": "6.8.0",
			"GoVersion": "go1.24.0", "GitCommit": "abc1234", "Experimental": false,
		})
	})
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/logs") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("logline\n"))
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ID": "ABCD:1234", "Containers": 3, "ContainersRunning": 2,
			"ContainersPaused": 0, "ContainersStopped": 1, "Images": 9,
			"Name": "node1", "ServerVersion": "29.1.3", "OperatingSystem": "Ubuntu 24.04",
			"OSType": "linux", "Architecture": "aarch64", "NCPU": 4,
			"MemTotal": 8 * 1024 * 1024 * 1024, "DockerRootDir": "/var/lib/docker",
		})
	})
	mux.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/images/json":
			w.Header().Set("Content-Type", "application/json")
			if f.images == nil {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_, _ = w.Write(f.images)
		case r.Method == http.MethodPost && r.URL.Path == "/images/create":
			if f.pullStatus != 0 {
				w.WriteHeader(f.pullStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(f.pullStream)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/images/"):
			if f.deleteStatus != 0 {
				w.WriteHeader(f.deleteStatus)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/images/prune":
			if f.pruneStatus != 0 {
				w.WriteHeader(f.pruneStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if f.pruneBody == nil {
				_, _ = w.Write([]byte(`{"ImagesDeleted":null,"SpaceReclaimed":0}`))
				return
			}
			_, _ = w.Write(f.pruneBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// 卷与网络（S7.9）：只读两端点。nil 时回 Engine 的空形态
	// （卷是对象包空数组、网络是裸空数组），与真实引擎一致。
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if f.volumes == nil {
			_, _ = w.Write([]byte(`{"Volumes":[],"Warnings":null}`))
			return
		}
		_, _ = w.Write(f.volumes)
	})
	mux.HandleFunc("/networks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if f.networks == nil {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write(f.networks)
	})
	// 网络详情端点：接入容器名单只在这里给（Engine 29 行为）。
	mux.HandleFunc("/networks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/networks/")
		body, ok := f.networkDetails[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	// Engine 对版本前缀路由：/v1.51/version 与 /version 等价。
	// 剥掉 /vX.Y 前缀后再进 mux，模拟 Engine 的真实路由行为。
	strip := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.URL.RequestURI())
		if len(r.URL.Path) > 2 && r.URL.Path[0] == '/' && r.URL.Path[1] == 'v' {
			rest := r.URL.Path[1:]
			if i := strings.Index(rest, "/"); i > 0 && isVer(rest[1:i]) {
				r2 := r.Clone(r.Context())
				r2.URL.Path = rest[i:]
				r = r2
			}
		}
		mux.ServeHTTP(w, r)
	})
	f.server = &http.Server{Handler: strip}
	go func() { _ = f.server.Serve(ln) }()
	t.Cleanup(func() { _ = f.server.Close() })
	return f, sock
}

func (f *fakeEngine) record(path string) {
	f.mu.Lock()
	f.paths = append(f.paths, path)
	f.mu.Unlock()
}

// isVer 判断 "1.51" 这类版本段。
func isVer(seg string) bool {
	parts := strings.Split(seg, ".")
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

func (f *fakeEngine) hitPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// TestNegotiateAndVersionInfo 走主链路：协商 → 版本前缀生效 → 两个只读调用。
func TestNegotiateAndVersionInfo(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	f.pingOK = true
	c := New(sock)

	if err := c.NegotiateAPIVersion(context.Background()); err != nil {
		t.Fatalf("协商失败: %v", err)
	}
	if got := c.APIVersion(); got != "1.51" {
		t.Errorf("协商后 apiVersion = %q，want 1.51", got)
	}

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Version != "29.1.3" || v.MinAPIVersion != "1.24" || v.Arch != "arm64" {
		t.Errorf("Version 不符: %+v", v)
	}

	i, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if i.Containers != 3 || i.ContainersRunning != 2 || i.ContainersStopped != 1 || i.Name != "node1" {
		t.Errorf("Info 不符: %+v", i)
	}

	// 版本前缀必须出现在 version/info 路径上，/_ping 不带前缀。
	paths := f.hitPaths()
	want := []string{"/_ping", "/v1.51/version", "/v1.51/info"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("请求路径 = %v，want %v", paths, want)
	}
}

// TestPingHeaders 验证 Ping 对响应头的解析。
func TestPingHeaders(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	f.pingOK = true
	p, err := New(sock).Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.APIVersion != "1.51" || p.Server != "Docker/29.1.3 (linux)" || p.OSType != "linux" || p.Experimental {
		t.Errorf("Ping 头解析不符: %+v", p)
	}
	if p.BuilderVersion != "2" {
		t.Errorf("BuilderVersion = %q，want 2", p.BuilderVersion)
	}
}

// TestNegotiate_EngineDown 验证引擎不可达时协商失败且不篡改现有版本——
// 调用方决定重试还是报错，客户端不吞。
func TestNegotiate_EngineDown(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	f.pingOK = false
	c := New(sock)
	if err := c.NegotiateAPIVersion(context.Background()); err == nil {
		t.Fatal("引擎 500 应报错")
	}
	if got := c.APIVersion(); got != DefaultAPIVersion {
		t.Errorf("失败后应保持默认版本 %q，got %q", DefaultAPIVersion, got)
	}
}

// TestNegotiate_BadHeader 验证非法版本头被拒——「不猜语义」：
// 拿不到可信版本就不改前缀。
func TestNegotiate_BadHeader(t *testing.T) {
	f, sock := newFakeEngine(t, "v-1.51-beta?")
	f.pingOK = true
	c := New(sock)
	if err := c.NegotiateAPIVersion(context.Background()); err == nil {
		t.Fatal("非法 Api-Version 头应报错")
	}
	if got := c.APIVersion(); got != DefaultAPIVersion {
		t.Errorf("非法头后应保持默认版本，got %q", got)
	}
}

// TestUnreachableSocket 验证 socket 不存在时的错误可读（含路径）。
func TestUnreachableSocket(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "nope.sock"))
	_, err := c.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nope.sock") {
		t.Fatalf("错误应包含 socket 路径，got %v", err)
	}
}

// TestSocketRemoved_ errors on removed socket file.
func TestSocketFileCleanup(t *testing.T) {
	_, sock := newFakeEngine(t, "1.51")
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket 文件应存在: %v", err)
	}
}

// TestLogsQueryEscaping 验证 Logs 拼给 Engine 的查询串：stdout/stderr 固定
// 合流，since/until 经 QueryEscape——RFC3339 的 "+08:00" 必须成 %2B，
// 否则 Engine 解析查询串时 + 变空格、时区静默丢失；纯数字（Agent 归一化
// 后的形态）转义前后一致，不受影响。这条守的是库自身的防御：不把正确性
// 寄托在调用方已经归一化。
func TestLogsQueryEscaping(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)

	st, err := c.Logs(context.Background(), "abc123", LogsOptions{
		Tail: "100", Since: "2026-10-14T08:00:00+08:00", Until: "2026-10-15T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	_ = st.Close()

	paths := f.hitPaths()
	if len(paths) == 0 {
		t.Fatal("假 Engine 没收到请求")
	}
	got := paths[len(paths)-1]
	for _, want := range []string{
		"stdout=1&stderr=1",
		"tail=100",
		"since=2026-10-14T08%3A00%3A00%2B08%3A00", // + 必须活着到 Engine
		"until=2026-10-15T00%3A00%3A00Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Engine 查询串应含 %q，got %q", want, got)
		}
	}
	if strings.Contains(got, "since=2026-10-14T08:00:00") {
		t.Errorf("RFC3339 不应裸拼进查询串（+ 会被吃成空格）：%q", got)
	}

	// 归一化后的 Unix 秒形态：转义中性，原样抵达。
	st2, err := c.Logs(context.Background(), "abc123", LogsOptions{Since: "1700000000", Until: "0"})
	if err != nil {
		t.Fatalf("Logs(秒): %v", err)
	}
	_ = st2.Close()
	got2 := f.hitPaths()[len(f.hitPaths())-1]
	if !strings.Contains(got2, "since=1700000000") || !strings.Contains(got2, "until=0") {
		t.Errorf("Unix 秒应原样抵达，got %q", got2)
	}
}
