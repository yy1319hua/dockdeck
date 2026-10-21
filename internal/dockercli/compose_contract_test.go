// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 生产实况契约测试：把生产宿主机上真实存在的 compose 文件名与目录固化进单测。
//
// 背景：v0.2.7 之前 compose 发现只认四个精确文件名，agent 那份
// docker-compose.agent.yml 永远被跳过——面板里根本看不到承载自己的项目。
// 本测试用生产真实文件名断言「文件名 → 角色后缀 → 项目名」的映射，
// 防止将来有人收窄白名单时又把 agent 那份漏掉（这类回归只能靠契约测试兜住：
// 单元测试里的假设名一旦变了，生产表现与测试都「正常」，问题却已复现）。

package dockercli

import (
	"path/filepath"
	"testing"
)

// TestProductionComposeNames 用生产真实文件名断言发现结果与项目名。
//
// 生产白名单目录与文件（2026-10-21 实测）：
//
//	komari/docker-compose.yml            → komari
//	watchtower/docker-compose.yml        → watchtower
//	home/ai/tunnel/docker-compose.yml         → tunnel
//	home/ai/dockdeck/docker-compose.yml       → dockdeck
//	home/ai/dockdeck/docker-compose.agent.yml → dockdeck-agent
//
// 前四个项目名与 v0.2.6 之前完全一致（零回归），第五个是新增可见的 agent 项目。
func TestProductionComposeNames(t *testing.T) {
	cases := []struct {
		dir      string
		file     string
		wantName string
	}{
		{"opt/komari", "docker-compose.yml", "komari"},
		{"opt/watchtower", "docker-compose.yml", "watchtower"},
		{"home/ai/tunnel", "docker-compose.yml", "tunnel"},
		{"home/ai/dockdeck", "docker-compose.yml", "dockdeck"},
		{"home/ai/dockdeck", "docker-compose.agent.yml", "dockdeck-agent"},
	}
	for _, c := range cases {
		full := filepath.Join(c.dir, c.file)
		suffix, ok := composeSuffixOf(c.file)
		if !ok {
			t.Errorf("%s 应被识别为 compose 入口文件", full)
			continue
		}
		if got := projectNameFor(c.dir, suffix); got != c.wantName {
			t.Errorf("%s 项目名 = %q，期望 %q", full, got, c.wantName)
		}
	}
}

// TestProductionNonComposeFiles 断言生产目录里的非 compose 文件不被误当项目。
func TestProductionNonComposeFiles(t *testing.T) {
	for _, name := range []string{
		"OPS-<host>.md",
		"README.md",
		"backup.sh",
		"restore.sh",
		"docker-compose.yml.bak.2026-10-21T10:24:44.123456789+08:00",
		"docker-compose.agent.yml.bak.2026-10-21T10:25:11.987654321+08:00",
	} {
		if _, ok := composeSuffixOf(name); ok {
			t.Errorf("%q 不应被识别为 compose 入口文件", name)
		}
	}
}
