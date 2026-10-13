package apitoken

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// newTestAPI 建一个带单个用户的库，返回 Token 访问器与底层句柄。
// 返回的 userStore 供断言直接查库。
func newTestAPI(t *testing.T) (*API, *store.Store, int64) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	st := &store.Store{DB: d, Key: bytes.Repeat([]byte("K"), 32)}
	uid, err := st.CreateUser(context.Background(), "admin", "longenough123")
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	return NewAPI(d), st, uid
}

func TestIssueAndLookup(t *testing.T) {
	a, st, uid := newTestAPI(t)
	ctx := context.Background()

	// 故意乱序输入，验证会去重排序。
	tk, err := a.Issue(ctx, uid, "ios-app", []string{ScopeWriteContainers, ScopeReadMetrics, "  "}, 0)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !strings.HasPrefix(tk.Plaintext, Prefix) {
		t.Fatalf("明文应带前缀 %q，得 %q", Prefix, tk.Plaintext)
	}
	want := []string{ScopeReadMetrics, ScopeWriteContainers}
	if strings.Join(tk.Scopes, ",") != strings.Join(want, ",") {
		t.Fatalf("scopes 应去重排序为 %v，得 %v", want, tk.Scopes)
	}

	got, err := a.Lookup(ctx, tk.Plaintext)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ID != tk.ID || got.Name != "ios-app" {
		t.Fatalf("Lookup 结果不符：%+v", got)
	}
	if got.Plaintext != "" {
		t.Fatal("Lookup 不应带回明文")
	}

	// 关键：库里既没有明文，也没有「前缀+某段」的形式能拼回明文。
	var hash string
	if err := st.DB.QueryRow(`SELECT token_hash FROM api_tokens WHERE id=?`, tk.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == tk.Plaintext {
		t.Fatal("库里存了明文")
	}
	if hash != crypto.HashToken(tk.Plaintext) {
		t.Fatal("库里存的哈希与明文对不上")
	}
	// 明文去掉前缀后的随机段也不应作为子串出现在任何列里。
	raw := strings.TrimPrefix(tk.Plaintext, Prefix)
	var dump string
	if err := st.DB.QueryRow(`SELECT token_hash || '|' || name || '|' || scopes FROM api_tokens WHERE id=?`, tk.ID).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, raw) {
		t.Fatal("库内某列含明文随机段")
	}
}

func TestIssueRejectsBadInput(t *testing.T) {
	a, _, uid := newTestAPI(t)
	ctx := context.Background()

	if _, err := a.Issue(ctx, uid, "x", []string{"read:nothing"}, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("未知 scope 应 ErrInvalidInput，得 %v", err)
	}
	if _, err := a.Issue(ctx, uid, "x", nil, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("空 scope 应 ErrInvalidInput，得 %v", err)
	}
	if _, err := a.Issue(ctx, uid, "", []string{ScopeReadMetrics}, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("空 name 应 ErrInvalidInput，得 %v", err)
	}
	if _, err := a.Issue(ctx, uid, strings.Repeat("n", 65), []string{ScopeReadMetrics}, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("超长 name 应 ErrInvalidInput，得 %v", err)
	}

	if _, err := a.Issue(ctx, uid, "dup", []string{ScopeReadMetrics}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Issue(ctx, uid, "dup", []string{ScopeReadMetrics}, 0); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("重名应 ErrNameTaken，得 %v", err)
	}
}

func TestLookupExpiresAndRevoked(t *testing.T) {
	a, st, uid := newTestAPI(t)
	ctx := context.Background()

	tk, err := a.Issue(ctx, uid, "will-expire", []string{ScopeReadMetrics}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Lookup(ctx, tk.Plaintext); err != nil {
		t.Fatalf("未过期应可用: %v", err)
	}
	// 直接把过期时间推到过去，比睡一秒可靠也快。
	if _, err := st.DB.Exec(`UPDATE api_tokens SET expires_at=? WHERE id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Lookup(ctx, tk.Plaintext); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("已过期应 ErrNotFound，得 %v", err)
	}

	ok, err := a.Issue(ctx, uid, "will-revoke", []string{ScopeReadMetrics}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 撤销前先有一次成功使用，验证 last_used_at 确实被写过。
	if _, err := a.Lookup(ctx, ok.Plaintext); err != nil {
		t.Fatal(err)
	}
	var used sql.NullString
	if err := st.DB.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id=?`, ok.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if !used.Valid {
		t.Fatal("成功使用后 last_used_at 应被记录")
	}

	hit, err := a.Revoke(ctx, ok.ID)
	if err != nil || !hit {
		t.Fatalf("撤销应命中，hit=%v err=%v", hit, err)
	}
	if _, err := a.Lookup(ctx, ok.Plaintext); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("已撤销应 ErrNotFound，得 %v", err)
	}
	if hit, err := a.Revoke(ctx, ok.ID); err != nil || hit {
		t.Fatalf("重复撤销应未命中，hit=%v err=%v", hit, err)
	}
	if hit, err := a.Revoke(ctx, 9999); err != nil || hit {
		t.Fatalf("撤销不存在的 id 应未命中，hit=%v err=%v", hit, err)
	}
}

func TestLookupUnknownToken(t *testing.T) {
	a, _, _ := newTestAPI(t)
	if _, err := a.Lookup(context.Background(), Prefix+"nonexistent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知 token 应 ErrNotFound，得 %v", err)
	}
}

func TestListOmitsSecret(t *testing.T) {
	a, _, uid := newTestAPI(t)
	ctx := context.Background()
	tk, _ := a.Issue(ctx, uid, "listed", []string{ScopeReadAudit}, 0)

	ts, err := a.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("应 1 条，得 %d", len(ts))
	}
	if ts[0].Plaintext != "" {
		t.Fatal("List 不应返回明文")
	}
	raw := strings.TrimPrefix(tk.Plaintext, Prefix)
	if strings.Contains(strings.Join(ts[0].Scopes, ","), raw) {
		t.Fatal("scopes 里混入了令牌内容")
	}
}

// ---- 鉴权包装 ----

// fakeSession 模拟会话中间件：cookie 值是 "sess-admin" 就放行并注入会话主体。
func fakeSession(w http.ResponseWriter, r *http.Request, next http.Handler, scope string) {
	c, err := r.Cookie("dd_session")
	if err != nil || c.Value != "sess-admin" {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "需登录")
		return
	}
	ctx := httpx.WithUser(r.Context(), httpx.UserPrincipal{
		UserID: 1, Username: "admin", Session: c.Value, AuthKind: httpx.AuthKindSession,
	})
	next.ServeHTTP(w, r.WithContext(ctx))
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.CurrentUser(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"username": p.Username, "kind": p.AuthKind, "token_name": p.TokenName,
	})
}

func call(h http.Handler, path string, bearer, cookie string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "dd_session", Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

func TestRequireScopes(t *testing.T) {
	a, _, uid := newTestAPI(t)
	ctx := context.Background()
	read, _ := a.Issue(ctx, uid, "readonly", []string{ScopeReadMetrics}, 0)
	write, _ := a.Issue(ctx, uid, "rw", []string{ScopeReadMetrics, ScopeWriteContainers}, 0)

	guarded := Require(fakeSession, a, ScopeWriteContainers)(http.HandlerFunc(echoHandler))

	// 无凭据 → 401
	if resp := call(guarded, "/x", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，得 %d", resp.StatusCode)
	}
	// scope 不足 → 403（身份成立，是权限不够，不是没登录）
	if resp := call(guarded, "/x", read.Plaintext, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 scope 应 403，得 %d", resp.StatusCode)
	}
	// scope 齐 → 200，且主体是 Token 来源
	resp := call(guarded, "/x", write.Plaintext, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合格 Token 应 200，得 %d", resp.StatusCode)
	}
	var env httpx.Envelope
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	data := env.Data.(map[string]any)
	if data["kind"] != httpx.AuthKindToken || data["token_name"] != "rw" {
		t.Fatalf("主体不对：%+v", data)
	}
	// 会话来源天然全权限
	if resp := call(guarded, "/x", "", "sess-admin"); resp.StatusCode != http.StatusOK {
		t.Fatalf("会话应放行，得 %d", resp.StatusCode)
	}
	// 坏 Token → 401，且不许借 cookie 蒙混
	if resp := call(guarded, "/x", Prefix+"bogus", "sess-admin"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("坏 Token 应 401，得 %d", resp.StatusCode)
	}
	// scope 为空表示该端点只要身份成立即可，不额外要求权限项
	open := Require(fakeSession, a, "")(http.HandlerFunc(echoHandler))
	if resp := call(open, "/x", read.Plaintext, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("未要求 scope 时只读 Token 应 200，得 %d", resp.StatusCode)
	}
}

func TestRequirePrefersBearerOverCookie(t *testing.T) {
	a, _, uid := newTestAPI(t)
	ctx := context.Background()
	read, _ := a.Issue(ctx, uid, "ro", []string{ScopeReadMetrics}, 0)

	// 同时带 cookie 与低权限 Token：必须只按 Token 判，否则只读 Token
	// 就能借浏览器会话的全权限越权。
	guarded := Require(fakeSession, a, ScopeWriteContainers)(http.HandlerFunc(echoHandler))
	if resp := call(guarded, "/x", read.Plaintext, "sess-admin"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("混用时应按 Token 判 403，得 %d", resp.StatusCode)
	}
}

func TestRequireRejectsDisabledCreator(t *testing.T) {
	a, st, uid := newTestAPI(t)
	ctx := context.Background()
	tk, _ := a.Issue(ctx, uid, "orphan", []string{ScopeReadMetrics}, 0)
	if _, err := st.DB.Exec(`UPDATE users SET disabled=1 WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	guarded := Require(fakeSession, a, "")(http.HandlerFunc(echoHandler))
	resp := call(guarded, "/x", tk.Plaintext, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("创建者被禁用应 403，得 %d", resp.StatusCode)
	}
}

func TestTryBearer(t *testing.T) {
	a, _, uid := newTestAPI(t)
	ctx := context.Background()
	tk, _ := a.Issue(ctx, uid, "try", []string{ScopeReadMetrics}, 0)

	var seen string
	h := a.TryBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := httpx.CurrentUser(r)
		if ok {
			seen = p.AuthKind
		} else {
			seen = "anon"
		}
		w.WriteHeader(http.StatusOK)
	}))

	if resp := call(h, "/x", "", ""); resp.StatusCode != http.StatusOK || seen != "anon" {
		t.Fatalf("无 Bearer 应匿名放行，seen=%s", seen)
	}
	if resp := call(h, "/x", tk.Plaintext, ""); resp.StatusCode != http.StatusOK || seen != httpx.AuthKindToken {
		t.Fatalf("有效 Bearer 应注入 token 主体，seen=%s", seen)
	}
	// 坏 Bearer 必须明确拒绝，不许降级成匿名
	if resp := call(h, "/x", Prefix+"bogus", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("坏 Bearer 应 401，得 %d", resp.StatusCode)
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"", "", false},
		{"Bearer ", "", false},
		{"Bearer   ", "", false},
		{"Basic abc", "", false},
		{"bearer abc", "", false}, // 方案名按 RFC 区分大小写由服务端定；这里固定只认 Bearer
		{"Bearer abc", "abc", true},
		{"Bearer  abc  ", "abc", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		got, ok := ExtractBearer(r)
		if got != c.want || ok != c.ok {
			t.Errorf("ExtractBearer(%q) = %q,%v 期望 %q,%v", c.header, got, ok, c.want, c.ok)
		}
	}
}
