// Package version 承载构建时由 -ldflags 注入的构建元数据。
//
// go build 对 -X 指向的「不存在」符号静默忽略（不报错），所以本包必须真实
// 存在这三个导出变量，否则版本注入为空操作。默认值给个占位，覆盖本地未注入
// 构建的场景（制品才由 Makefile 注入真值）。
package version

// Version 语义化版本或 commit（Makefile: git describe --tags --always --dirty，无标签回退 dev）。
var Version = "dev"

// Commit 短 commit。
var Commit = "unknown"

// Date UTC 构建时间（RFC3339）。
var Date = "unknown"
