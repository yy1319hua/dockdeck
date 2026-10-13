package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ok200(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

func TestAddRegistersAndNormalizes(t *testing.T) {
	var wrapped []string
	mux := http.NewServeMux()
	tbl := New(mux, func(e Entry, h http.Handler) http.Handler {
		wrapped = append(wrapped, e.AuthMode+":"+e.Scope)
		return h
	})

	tbl.Add(Entry{
		Method: "get", Path: "api/v1/servers/{id}/metrics/", AuthMode: AuthScope,
		Scope: ScopeForTest, Summary: "x",
	}, ok200)

	// 路径与方法应被规范化。
	es := tbl.Entries()
	if len(es) != 1 {
		t.Fatalf("应 1 条，得 %d", len(es))
	}
	if es[0].Path != "/api/v1/servers/{id}/metrics" {
		t.Fatalf("路径未规范化：%q", es[0].Path)
	}
	if es[0].Method != "GET" {
		t.Fatalf("方法未大写：%q", es[0].Method)
	}
	if es[0].Status != http.StatusOK {
		t.Fatalf("Status 应默认 200，得 %d", es[0].Status)
	}
	if len(wrapped) != 1 || wrapped[0] != AuthScope+":"+ScopeForTest {
		t.Fatalf("包装未按声明套用：%v", wrapped)
	}

	// 注册确实生效：路径带通配也能命中。
	rec := httptest.NewRecorder()
	tbl.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/servers/7/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("路由未命中，得 %d", rec.Code)
	}
	// 错误方法应 405，说明方法约束真的进了 mux。
	rec2 := httptest.NewRecorder()
	tbl.Mux().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/v1/servers/7/metrics", nil))
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("错误方法应 405，得 %d", rec2.Code)
	}
}

const ScopeForTest = "read:metrics"

func TestAddPanicsOnBadDeclarations(t *testing.T) {
	cases := []struct {
		name string
		e    Entry
		why  string
	}{
		{"非 /api 前缀", Entry{Method: "GET", Path: "/v1/x", AuthMode: AuthNone}, "以 /api/ 开头"},
		{"非法方法", Entry{Method: "FETCH", Path: "/api/v1/x", AuthMode: AuthNone}, "非法方法"},
		{"空方法", Entry{Method: "", Path: "/api/v1/x", AuthMode: AuthNone}, "非法方法"},
		{"scope 端点未声明 scope", Entry{Method: "GET", Path: "/api/v1/x", AuthMode: AuthScope}, "未声明所需 scope"},
		{"未知鉴权模式", Entry{Method: "GET", Path: "/api/v1/x", AuthMode: "magic"}, "鉴权模式非法"},
		{"缺鉴权模式", Entry{Method: "GET", Path: "/api/v1/x"}, "鉴权模式非法"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer assertPanic(t, c.why)
			New(http.NewServeMux(), nil).Add(c.e, ok200)
		})
	}

	t.Run("重复声明", func(t *testing.T) {
		tbl := New(http.NewServeMux(), nil)
		tbl.Add(Entry{Method: "GET", Path: "/api/v1/x", AuthMode: AuthNone}, ok200)
		defer assertPanic(t, "重复声明")
		tbl.Add(Entry{Method: "GET", Path: "/api/v1/x", AuthMode: AuthNone}, ok200)
	})

	t.Run("nil handler", func(t *testing.T) {
		defer assertPanic(t, "未提供 handler")
		New(http.NewServeMux(), nil).Add(Entry{Method: "GET", Path: "/api/v1/x", AuthMode: AuthNone}, nil)
	})
}

func assertPanic(t *testing.T, want string) {
	t.Helper()
	r := recover()
	if r == nil {
		t.Fatalf("期望 panic 含 %q，实际没有 panic", want)
	}
	if s, _ := r.(string); !strings.Contains(s, want) {
		t.Fatalf("panic 文案应含 %q，得 %v", want, r)
	}
}

func TestEntriesReturnsCopy(t *testing.T) {
	tbl := New(http.NewServeMux(), nil)
	tbl.Add(Entry{Method: "GET", Path: "/api/v1/b", AuthMode: AuthNone, Summary: "first", Tags: []string{"A"}}, ok200)
	tbl.Add(Entry{Method: "GET", Path: "/api/v1/a", AuthMode: AuthNone, Summary: "second"}, ok200)

	es := tbl.Entries()
	if es[0].Path != "/api/v1/a" {
		t.Fatalf("Entries 应按路径排序，首位 %q", es[0].Path)
	}
	// 改副本不应影响登记表。
	es[0].Summary = "mutated"
	if got := tbl.Entries(); got[0].Summary != "second" {
		t.Fatalf("Entries 返回了内部引用，副本被改成 %q", got[0].Summary)
	}
}

func TestKeys(t *testing.T) {
	tbl := New(http.NewServeMux(), nil)
	tbl.Add(Entry{Method: "GET", Path: "/api/v1/x", AuthMode: AuthNone}, ok200)
	tbl.Add(Entry{Method: "DELETE", Path: "/api/v1/x/{id}", AuthMode: AuthSession}, ok200)

	k := tbl.Keys()
	if len(k) != 2 {
		t.Fatalf("应 2 个键，得 %d", len(k))
	}
	for _, want := range []string{"GET /api/v1/x", "DELETE /api/v1/x/{id}"} {
		if _, ok := k[want]; !ok {
			t.Fatalf("缺键 %q，现有 %v", want, keysOf(k))
		}
	}
}

func keysOf(m map[string]Entry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
