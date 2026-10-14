// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器端点测试：假 Engine 架在真 unix socket 上（与 dockercli 测试同一思路），
// Agent → dockercli → socket → 假 Engine 全链路真跑，只 mock Engine 本身。

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
)

// isVerSeg 判断 "1.51" 这类版本段（数字与点）。
func isVerSeg(seg string) bool {
	if seg == "" {
		return false
	}
	for _, ch := range seg {
		if (ch < 0x30 || ch > 0x39) && ch != 0x2E {
			return false
		}
	}
	return true
}

func qmark(q string) string {
	if q == "" {
		return ""
	}
	return "?" + q
}

// fakeDocker 是最小假 Engine：容器读写全走它，记录请求方法+路径供断言。
type fakeDocker struct {
	ln     net.Listener
	server *http.Server
	sock   string
	paths  chan string
	// 日志形态配置（serveLogs 设置后 /containers/*/logs 有响应）：
	logsMuxed  []byte
	logsFollow bool
	// 镜像形态配置（S4.1）：
	imgList      []byte // GET /images/json 响应体（nil 返 []）
	pullStream   []byte // POST /images/create 响应体
	pruneBody    []byte // POST /images/prune 响应体（nil 返空结果）
	deleteStatus int    // DELETE /images/{ref} 非零状态码
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{ln: ln, sock: sock, paths: make(chan string, 64)}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		f.paths <- r.URL.Path + "?all=" + r.URL.Query().Get("all")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"Id":"abc123def456","Names":["/web"],"Image":"nginx","State":"running","Status":"Up 1h","Created":1700000000}]`))
	})
	mux.HandleFunc("/images/json", func(w http.ResponseWriter, r *http.Request) {
		f.paths <- "GET " + r.URL.Path + qmark(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if f.imgList == nil {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write(f.imgList)
	})
	mux.HandleFunc("/images/create", func(w http.ResponseWriter, r *http.Request) {
		f.paths <- "POST " + r.URL.Path + qmark(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(f.pullStream)
	})
	mux.HandleFunc("/images/prune", func(w http.ResponseWriter, r *http.Request) {
		f.paths <- "POST " + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if f.pruneBody == nil {
			_, _ = w.Write([]byte(`{"ImagesDeleted":null,"SpaceReclaimed":0}`))
			return
		}
		_, _ = w.Write(f.pruneBody)
	})
	// DELETE /images/{ref...} 兜底：精确模式（json/create/prune）优先。
	mux.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		f.paths <- r.Method + " " + r.URL.Path
		if f.deleteStatus != 0 {
			w.WriteHeader(f.deleteStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		f.paths <- r.Method + " " + r.URL.Path + qmark(r.URL.RawQuery)
		if strings.Contains(r.URL.Path, "/logs") {
			f.serveLogs(w, r)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"Id":"abc123def456","Name":"/web","State":{"Running":true}}`))
			return
		}
		ok(w)
	})
	// Engine 对版本前缀路由等价处理：剥掉 /vX.Y 再进 mux。
	strip := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest := r.URL.Path; len(rest) > 2 && rest[:2] == "/v" {
			after := rest[2:] // "1.44/containers/..."
			if i := strings.IndexByte(after, 0x2F); i > 0 && isVerSeg(after[:i]) {
				r2 := r.Clone(r.Context())
				r2.URL.Path = after[i:] // "/containers/..."
				r = r2
			}
		}
		mux.ServeHTTP(w, r)
	})
	f.server = &http.Server{Handler: strip}
	go func() { _ = f.server.Serve(ln) }()
	t.Cleanup(func() { _ = f.server.Close() })
	return f
}

// agentWith 按给定 socket 与自身标识起一个鉴权完好的 Agent httptest 服务器。
func agentWith(t *testing.T, socket, self string) (*httptest.Server, *fakeDocker) {
	t.Helper()
	f := newFakeDocker(t)
	if socket == "" {
		socket = f.sock
	}
	srv := httptest.NewServer(buildHandler(smokeToken, smokeCollector(t), nil, newContainersAPI(socket, self), newImagesAPI(socket), newComposeAPI(nil)))
	t.Cleanup(srv.Close)
	return srv, f
}

// nextPath 读假 Engine 最近一次收到的请求路径。
func (f *fakeDocker) nextPath(t *testing.T) string {
	t.Helper()
	select {
	case p := <-f.paths:
		return p
	default:
		t.Fatal("假 Engine 没收到请求")
		return ""
	}
}

// TestContainers_List 验证清单转发与 all 参数透传。
func TestContainers_List(t *testing.T) {
	srv, f := agentWith(t, "", "")
	resp, body := hGET(t, srv, "/api/v1/containers?all=true")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，got %d：%s", resp.StatusCode, body)
	}
	if got := f.nextPath(t); got != "/containers/json?all=true" {
		t.Errorf("all 参数应透传，got %q", got)
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Data) != 1 || env.Data[0]["Id"] != "abc123def456" {
		t.Errorf("清单内容不符：%s err=%v", body, err)
	}
}

// TestContainers_Inspect 验证详情经信封原样转发（Engine 字段不增不减）。
// 信封是 Hub↔Agent 的既有契约：data 层是 Engine 原样 JSON，外层不掺业务包装。
func TestContainers_Inspect(t *testing.T) {
	srv, _ := agentWith(t, "", "")
	resp, body := hGET(t, srv, "/api/v1/containers/abc123def456")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，got %d：%s", resp.StatusCode, body)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Data["Name"] != "/web" {
		t.Fatalf("详情应经信封原样转发：%s err=%v", body, err)
	}
	if !contains(body, `"Name":"/web"`) {
		t.Errorf("Engine 原始字段应保留：%s", body)
	}
}

// TestContainers_WriteOps 验证生命周期写操作转发到正确的 Engine 路径。
func TestContainers_WriteOps(t *testing.T) {
	cases := []struct {
		method, url, wantPath string
		body                  string
	}{
		{http.MethodPost, "/api/v1/containers/abc123def456/start", "POST /containers/abc123def456/start", ""},
		{http.MethodPost, "/api/v1/containers/abc123def456/stop", "POST /containers/abc123def456/stop?t=5", `{"timeout_sec":5}`},
		{http.MethodPost, "/api/v1/containers/abc123def456/restart", "POST /containers/abc123def456/restart", ""},
		{http.MethodPost, "/api/v1/containers/abc123def456/pause", "POST /containers/abc123def456/pause", ""},
		{http.MethodPost, "/api/v1/containers/abc123def456/unpause", "POST /containers/abc123def456/unpause", ""},
		{http.MethodPost, "/api/v1/containers/abc123def456/kill", "POST /containers/abc123def456/kill?signal=SIGTERM", `{"signal":"sigterm"}`},
		{http.MethodDelete, "/api/v1/containers/abc123def456?force=1&v=1", "DELETE /containers/abc123def456/?force=1&v=1", ""},
	}
	for _, c := range cases {
		t.Run(c.wantPath, func(t *testing.T) {
			srv, f := agentWith(t, "", "")
			resp, body := hDo(t, srv, c.method, c.url, c.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("应 200，got %d：%s", resp.StatusCode, body)
			}
			if got := f.nextPath(t); got != c.wantPath {
				t.Errorf("Engine 路径 = %q，want %q", got, c.wantPath)
			}
		})
	}
}

// TestContainers_SelfProtected 验证自我保护：写入自身容器一律 403，
// 假 Engine 收不到任何请求（守卫在转发之前）。
func TestContainers_SelfProtected(t *testing.T) {
	srv, f := agentWith(t, "", "abc123def456")
	for _, c := range []struct{ method, url string }{
		{http.MethodPost, "/api/v1/containers/abc123def456/stop"},
		{http.MethodPost, "/api/v1/containers/abc/start"}, // 短前缀也算命中
		{http.MethodDelete, "/api/v1/containers/abc123def456"},
	} {
		resp, body := hDo(t, srv, c.method, c.url, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s 应 403，got %d：%s", c.method, c.url, resp.StatusCode, body)
		}
	}
	select {
	case p := <-f.paths:
		t.Errorf("守卫应拦截在转发之前，但 Engine 收到 %q", p)
	default:
	}
}

// TestContainers_BadTarget 验证非法 ID 直接 400，不打到 Engine。
func TestContainers_BadTarget(t *testing.T) {
	srv, f := agentWith(t, "", "")
	resp, _ := hDo(t, srv, http.MethodPost, "/api/v1/containers/..%2Fetc/start", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 ID 应 400，got %d", resp.StatusCode)
	}
	select {
	case p := <-f.paths:
		t.Errorf("非法 ID 不应触达 Engine，但收到 %q", p)
	default:
	}
}

// TestContainers_UnknownAction 验证白名单外的 action 返回 404。
func TestContainers_UnknownAction(t *testing.T) {
	srv, _ := agentWith(t, "", "")
	resp, _ := hDo(t, srv, http.MethodPost, "/api/v1/containers/abc123def456/update", `{}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("未知 action 应 404，got %d", resp.StatusCode)
	}
}

// ---- 小工具 ----

func hGET(t *testing.T, srv *httptest.Server, url string) (*http.Response, []byte) {
	t.Helper()
	return hDo(t, srv, http.MethodGet, url, "")
}

func hDo(t *testing.T, srv *httptest.Server, method, url, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+url, strings.NewReader(body))

	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Authorization", "Bearer "+smokeToken)
	req.Header.Set("X-DD-Timestamp", stamp)
	// 签名口径与 agenthttp.CanonicalPath 一致：RequestURI 含查询串。
	req.Header.Set("X-DD-Signature", signFor(method, url, []byte(body), stamp))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return resp, buf
}

func contains(b []byte, sub string) bool { return strings.Contains(string(b), sub) }

// signFor 与 main_test.go 的 authed 同口径。
func signFor(method, path string, body []byte, stamp string) string {
	return crypto.SignRequest([]byte(smokeToken), stamp, method, path, body)
}
