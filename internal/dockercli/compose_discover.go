// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S：Compose 目录的第二发现来源——从容器 label 反推。
//
// 由来：运维手填白名单总是不准（填窄了看不到，填宽了引入备份副本等噪音），
// 而 Docker 自己把答案写在容器 label 里：
//
//	com.docker.compose.project            → compose 项目名
//	com.docker.compose.project.working_dir → 项目工作目录（宿主路径）
//	com.docker.compose.project.config_files→ 项目 compose 文件（宿主路径）
//
// 这些 label 由 compose CLI 在创建容器时写入，指向的是**宿主**路径。
// Agent 若以同路径 bind 挂载（dockdeck 的既有约定），容器内同路径即可见，
// 可直接扫描；不是同路径时容器内该路径不存在，扫描自然扫不到——
// 不报错、不猜测，扫不到就是扫不到。

package dockercli

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// compose label 的键名。集中定义，避免散落各处硬编码字符串。
const (
	labelComposeProject = "com.docker.compose.project"
	labelComposeWorkDir = "com.docker.compose.project.working_dir"
	labelComposeConfigs = "com.docker.compose.project.config_files"
)

// ComposeDirsFromLabels 从容器清单的 compose label 反推宿主 compose 目录。
//
// 只认 running 的容器：已停止容器的 label 仍指向旧目录，纳入后会把早已
// 废弃的部署目录重新拉回清单——那是噪音，不是事实。
//
// 只认绝对路径：label 正常情况下总是绝对路径，出现相对路径说明异常，
// 宁可漏掉也不去解析它（相对路径的解释权不该在这里）。
//
// 返回值按路径去重后升序返回，保证输出稳定（同样输入必得同样结果，
// 否则前端列表顺序会随容器遍历顺序抖动）。
func ComposeDirsFromLabels(containers []protocol.ContainerSummary) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		clean := filepath.Clean(p)
		if seen[clean] {
			return
		}
		seen[clean] = true
		out = append(out, clean)
	}
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		if len(c.Labels) == 0 {
			continue
		}
		// 没有 project label 的容器不是 compose 创建的（docker run 起的），
		// 它的工作目录与 compose 项目无关，不参与反推。
		if c.Labels[labelComposeProject] == "" {
			continue
		}
		add(c.Labels[labelComposeWorkDir])
		// config_files 是逗号分隔的文件列表（含角色后缀的文件名，如
		// docker-compose.agent.yml）。取其目录部分——工作目录缺失时，
		// 配置文件所在目录就是项目目录。
		for _, f := range strings.Split(c.Labels[labelComposeConfigs], ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if !filepath.IsAbs(f) {
				continue
			}
			add(filepath.Dir(f))
		}
	}
	sort.Strings(out)
	return out
}
