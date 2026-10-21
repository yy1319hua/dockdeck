// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// v0.2.7：compose 项目发现与自保护的单元测试。
//
// 覆盖三件事：
//  1. compose 文件名识别——基础名 + 带角色后缀（docker-compose.agent.yml）；
//  2. 项目命名——同目录多份 compose 各自成项目，重名再消解（-2 后缀）；
//  3. 自保护——down/restart 命中承载 Agent 自身的 compose 项目时被拒。

package dockercli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeSuffixOf(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
		ok     bool
	}{
		{"docker-compose.yml", "", true},
		{"docker-compose.yaml", "", true},
		{"compose.yml", "", true},
		{"compose.yaml", "", true},
		{"docker-compose.agent.yml", "agent", true},
		{"docker-compose.hub.yaml", "hub", true},
		{"compose.agent.yml", "agent", true},
		// override 是叠加层不是独立项目，不认。
		{"docker-compose.override.yml", "", false},
		{"compose.override.yaml", "", false},
		// 备份文件不以 .yml/.yaml 结尾，天然不认。
		{"docker-compose.yml.bak.2026-01-02T15:04:05.000000000Z07:00", "", false},
		// 无关文件不认。
		{"notes.yml", "", false},
		{"docker-compose.yml.txt", "", false},
	}
	for _, c := range cases {
		suffix, ok := composeSuffixOf(c.name)
		if ok != c.ok || suffix != c.suffix {
			t.Errorf("composeSuffixOf(%q) = (%q,%v)，期望 (%q,%v)", c.name, suffix, ok, c.suffix, c.ok)
		}
	}
}

// writeComposeFile 在 dir 下写一个指定文件名的 compose 文件。
func writeComposeFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("services:\n  x:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 同目录两份 compose 文件必须各自成项目，且名字可区分。
func TestComposeSameDirTwoFiles(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "dockdeck")
	p1 := writeComposeFile(t, dir, "docker-compose.yml")
	p2 := writeComposeFile(t, dir, "docker-compose.agent.yml")
	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " ps --format json": `[{"Service":"hub","State":"running","Image":"hub:1"}]`,
		"-f " + p2 + " ps --format json": `[{"Service":"agent","State":"running","Image":"agent:1"}]`,
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("应发现 2 个项目（agent 那份不再被跳过），得 %d：%+v", len(projects), projects)
	}
	names := map[string]bool{}
	for _, p := range projects {
		names[p.Name] = true
	}
	if !names["dockdeck"] || !names["dockdeck-agent"] {
		t.Errorf("项目名应含 dockdeck 与 dockdeck-agent，得 %v", names)
	}
	// 两个项目各自能按名解析到（早前同目录两份只有前者可达）。
	for _, want := range []string{"dockdeck", "dockdeck-agent"} {
		if _, ok := c.ResolveProject(context.Background(), want); !ok {
			t.Errorf("项目 %q 应可解析", want)
		}
	}
}

// 跨目录同名项目：后者加 -2 序号，保证都能解析到。
func TestComposeDuplicateNamesDedup(t *testing.T) {
	base := t.TempDir()
	p1 := writeComposeFile(t, filepath.Join(base, "a", "same"), "docker-compose.yml")
	p2 := writeComposeFile(t, filepath.Join(base, "b", "same"), "docker-compose.yml")
	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " ps --format json": `[]`,
		"-f " + p2 + " ps --format json": `[]`,
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("应发现 2 个项目，得 %d", len(projects))
	}
	if projects[0].Name != "same" || projects[1].Name != "same-2" {
		t.Errorf("重名消解应得 same / same-2，得 %q / %q", projects[0].Name, projects[1].Name)
	}
	for _, want := range []string{"same", "same-2"} {
		if _, ok := c.ResolveProject(context.Background(), want); !ok {
			t.Errorf("重名消解后项目 %q 应可解析", want)
		}
	}
}

// 自保护：down/restart 命中含自身容器的项目被拒；up/pull 不拦。
func TestComposeSelfProtection(t *testing.T) {
	base := t.TempDir()
	p1 := writeComposeFile(t, filepath.Join(base, "selfhost"), "docker-compose.yml")
	psJSON := `[{"Service":"agent","State":"running","Image":"agent:1","Name":"dockdeck-agent","ID":"a71af3ab9cf1"}]`
	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " ps --format json": psJSON,
		"-f " + p1 + " up -d":            "started",
		"-f " + p1 + " down":             "removed",
		"-f " + p1 + " restart":          "restarted",
		"-f " + p1 + " pull":             "pulled",
	}}
	c := NewComposeClientWithSelf([]string{base}, "dockdeck-agent")
	c.runner = runner // 注入假执行器：只验判定逻辑，不碰真 docker
	ctx := context.Background()

	_, err := c.Down(ctx, p1)
	if err == nil {
		t.Fatal("down 命中自身容器应被拒")
	}
	if !strings.Contains(err.Error(), "自身容器") {
		t.Errorf("错误信息应说明自身容器原因，得 %q", err.Error())
	}
	if runner.calledWith("-f", p1, "down") {
		t.Error("被拒的 down 不应真的执行")
	}
	if _, err := c.Restart(ctx, p1); err == nil {
		t.Error("restart 命中自身容器应被拒")
	}
	// up / pull 不停容器，不拦。
	if _, err := c.Up(ctx, p1); err != nil {
		t.Errorf("up 不应被自保护拦下：%v", err)
	}
	if _, err := c.Pull(ctx, p1); err != nil {
		t.Errorf("pull 不应被自保护拦下：%v", err)
	}
}

// 未配自身标识时不做自保护（与容器模块口径一致）。
func TestComposeSelfProtectionDisabled(t *testing.T) {
	base := t.TempDir()
	p1 := writeComposeFile(t, filepath.Join(base, "selfhost"), "docker-compose.yml")
	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " ps --format json": `[{"Service":"agent","State":"running","Name":"dockdeck-agent"}]`,
		"-f " + p1 + " down":             "removed",
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}
	if _, err := c.Down(context.Background(), p1); err != nil {
		t.Errorf("未配自身标识时不应拦 down：%v", err)
	}
}

// 容器身份取自 ps 输出的 Name；老版本只给 Names 时取首段。
func TestComposeServiceContainerIdentity(t *testing.T) {
	base := t.TempDir()
	p1 := writeComposeFile(t, filepath.Join(base, "idtest"), "docker-compose.yml")
	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " ps --format json": `[{"Service":"a","State":"running","Image":"i","Names":"proj-a-1,proj-a-2"}]`,
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || len(projects[0].Services) != 1 {
		t.Fatalf("应得 1 项目 1 服务：%+v", projects)
	}
	if got := projects[0].Services[0].Container; got != "proj-a-1" {
		t.Errorf("容器名应取 Names 首段 proj-a-1，得 %q", got)
	}
}
