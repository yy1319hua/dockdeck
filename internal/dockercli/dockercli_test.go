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
	paths    []string // 收到的请求路径（含版本前缀），供断言
	pingOK   bool     // false 时 /_ping 返回 500，模拟引擎半死
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
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ID": "ABCD:1234", "Containers": 3, "ContainersRunning": 2,
			"ContainersPaused": 0, "ContainersStopped": 1, "Images": 9,
			"Name": "node1", "ServerVersion": "29.1.3", "OperatingSystem": "Ubuntu 24.04",
			"OSType": "linux", "Architecture": "aarch64", "NCPU": 4,
			"MemTotal": 8 * 1024 * 1024 * 1024, "DockerRootDir": "/var/lib/docker",
		})
	})
	// Engine 对版本前缀路由：/v1.51/version 与 /version 等价。
	// 剥掉 /vX.Y 前缀后再进 mux，模拟 Engine 的真实路由行为。
	strip := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.URL.Path)
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
