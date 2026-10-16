package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/auth"
	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/middleware"
	"github.com/yy1319hua/dockdeck/internal/openapi"
	"github.com/yy1319hua/dockdeck/internal/route"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// hub 是测试用的完整装配：真库、真会话、真 Token，只把监听换成 httptest。
type hub struct {
	srv     *server
	handler http.Handler
	st      *store.Store
	uid     int64
}

func newHub(t *testing.T) *hub {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	st := &store.Store{DB: d, Key: bytes.Repeat([]byte("K"), 32)}

	opt := auth.DefaultOptions()
	opt.Secure = false // httptest 走 http，Secure cookie 读不回来
	am := auth.NewManager(st, opt)

	srv := &server{
		st:     st,
		am:     am,
		tokens: apitoken.NewAPI(d),
		// 容量给足，避免限流把断言变成随机红。
		rl: middleware.NewRateLimit(1000, time.Minute),
	}
	handler := middleware.RequestCtx(httpx.SecurityHeaders(srv.routes()))

	uid, err := st.CreateUser(context.Background(), "admin", "longenough123")
	if err != nil {
		t.Fatal(err)
	}
	return &hub{srv: srv, handler: handler, st: st, uid: uid}
}

// session 走真实登录流程拿 cookie。
func (h *hub) session(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"longenough123"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("登录失败：%d %s", w.Result().StatusCode, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "dd_session" && c.Value != "" {
			return c.Value
		}
	}
	t.Fatal("登录未下发会话 cookie")
	return ""
}

// token 直接落库签发，绕开 HTTP 层以隔离被测对象。
func (h *hub) token(t *testing.T, name string, scopes ...string) string {
	t.Helper()
	tk, err := h.srv.tokens.Issue(context.Background(), h.uid, name, scopes, 0)
	if err != nil {
		t.Fatalf("签发 %s 失败: %v", name, err)
	}
	return tk.Plaintext
}

func (h *hub) get(t *testing.T, path, bearer, cookie string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "dd_session", Value: cookie})
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	resp := w.Result()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func (h *hub) do(t *testing.T, method, path, body, bearer, cookie string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "dd_session", Value: cookie})
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	resp := w.Result()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// ---- 一致性：登记表 ↔ spec ↔ 实际路由 ----

// TestSpecMatchesRouteTable 是 S1.10 的核心保证：
// 文档与实装同源，任何一侧单独漂移都会让这里红。
func TestSpecMatchesRouteTable(t *testing.T) {
	h := newHub(t)
	entries := h.srv.table.Entries()
	served := h.srv.spec().MethodPaths()

	// 两侧先归一到文档形态再比：Go mux 的尾通配符 {ref...} 在 spec 里是 {ref}
	// （openapi.SpecPath 是唯一口径），登记表一侧不规范化就比不出漂移。
	want := map[string]bool{}
	for _, e := range entries {
		want[e.Method+" "+openapi.SpecPath(e.Path)] = true
	}
	if len(want) != len(entries) {
		t.Errorf("规范化后端点键撞车：登记表 %d 条、去重后 %d 条（同一路径不能既声明 {x} 又声明 {x...}）",
			len(entries), len(want))
	}
	for k := range want {
		if !served[k] {
			t.Errorf("spec 缺端点 %s", k)
		}
	}
	// 反向：spec 不得凭空多出没登记的端点。
	for k := range served {
		if !want[k] {
			t.Errorf("spec 有 %q 但登记表没有", k)
		}
	}
}

// TestSpecAnnotationsComplete 把 S8.1 补齐的注解钉住：任何端点缺 summary /
// 分组 / 说明，或参数与响应缺说明，或用到没写说明的分组，都在这里红。
// 注解漏了不影响运行、只在客户端与文档里体现，所以只能靠测试兜。
func TestSpecAnnotationsComplete(t *testing.T) {
	h := newHub(t)
	d := h.srv.spec()

	for _, e := range h.srv.table.Entries() {
		key := e.Method + " " + e.Path
		if e.Summary == "" {
			t.Errorf("%s：缺 Summary", key)
		}
		if len(e.Tags) == 0 {
			t.Errorf("%s：缺 Tags（不进任何分组，文档里会散落无标题）", key)
		}
		if e.Desc == "" {
			t.Errorf("%s：缺 Desc", key)
		}
		for _, p := range e.Params {
			if p.Desc == "" {
				t.Errorf("%s：参数 %s 缺 Desc", key, p.Name)
			}
		}
		for _, r := range e.Responses {
			if r.Desc == "" {
				t.Errorf("%s：响应 %d 缺 Desc", key, r.Status)
			}
		}
		op := specOperation(t, d, e.Method, openapi.SpecPath(e.Path))
		if op == nil {
			continue
		}
		if op.Summary != e.Summary {
			t.Errorf("%s：spec 的 summary %q 与登记表 %q 不一致", key, op.Summary, e.Summary)
		}
		if len(op.Responses) == 0 {
			t.Errorf("%s：spec 里没有任何响应", key)
		}
	}

	// 分组说明：spec 里每个 tag 都要有说明（未登记的 tag 会渲染成空标题）。
	if len(d.Tags) == 0 {
		t.Error("spec 没有任何 tag")
	}
	for _, tag := range d.Tags {
		if tag.Description == "" {
			t.Errorf("分组 %s 在 spec 里没有说明（补 openapi.tagDesc）", tag.Name)
		}
	}
}

// TestCommittedSpecMatchesGenerated 钉住「产物只能由生成器产出」：现生成一份到
// 临时目录，与仓库里的 docs/openapi.json 与 docs/openapi.yaml 逐字节比对。
// 手改产物、或改了注解却忘了重新生成，都在这里红（spec 是公开契约，与实际
// 实装不一致的「文档」比没有文档更糟）。
func TestCommittedSpecMatchesGenerated(t *testing.T) {
	dir := t.TempDir()
	if err := dumpOpenAPI(dir); err != nil {
		t.Fatalf("生成 spec：%v", err)
	}
	for _, name := range []string{"openapi.json", "openapi.yaml"} {
		generated, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读现生成产物 %s：%v", name, err)
		}
		committed, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
		if err != nil {
			t.Fatalf("读仓库产物 docs/%s：%v", name, err)
		}
		if !bytes.Equal(committed, generated) {
			t.Errorf("docs/%s 与现生成不一致：产物过期或被手改——重跑 "+
				"`go run ./cmd/hub --dump-openapi docs` 后一并提交", name)
		}
	}
}

// specOperation 取端点在 spec 里的 operation；路径或方法对不上时报错并返回 nil。
func specOperation(t *testing.T, d *openapi.Doc, method, path string) *openapi.Operation {
	t.Helper()
	pi, ok := d.Paths[path]
	if !ok {
		t.Errorf("spec 里没有路径 %s", path)
		return nil
	}
	switch method {
	case http.MethodGet:
		return pi.Get
	case http.MethodPost:
		return pi.Post
	case http.MethodPut:
		return pi.Put
	case http.MethodPatch:
		return pi.Patch
	case http.MethodDelete:
		return pi.Delete
	}
	t.Errorf("未知方法 %s", method)
	return nil
}

// TestSpecHasNoDuplicateParameters 扫真实登记表的每一条：任何端点都不得
// 生成重名参数（同一 name+in 两份是非法 OpenAPI，Swagger UI 报错）。
// S2.7 指标端点与 S3.4 容器端点都在 Entry.Params 里声明了与路径同名的
// id/cid/action（为补描述/示例），修复前这些端点全部中招。
func TestSpecHasNoDuplicateParameters(t *testing.T) {
	h := newHub(t)
	d := h.srv.spec()
	for _, e := range h.srv.table.Entries() {
		pi := d.Paths[openapi.SpecPath(e.Path)]
		var op *openapi.Operation
		switch e.Method {
		case http.MethodGet:
			op = pi.Get
		case http.MethodPost:
			op = pi.Post
		case http.MethodPut:
			op = pi.Put
		case http.MethodPatch:
			op = pi.Patch
		case http.MethodDelete:
			op = pi.Delete
		}
		if op == nil {
			t.Errorf("%s %s：spec 里找不到 operation", e.Method, e.Path)
			continue
		}
		// 路径占位符集合：{id}/{cid}/{action} 这些名字只允许以 path 形态
		// 出现一次；Entry.Params 里的同名声明是给它们补描述/示例的，
		// 不该另生出 query 参数（修复前的形态，Swagger UI 报错）。
		placeholders := map[string]bool{}
		for _, seg := range strings.Split(e.Path, "/") {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				// 尾通配符 {ref...} 的文档名剥掉 "..."（与 openapi.pathParams 同口径）。
				placeholders[strings.TrimSuffix(seg[1:len(seg)-1], "...")] = true
			}
		}
		seen := map[string]bool{}
		for _, p := range op.Params {
			k := p.In + ":" + p.Name
			if seen[k] {
				t.Errorf("%s %s：参数 %s 重复出现", e.Method, e.Path, k)
			}
			seen[k] = true
			if placeholders[p.Name] && p.In != "path" {
				t.Errorf("%s %s：路径占位参数 %s 以 %s 形态出现，应为 path", e.Method, e.Path, p.Name, p.In)
			}
		}
		for name := range placeholders {
			if !seen["path:"+name] {
				t.Errorf("%s %s：缺路径参数 %s", e.Method, e.Path, name)
			}
		}
	}
}

// TestEveryDeclaredRouteIsActuallyRoutable 证明登记不只是写进表，确实注册进了 mux。
func TestEveryDeclaredRouteIsActuallyRoutable(t *testing.T) {
	h := newHub(t)
	for _, e := range h.srv.table.Entries() {
		req := httptest.NewRequest(e.Method, samplePath(e.Path), nil)
		_, pattern := h.srv.table.Mux().Handler(req)
		if want := e.Method + " " + e.Path; pattern != want {
			t.Errorf("声明 %s，实际命中 %q", want, pattern)
		}
	}
}

// TestNoRoutesBypassingTable 堵住唯一能让 spec 失真的写法：
// ServeMux 不支持反查已注册路由，所以直接审装配源码。
func TestNoRoutesBypassingTable(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go: %v", err)
	}
	// 逐行看真实注册语句，注释里提到 mux.Handle 不算。
	var registered []string
	for _, line := range strings.Split(string(src), "\n") {
		stmt := strings.TrimSpace(line)
		if strings.HasPrefix(stmt, "//") || !strings.Contains(stmt, "mux.Handle") {
			continue
		}
		registered = append(registered, stmt)
	}
	// 只允许 SPA 兜底那一处。
	if len(registered) != 1 {
		t.Errorf("main.go 有 %d 处 mux.Handle*，应为 1（仅 SPA 兜底）；API 端点请走 tbl.Add：%v",
			len(registered), registered)
	} else if !strings.Contains(registered[0], `("/", `) {
		t.Errorf("唯一的直接注册不是 SPA 兜底：%s", registered[0])
	}
	for _, l := range registered {
		if strings.Contains(l, `"/api`) {
			t.Errorf("绕过登记表注册了 API 路由：%s", l)
		}
	}
}

// ---- 鉴权矩阵 ----

func TestAuthMatrix(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)
	// 注：/api/v1/servers 要求 read:metrics。
	read := h.token(t, "ro", apitoken.ScopeReadMetrics)
	write := h.token(t, "rw", apitoken.ScopeWriteContainers)

	cases := []struct {
		why    string
		path   string
		bearer string
		cookie string
		want   int
	}{
		{"健康检查公开", "/api/v1/health", "", "", http.StatusOK},
		{"spec 公开", "/api/openapi.json", "", "", http.StatusOK},
		{"scope 字典公开", "/api/v1/scopes", "", "", http.StatusOK},

		{"scope 端点匿名 401", "/api/v1/servers", "", "", http.StatusUnauthorized},
		{"read:metrics 可读指标", "/api/v1/servers", read, "", http.StatusOK},
		{"write:containers 不含读指标", "/api/v1/servers", write, "", http.StatusForbidden},
		{"会话全权限", "/api/v1/servers", "", cookie, http.StatusOK},
		{"坏 Token 不得回落会话", "/api/v1/servers", "ddk_bogus", cookie, http.StatusUnauthorized},

		{"Token 管理需身份", "/api/v1/api-tokens", "", "", http.StatusUnauthorized},
		{"Token 不能管 Token", "/api/v1/api-tokens", read, "", http.StatusUnauthorized},
		{"会话可管 Token", "/api/v1/api-tokens", "", cookie, http.StatusOK},

		{"未知 /api 路径回 JSON 404", "/api/v1/nope", "", cookie, http.StatusNotFound},
		{"根路径给前端", "/", "", "", http.StatusOK},
	}
	for _, c := range cases {
		resp, body := h.get(t, c.path, c.bearer, c.cookie)
		if resp.StatusCode != c.want {
			t.Errorf("%s：%s %s bearer=%v cookie=%v 得 %d，期望 %d｜%s",
				c.why, "GET", c.path, c.bearer != "", c.cookie != "", resp.StatusCode, c.want, trim(body))
		}
	}
}

// TestDeclaredScopeEqualsEnforcedScope 防止「Entry.Scope 改了但包装链没跟上」。
// 每个 scope 端点都用一枚只含其它 scope 的 Token 打一遍，必须全部 403。
// 按条目自身的方法发请求：Go mux 的方法限定模式下，GET 打不中
// "POST /path" 模式（会落到 SPA 兜底拿 404），那测的就不是鉴权了。
// 越权 Token 在包装链就被拦，handler 不会执行，无副作用可言。
func TestDeclaredScopeEqualsEnforcedScope(t *testing.T) {
	h := newHub(t)
	probed := 0
	for _, e := range h.srv.table.Entries() {
		if e.AuthMode != route.AuthScope {
			continue
		}
		probed++
		other := apitoken.ScopeReadAudit
		if e.Scope == other {
			other = apitoken.ScopeReadContainers
		}
		tok := h.token(t, "probe-"+strconv.Itoa(probed), other)
		resp, body := h.do(t, e.Method, samplePath(e.Path), "", tok, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s（需 %s）带 scope %q 竟得 %d：%s",
				e.Method, e.Path, e.Scope, other, resp.StatusCode, trim(body))
		}
	}
	if probed == 0 {
		t.Fatal("没有 scope 端点可测，登记表退化了")
	}
}

// TestSessionOnlyEndpointsRejectEveryToken 覆盖 Tokens 分组：任何 Token 都不该满足它们。
func TestSessionOnlyEndpointsRejectEveryToken(t *testing.T) {
	h := newHub(t)
	// 给足全部 scope，仍然必须 401——权限大小与身份类型是两回事。
	all := h.token(t, "omni", apitoken.AllScopes...)

	for _, e := range h.srv.table.Entries() {
		if e.AuthMode != route.AuthSession {
			continue
		}
		if e.Method != http.MethodGet {
			continue // DELETE 有副作用，留给撤销流程测
		}
		resp, body := h.get(t, samplePath(e.Path), all, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("仅会话端点 %s 被全权限 Token 得 %d：%s", e.Path, resp.StatusCode, trim(body))
		}
	}
}

// ---- Token 全生命周期（走真实 HTTP）----

func TestTokenLifecycleOverHTTP(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)

	resp, body := h.do(t, http.MethodPost, "/api/v1/api-tokens",
		`{"name":"ios","scopes":["read:metrics"],"ttl":"30d"}`, "", cookie)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("签发应 201，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var env struct {
		Data struct {
			Token struct {
				ID        int64    `json:"id"`
				Name      string   `json:"name"`
				Scopes    []string `json:"scopes"`
				Active    bool     `json:"active"`
				ExpiresAt string   `json:"expires_at"`
			} `json:"token"`
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	plain := env.Data.Plaintext
	if !strings.HasPrefix(plain, apitoken.Prefix) {
		t.Fatalf("明文应有前缀，得 %q", plain)
	}
	if env.Data.Token.Active != true || env.Data.Token.ExpiresAt == "" {
		t.Fatalf("元数据不对：%+v", env.Data.Token)
	}

	// 明文能当真用。
	if resp, _ := h.get(t, "/api/v1/servers", plain, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("新 Token 读指标应 200，得 %d", resp.StatusCode)
	}

	// 列表里没有明文也没有哈希。
	resp, body = h.do(t, http.MethodGet, "/api/v1/api-tokens", "", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表应 200，得 %d", resp.StatusCode)
	}
	if s := string(body); strings.Contains(s, strings.TrimPrefix(plain, apitoken.Prefix)) {
		t.Error("列表响应泄漏了明文")
	}

	// 撤销后立刻不可用。
	id := strconv.FormatInt(env.Data.Token.ID, 10)
	if resp, _ := h.do(t, http.MethodDelete, "/api/v1/api-tokens/"+id, "", "", cookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("撤销应 200，得 %d", resp.StatusCode)
	}
	if resp, _ := h.get(t, "/api/v1/servers", plain, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("已撤销 Token 应 401，得 %d", resp.StatusCode)
	}
	// 再撤一次 404。
	if resp, _ := h.do(t, http.MethodDelete, "/api/v1/api-tokens/"+id, "", "", cookie); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复撤销应 404，得 %d", resp.StatusCode)
	}
	// 非法 id 400。
	for _, bad := range []string{"abc", "-3", "0"} {
		if resp, _ := h.do(t, http.MethodDelete, "/api/v1/api-tokens/"+bad, "", "", cookie); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("id %q 应 400，得 %d", bad, resp.StatusCode)
		}
	}
}

func TestCreateTokenRejectsBadInput(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)

	cases := []struct{ why, body, code string }{
		{"未知 scope", `{"name":"a","scopes":["nope"]}`, "bad_request"},
		{"空 scope", `{"name":"a","scopes":[]}`, "bad_request"},
		{"缺 name", `{"scopes":["read:metrics"]}`, "bad_request"},
		{"坏 ttl", `{"name":"a","scopes":["read:metrics"],"ttl":"5w"}`, "bad_ttl"},
		{"多余字段", `{"name":"a","scopes":["read:metrics"],"admin":true}`, "bad_request"},
	}
	for _, c := range cases {
		resp, body := h.do(t, http.MethodPost, "/api/v1/api-tokens", c.body, "", cookie)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400，得 %d｜%s", c.why, resp.StatusCode, trim(body))
			continue
		}
		if code := errorCode(t, body); code != c.code {
			t.Errorf("%s 的 code 应为 %s，得 %q", c.why, c.code, code)
		}
	}

	// 重名 409
	if resp, _ := h.do(t, http.MethodPost, "/api/v1/api-tokens",
		`{"name":"dup","scopes":["read:metrics"]}`, "", cookie); resp.StatusCode != http.StatusCreated {
		t.Fatal("首次签发应 201")
	}
	resp, body := h.do(t, http.MethodPost, "/api/v1/api-tokens",
		`{"name":"dup","scopes":["read:metrics"]}`, "", cookie)
	if resp.StatusCode != http.StatusConflict || errorCode(t, body) != "name_taken" {
		t.Errorf("重名应 409 name_taken，得 %d｜%s", resp.StatusCode, trim(body))
	}
}

func TestScopesEndpointShape(t *testing.T) {
	h := newHub(t)
	resp, body := h.get(t, "/api/v1/scopes", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d", resp.StatusCode)
	}
	var env struct {
		Data []struct {
			Scope       string `json:"scope"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data) != len(apitoken.AllScopes) {
		t.Fatalf("应 %d 项，得 %d", len(apitoken.AllScopes), len(env.Data))
	}
	for _, it := range env.Data {
		if it.Scope == "" || it.Description == "" {
			t.Errorf("字典项不完整：%+v", it)
		}
	}
}

// TestSecurityHeadersStillApplied 确认新增的公开端点没把安全头漏掉。
func TestSecurityHeadersStillApplied(t *testing.T) {
	h := newHub(t)
	for _, p := range []string{"/api/v1/health", "/api/openapi.json"} {
		resp, _ := h.get(t, p, "", "")
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s 缺安全头，X-Content-Type-Options=%q", p, got)
		}
		if got := resp.Header.Get("X-Request-Id"); got == "" {
			t.Errorf("%s 未注入 request id", p)
		}
	}
}

// ---- helpers ----

// samplePath 把 {id} 之类通配换成可请求的具体值。
func samplePath(p string) string {
	out := p
	for {
		i := strings.IndexByte(out, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(out[i:], '}')
		if j < 0 {
			return out
		}
		out = out[:i] + "1" + out[i+j+1:]
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env httpx.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析失败：%v｜%s", err, trim(body))
	}
	if env.Error == nil {
		return ""
	}
	return env.Error.Code
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}
