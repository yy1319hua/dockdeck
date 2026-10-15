// Package assets 提供 Hub 的静态资源服务。
//
// 资源来源分两级：
//  1. 前端构建产物 web/dist（S6.5 起的内嵌目标）；
//  2. 产物尚未构建时（dist 里只有 .gitkeep），回退到 S1 的占位页，
//     避免 fresh clone 直接起 Hub 得到一片空白——编译也永不因此失败。
//
// 为什么 embed 声明不写在本包：go:embed 的 pattern 只能引用当前包目录树内的路径，
// 不允许 ..，也不允许跨到别的包目录。产物在 web/dist，故声明放在 web 包内（web/assets.go）。
package assets

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	webassets "github.com/yy1319hua/dockdeck/web"
)

//go:embed all:static
var staticFS embed.FS

// Handler 返回 SPA 静态资源处理器：命中文件直接返回，未命中回 index.html。
func Handler() http.Handler {
	// 真产物优先：dist 里存在 index.html 才认为前端已构建。
	if sub, err := fs.Sub(webassets.Dist, "dist"); err == nil {
		if _, statErr := fs.Stat(sub, "index.html"); statErr == nil {
			return spaHandler(sub)
		}
	}

	// 未构建：回退占位页，保证 Hub 起得来、页面不是空白。
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// 内嵌目录缺失属构建期问题，运行期以 500 兜底而非 panic。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "assets unavailable", http.StatusInternalServerError)
		})
	}
	return spaHandler(sub)
}

// spaHandler 把任意 fs.FS 包成 SPA 服务：文件命中直出，未命中回落 index.html。
// 单独抽出来是为了能用 fstest.MapFS 直接测，不必依赖「前端是否已构建」。
func spaHandler(sub fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		// 存在则交 FileServer，不存在回落到 index.html（SPA 前端路由）。
		if _, err := fs.Stat(sub, p); err != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
