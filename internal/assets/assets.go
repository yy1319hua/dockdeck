// Package assets 提供 Hub 的静态资源服务。
//
// S1 阶段前端尚未构建，这里内嵌一个占位页，保证 Hub 可独立编译运行与 API 联调。
// S5 完成后，assets 将改为内嵌 web/dist 的真实构建产物（见 PROGRESS.md S5.5）。
package assets

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:static
var staticFS embed.FS

// Handler 返回 SPA 静态资源处理器：命中文件直接返回，未命中回 index.html。
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// 内嵌目录缺失属构建期问题，运行期以 500 兜底而非 panic。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "assets unavailable", http.StatusInternalServerError)
		})
	}
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
