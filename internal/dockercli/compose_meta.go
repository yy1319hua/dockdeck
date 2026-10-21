// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// compose 元信息读取：从 compose 文件里取出 compose 自己认的项目名。
//
// 为什么必须读文件：compose 的项目名优先级是「文件顶层 name: > 目录名」，
// 而面板展示用的名字是「目录名-角色后缀」。两者不一致时，面板看到的 A 与
// compose 实际操作的对象 B 不是一回事——在备份副本上点「停止」停掉线上项目，
// 就是这么来的。只看目录名永远发现不了这件事。

package dockercli

import (
	"os"

	"gopkg.in/yaml.v3"
)

// composeNameFromFile 读取 compose 文件顶层的 name: 字段。
//
// 返回值语义：
//   - 有 name: 且非空 → 该值（compose 自己认的项目名）
//   - 文件读不了 / YAML 解析失败 / 没有 name: → 空串，调用方自行回落
//
// 只取顶层字段：compose 文件顶层是映射，services 等子键不影响 name。
// 用 yaml.Node 解析而非 map[string]string——后者遇到 services 下的复杂结构
// 会整体报错（[]any / map[string]any 装不进 string）。
//
// 解析失败不视为错误：compose 文件可能正在被编辑到一半，或用了 YAML 锚点等
// 特性。此时退回目录名即可，不该让整个列举失败。
func composeNameFromFile(file string) string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	// 文档节点下第 0 个是 DocumentContent，再下才是映射本体。
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return ""
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		val := root.Content[i+1]
		if key.Value != "name" {
			continue
		}
		if val.Kind != yaml.ScalarNode {
			return ""
		}
		return val.Value
	}
	return ""
}
