package assets

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	webassets "github.com/yy1319hua/dockdeck/web"
)

// 用 MapFS 造资源，两个用例都不依赖「前端是否已构建」——
// 换了机器、没跑过 npm run build 也必须绿。
func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":     &fstest.MapFile{Data: []byte("<html>DockDeck index</html>")},
		"assets/app.css": &fstest.MapFile{Data: []byte("body{margin:0}")},
	}
}

func TestSPAHandlerServesKnownFile(t *testing.T) {
	h := spaHandler(testFS())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("已知文件应直出，实际状态 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "body{margin:0}") {
		t.Fatalf("已知文件内容应原样返回，实际：%q", rec.Body.String())
	}
}

func TestSPAHandlerFallsBackToIndex(t *testing.T) {
	h := spaHandler(testFS())

	for _, path := range []string{"/", "/servers/42", "/nonexistent/deep/path"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应回落 index.html，实际状态 %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "DockDeck index") {
			t.Fatalf("%s 应返回 index.html 内容，实际：%q", path, rec.Body.String())
		}
	}
}

// Handler 走哪条分支取决于 dist 里有没有 index.html，两者都得能用：
// 有产物 → 真前端；只有 .gitkeep → 占位页。无论哪种，根路径都该是 200 且非空。
func TestHandlerServesRootInEitherBuildState(t *testing.T) {
	h := Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("根路径应 200（产物或占位页兜底），实际 %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("根路径响应体不应为空——兜底失败意味着 fresh clone 起 Hub 会白屏")
	}
}

// 真产物只在跑过 npm run build 后才存在，所以本例是条件性的：
// 构建过 → 验证产物确实被嵌进来且含 SPA 挂载点（这是 S6.5 的交付要点）；
// 没构建 → 跳过，由上一例保证占位页兜底仍然可用。
func TestDistEmbedsBuiltAssets(t *testing.T) {
	sub, err := fs.Sub(webassets.Dist, "dist")
	if err != nil {
		// GAP-8 已让 dist 目录随仓库存在，走到这里说明目录又没了。
		t.Fatalf("dist 应始终可内嵌：%v", err)
	}

	if _, err := fs.Stat(sub, "index.html"); err != nil {
		t.Skipf("前端未构建，跳过（dist 内无 index.html）：%v", err)
	}

	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("读取内嵌产物失败：%v", err)
	}
	if !strings.Contains(string(data), `id="app"`) {
		t.Fatalf("内嵌的 index.html 应含 SPA 挂载点，实际前 200 字节：%q", string(data[:min(200, len(data))]))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
