// Package assets 内嵌前端构建产物，供 Hub 的 internal/assets 引用。
//
// 为什么 embed 声明放在这里，而不是 internal/assets：
//
//	go:embed 的 pattern 只能引用「当前包目录树内」的路径——不允许 ..，
//	也不允许跨到别的包目录。产物落在 web/dist，所以声明必须与产物同处一棵树。
//
// 目录本身必须存在，否则编译失败。这正是 GAP-8 修复的意义所在：
// web/dist/.gitkeep 已入库，fresh clone 也有这个目录，go build 不会因为
// 「前端还没构建过」就失败。目录里只有 .gitkeep 时（尚未跑 npm run build）
// 依然可编译，运行期由 internal/assets 检测不到 index.html 后回退占位页。
package assets

import "embed"

//go:embed all:dist
var Dist embed.FS
