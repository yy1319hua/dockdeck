package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/store"
)

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	st := &store.Store{DB: d, Key: bytes.Repeat([]byte("K"), 32)}
	opt := DefaultOptions()
	opt.Secure = false // 测试走 http，不设 Secure cookie 才能被 httptest 读回
	return NewManager(st, opt), st
}

func doReq(m *Manager, method, path string, body any, jar *cookieJar) *http.Response {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if jar != nil && jar.value != "" {
		req.AddCookie(&http.Cookie{Name: "dd_session", Value: jar.value})
	}
	w := httptest.NewRecorder()
	m.handler().ServeHTTP(w, req)
	resp := w.Result()
	if jar != nil {
		if c := findCookie(resp.Cookies(), "dd_session"); c != nil {
			jar.value = c.Value
		}
	}
	return resp
}

func (m *Manager) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/auth/me", m.TryAuth(http.HandlerFunc(m.Me)))
	mux.Handle("POST /api/v1/auth/login", http.HandlerFunc(m.Login))
	mux.Handle("POST /api/v1/auth/setup", http.HandlerFunc(m.Setup))
	mux.Handle("POST /api/v1/auth/logout", m.RequireAuth(http.HandlerFunc(m.Logout)))
	mux.Handle("POST /api/v1/auth/password", m.RequireAuth(http.HandlerFunc(m.ChangePassword)))
	mux.Handle("GET /api/v1/protected", m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	})))
	return mux
}

type cookieJar struct{ value string }

func findCookie(cs []*http.Cookie, name string) *http.Cookie {
	for _, c := range cs {
		if c.Name == name && c.Value != "" && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

func TestSetupThenLoginFlow(t *testing.T) {
	m, st := newTestManager(t)
	jar := &cookieJar{}

	// 1) me 指示需要建号
	resp := doReq(m, "GET", "/api/v1/auth/me", nil, nil)
	var me struct {
		SetupRequired bool `json:"setup_required"`
	}
	readData(t, resp, &me)
	if !me.SetupRequired {
		t.Fatal("空库应 setup_required=true")
	}

	// 2) 建号（弱口令应被拒）
	resp = doReq(m, "POST", "/api/v1/auth/setup", map[string]string{"username": "admin", "password": "short"}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("弱口令应 400，得 %d", resp.StatusCode)
	}
	resp = doReq(m, "POST", "/api/v1/auth/setup", map[string]string{"username": "admin", "password": "longenough123"}, jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建号失败：%d", resp.StatusCode)
	}
	if jar.value == "" {
		t.Fatal("建号后应下发会话 cookie")
	}

	// 3) 已有账户后 setup 应 409
	resp = doReq(m, "POST", "/api/v1/auth/setup", map[string]string{"username": "x", "password": "longenough123"}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复建号应 409，得 %d", resp.StatusCode)
	}

	// 4) 带 cookie 访问受保护端点
	resp = doReq(m, "GET", "/api/v1/protected", nil, jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("已登录访问受保护端点应 200，得 %d", resp.StatusCode)
	}

	// 5) 错误口令登录 401，不产生会话
	bad := &cookieJar{}
	resp = doReq(m, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrongpass"}, bad)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误口令应 401，得 %d", resp.StatusCode)
	}
	if bad.value != "" {
		t.Fatal("错误口令不应下发 cookie")
	}

	// 6) 正确口令登录 200
	ok := &cookieJar{}
	resp = doReq(m, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "longenough123"}, ok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正确口令应 200，得 %d", resp.StatusCode)
	}

	// 7) 审计应有记录
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("审计应有记录")
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	m, _ := newTestManager(t)
	jar := &cookieJar{}
	if resp := doReq(m, "POST", "/api/v1/auth/setup", map[string]string{"username": "admin", "password": "longenough123"}, jar); resp.StatusCode != 200 {
		t.Fatal("setup 失败")
	}
	// 改密
	resp := doReq(m, "POST", "/api/v1/auth/password", map[string]string{
		"old_password": "longenough123", "new_password": "brandnewpass456",
	}, jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改密应 200，得 %d", resp.StatusCode)
	}
	// 旧 cookie 应立即失效
	resp = doReq(m, "GET", "/api/v1/protected", nil, jar)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("改密后旧会话应失效，得 %d", resp.StatusCode)
	}
	// 新口令可登录
	njar := &cookieJar{}
	resp = doReq(m, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "brandnewpass456"}, njar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("新口令应可登录，得 %d", resp.StatusCode)
	}
}

func readData(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var env httpx.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, b)
	}
	if env.Error != nil {
		t.Fatalf("响应错误: %s", env.Error.Message)
	}
	// 把 Data 再转成目标结构
	tmp, _ := json.Marshal(env.Data)
	if err := json.Unmarshal(tmp, dst); err != nil {
		t.Fatalf("转结构失败: %v", err)
	}
}
