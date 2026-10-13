package apitoken

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/route"
)

// testServer 用登记表把 Token 管理端点接起来，装配方式应与 cmd/hub 保持一致。
type testServer struct {
	*API
	mux http.Handler
	uid int64
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	a, _, uid := newTestAPI(t)

	mux := http.NewServeMux()
	tbl := route.New(mux, func(e route.Entry, h http.Handler) http.Handler {
		if e.AuthMode == route.AuthSession {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fakeSession(w, r, h, e.Scope)
			})
		}
		return h
	})
	tbl.Add(route.Entry{Method: "GET", Path: "/api/v1/api-tokens", AuthMode: route.AuthSession}, a.ListHandler)
	tbl.Add(route.Entry{Method: "POST", Path: "/api/v1/api-tokens", AuthMode: route.AuthSession, Status: http.StatusCreated}, a.CreateHandler)
	tbl.Add(route.Entry{Method: "DELETE", Path: "/api/v1/api-tokens/{id}", AuthMode: route.AuthSession}, a.RevokeHandler)
	tbl.Add(route.Entry{Method: "GET", Path: "/api/v1/scopes", AuthMode: route.AuthNone}, ScopesHandler)

	return &testServer{API: a, mux: mux, uid: uid}
}

func (s *testServer) do(method, path string, body any, bearer, cookie string) (*http.Response, map[string]any) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
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
	s.mux.ServeHTTP(w, req)
	resp := w.Result()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var env httpx.Envelope
	_ = json.Unmarshal(b, &env)
	data := map[string]any{}
	if env.Data != nil {
		if m, ok := env.Data.(map[string]any); ok {
			data = m
		} else {
			data["__raw__"] = env.Data
		}
	}
	if env.Error != nil {
		data["__code__"] = env.Error.Code
	}
	return resp, data
}

const adminCookie = "sess-admin"

func TestCreateListRevokeOverHTTP(t *testing.T) {
	s := newTestServer(t)

	resp, data := s.do(http.MethodPost, "/api/v1/api-tokens", map[string]any{
		"name": "app", "scopes": []string{ScopeReadMetrics}, "ttl": "30d",
	}, "", adminCookie)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("签发应 201，得 %d %v", resp.StatusCode, data)
	}
	plain, _ := data["plaintext"].(string)
	if !strings.HasPrefix(plain, Prefix) {
		t.Fatalf("响应应含明文，得 %v", data)
	}
	tokMap, _ := data["token"].(map[string]any)
	if tokMap == nil || tokMap["active"] != true {
		t.Fatalf("token 元数据不对：%v", tokMap)
	}
	if _, leaked := tokMap["plaintext"]; leaked {
		t.Fatal("token 元数据里不应重复明文")
	}
	if _, leaked := tokMap["token_hash"]; leaked {
		t.Fatal("响应里带了哈希")
	}

	// 列表不含明文。
	resp, data = s.do(http.MethodGet, "/api/v1/api-tokens", nil, "", adminCookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表应 200，得 %d", resp.StatusCode)
	}
	raw, _ := json.Marshal(data["__raw__"])
	if strings.Contains(string(raw), strings.TrimPrefix(plain, Prefix)) {
		t.Fatal("列表响应里出现了明文片段")
	}

	// 撤销。
	id := strconv.FormatInt(int64(tokMap["id"].(float64)), 10)
	resp, _ = s.do(http.MethodDelete, "/api/v1/api-tokens/"+id, nil, "", adminCookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("撤销应 200，得 %d", resp.StatusCode)
	}
	// 库里应真的写了 revoked_at，而不是只把响应糊过去。
	var revoked sql.NullString
	if err := s.db.QueryRow(`SELECT revoked_at FROM api_tokens WHERE id=?`, id).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked.Valid {
		t.Fatal("撤销未落库")
	}

	resp, data = s.do(http.MethodDelete, "/api/v1/api-tokens/"+id, nil, "", adminCookie)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复撤销应 404，得 %d %v", resp.StatusCode, data)
	}

	// 非法 id
	if resp, _ := s.do(http.MethodDelete, "/api/v1/api-tokens/abc", nil, "", adminCookie); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非数字 id 应 400，得 %d", resp.StatusCode)
	}
	if resp, _ := s.do(http.MethodDelete, "/api/v1/api-tokens/-3", nil, "", adminCookie); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("负 id 应 400，得 %d", resp.StatusCode)
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		name string
		body map[string]any
		code string
	}{
		{"未知 scope", map[string]any{"name": "a", "scopes": []string{"nope"}}, "bad_request"},
		{"空 scope", map[string]any{"name": "a", "scopes": []string{}}, "bad_request"},
		{"缺 name", map[string]any{"scopes": []string{ScopeReadMetrics}}, "bad_request"},
		{"坏 ttl", map[string]any{"name": "a", "scopes": []string{ScopeReadMetrics}, "ttl": "5w"}, "bad_ttl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, data := s.do(http.MethodPost, "/api/v1/api-tokens", c.body, "", adminCookie)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("应 400，得 %d %v", resp.StatusCode, data)
			}
			if data["__code__"] != c.code {
				t.Fatalf("code 应为 %s，得 %v", c.code, data["__code__"])
			}
		})
	}

	// 重名 409
	if resp, d := s.do(http.MethodPost, "/api/v1/api-tokens",
		map[string]any{"name": "dup", "scopes": []string{ScopeReadMetrics}}, "", adminCookie); resp.StatusCode != http.StatusCreated {
		t.Fatalf("首次签发应 201，得 %d：%v", resp.StatusCode, d)
	}
	resp, data := s.do(http.MethodPost, "/api/v1/api-tokens",
		map[string]any{"name": "dup", "scopes": []string{ScopeReadMetrics}}, "", adminCookie)
	if resp.StatusCode != http.StatusConflict || data["__code__"] != "name_taken" {
		t.Fatalf("重名应 409 name_taken，得 %d %v", resp.StatusCode, data)
	}
}

func TestTokenEndpointsRejectBearer(t *testing.T) {
	s := newTestServer(t)
	tk, err := s.Issue(context.Background(), s.uid, "self-issue", []string{ScopeReadMetrics}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 一枚 Token 绝不能自己签发更多 Token，否则泄漏后权限无法收敛。
	resp, _ := s.do(http.MethodGet, "/api/v1/api-tokens", nil, tk.Plaintext, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Token 调用列表应 401，得 %d", resp.StatusCode)
	}
	resp, _ = s.do(http.MethodPost, "/api/v1/api-tokens",
		map[string]any{"name": "second", "scopes": []string{ScopeWriteFiles}}, tk.Plaintext, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Token 调用签发应 401，得 %d", resp.StatusCode)
	}
	// 未登录也不允许
	resp, _ = s.do(http.MethodPost, "/api/v1/api-tokens",
		map[string]any{"name": "second", "scopes": []string{ScopeWriteFiles}}, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名签发应 401，得 %d", resp.StatusCode)
	}
}

func TestCreateHandlerRejectsTokenPrincipal(t *testing.T) {
	// CreateHandler 只接受会话主体：即便中间件挂错、送进来一个 Token 主体，
	// 也必须 500 而不是静默拿 TokenID 当创建者签出新 Token。
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/api-tokens", strings.NewReader(`{"name":"x","scopes":["read:metrics"]}`))
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithUser(req.Context(), httpx.UserPrincipal{
		UserID: 1, Username: "admin", AuthKind: httpx.AuthKindToken, TokenID: 5,
	})
	w := httptest.NewRecorder()
	s.CreateHandler(w, req.WithContext(ctx))
	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("Token 主体直连应 500，得 %d", w.Result().StatusCode)
	}
}

func TestScopesHandler(t *testing.T) {
	s := newTestServer(t)
	resp, data := s.do(http.MethodGet, "/api/v1/scopes", nil, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scope 字典应公开 200，得 %d", resp.StatusCode)
	}
	list, _ := data["__raw__"].([]any)
	if len(list) != len(AllScopes) {
		t.Fatalf("应 %d 项，得 %d", len(AllScopes), len(list))
	}
	for _, it := range list {
		m, _ := it.(map[string]any)
		if m["scope"] == "" || m["description"] == "" {
			t.Fatalf("字典项不完整：%v", m)
		}
	}
}

func TestTTLParam(t *testing.T) {
	for _, good := range []string{"", "7d", "30d", "90d", "1y", "never", " 30d "} {
		if _, ok := TTLParam(good); !ok {
			t.Errorf("ttl %q 应被接受", good)
		}
	}
	for _, bad := range []string{"5w", "1d", "0", "30", "forever"} {
		if _, ok := TTLParam(bad); ok {
			t.Errorf("ttl %q 应被拒绝", bad)
		}
	}
}
