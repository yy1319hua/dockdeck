// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.2：Compose 客户端的单元测试。策略是「真实文件系统 + 假 executor」：
// discover 走真实 os 递归（验证白名单目录扫描、忽略非 compose 文件、嵌套
// 发现），exec 执行用 fakeComposeRunner 注入（验证 ps 状态解析、写动作参数、
// logs 流）。这样既能测真实的目录遍历行为，又不必在单测里真装 docker。

package dockercli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeComposeRunner 记录每次调用参数，并按精确参数串返回预置响应。
type fakeComposeRunner struct {
	resp map[string]string
	got  [][]string
}

func (f *fakeComposeRunner) Captured(ctx context.Context, args ...string) ([]byte, error) {
	f.got = append(f.got, args)
	return []byte(f.resp[strings.Join(args, " ")]), nil
}

func (f *fakeComposeRunner) Streamed(ctx context.Context, args ...string) (io.ReadCloser, error) {
	f.got = append(f.got, args)
	return io.NopCloser(strings.NewReader(f.resp[strings.Join(args, " ")])), nil
}

func (f *fakeComposeRunner) calledWith(want ...string) bool {
	for _, g := range f.got {
		if strings.Join(g, " ") == strings.Join(want, " ") {
			return true
		}
	}
	return false
}

// writeCompose 在 base 下造一个 compose 项目（dir/docker-compose.yml）。
func writeCompose(t *testing.T, base, dir string) string {
	t.Helper()
	d := filepath.Join(base, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "docker-compose.yml")
	if err := os.WriteFile(p, []byte("services:\n  x:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestComposeListProjects(t *testing.T) {
	base := t.TempDir()
	p1 := writeCompose(t, base, "myproj")
	p2 := writeCompose(t, base, "nested/deep/myproj2")
	// 非 compose 文件必须被忽略
	if err := os.WriteFile(filepath.Join(base, "notes.yml"), []byte("x: 1"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " ps --format json": `[{"Service":"web","State":"running","Image":"nginx:1.27"}]`,
		"-f " + p2 + " ps --format json": `[{"Service":"db","State":"exited","Image":"postgres:16"},{"Service":"cache","State":"running","Image":"redis:7"}]`,
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}

	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("发现 %d 个项目，期望 2（notes.yml 应被忽略），got %+v", len(projects), projects)
	}
	byName := map[string]protocolProjectStatus{}
	for _, p := range projects {
		byName[p.Name] = protocolProjectStatus{p.Status, len(p.Services)}
	}
	if s := byName["myproj"]; s.status != "running" || s.svcs != 1 {
		t.Errorf("myproj 应 running/1 服务，得 %+v", s)
	}
	// myproj2 一个 exited 一个 running → partial
	if s := byName["myproj2"]; s.status != "partial" || s.svcs != 2 {
		t.Errorf("myproj2 应 partial/2 服务，得 %+v", s)
	}
}

// protocolProjectStatus 只是测试内的小载体，避免再 import protocol 名冲突。
type protocolProjectStatus struct {
	status string
	svcs   int
}

func TestComposeStatusStoppedAndUnknown(t *testing.T) {
	base := t.TempDir()
	p1 := writeCompose(t, base, "stoppedproj")
	p2 := writeCompose(t, base, "brokenproj")

	runner := &fakeComposeRunner{resp: map[string]string{
		// 无容器：ps 返回空数组 → stopped
		"-f " + p1 + " ps --format json": `[]`,
		// 解析失败（非法 JSON）→ unknown（不阻断列举）
		"-f " + p2 + " ps --format json": `not-json`,
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, p := range projects {
		byName[p.Name] = p.Status
	}
	if byName["stoppedproj"] != "stopped" {
		t.Errorf("stoppedproj 应 stopped，得 %q", byName["stoppedproj"])
	}
	if byName["brokenproj"] != "unknown" {
		t.Errorf("brokenproj 应 unknown（解析失败），得 %q", byName["brokenproj"])
	}
}

func TestComposeResolveProjectWhitelist(t *testing.T) {
	base := t.TempDir()
	p1 := writeCompose(t, base, "real")
	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " ps --format json": `[{"Service":"web","State":"running","Image":"nginx"}]`,
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}

	if _, ok := c.ResolveProject(context.Background(), "real"); !ok {
		t.Error("real 应可解析")
	}
	// 白名单外的名字（含穿越尝试）一律 false
	for _, name := range []string{"ghost", "../escape", "/etc/passwd", "real/../other"} {
		if _, ok := c.ResolveProject(context.Background(), name); ok {
			t.Errorf("resolveProject(%q) 不应解析成功（越出白名单）", name)
		}
	}
}

func TestComposeActionArgs(t *testing.T) {
	base := t.TempDir()
	p1 := writeCompose(t, base, "p")
	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " ps --format json": `[]`,
		"-f " + p1 + " up -d":            "Container p-web-1 Started",
		"-f " + p1 + " down":             "Container p-web-1 Removed",
		"-f " + p1 + " restart":          "Container p-web-1 Restarted",
		"-f " + p1 + " pull":             "Pulling web ... done",
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}
	ctx := context.Background()

	if out, err := c.Up(ctx, p1); err != nil || out != "Container p-web-1 Started" {
		t.Errorf("Up = (%q,%v)，期望 Started/nil", out, err)
	}
	if !runner.calledWith("-f", p1, "up", "-d") {
		t.Errorf("Up 应带 -d（后台启动），实际参数 %v", runner.got)
	}
	if out, err := c.Down(ctx, p1); err != nil || out != "Container p-web-1 Removed" {
		t.Errorf("Down = (%q,%v)", out, err)
	}
	if _, err := c.Restart(ctx, p1); err != nil {
		t.Errorf("Restart err = %v", err)
	}
	if _, err := c.Pull(ctx, p1); err != nil {
		t.Errorf("Pull err = %v", err)
	}
}

func TestComposeLogsStream(t *testing.T) {
	base := t.TempDir()
	p1 := writeCompose(t, base, "p")
	runner := &fakeComposeRunner{resp: map[string]string{
		"-f " + p1 + " logs --no-color": "line1\nline2\n",
	}}
	c := &ComposeClient{dirs: []string{base}, runner: runner}
	rc, err := c.Logs(context.Background(), p1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, rc); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "line1\nline2\n" {
		t.Errorf("Logs 流内容 = %q", buf.String())
	}
	if !runner.calledWith("-f", p1, "logs", "--no-color") {
		t.Errorf("Logs 非 follow 不应带 --follow，实际 %v", runner.got)
	}
}
