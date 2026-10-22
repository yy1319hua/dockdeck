// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.2：Compose 端点(Agent 侧)的单元测试。用假 ComposeOperator 注入，
// 验证路由、动作白名单、白名单项目解析(404 拦截穿越)、logs 流，不触碰真实
// `docker compose`。鉴权与服务器装配复用 containers_test.go 的 hGET/hDo/signFor。

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// fakeComposeOp 实现 ComposeOperator，按文件|动作键返回预置输出。
type fakeComposeOp struct {
	projects []protocol.ComposeProject
	listErr  error
	outputs  map[string]string // key: "<configfile>|<action>"
	logsData string
	logsErr  error
	// called 记录动作分发，验证 up/down/restart/pull 各走对应方法。
	called []string

	// S4.3 文件读写相关。
	fileContent string // ReadFile 返回的当前内容
	fileErr     error  // ReadFile/Diff/WriteFile 的统一错误注入
	diffOut     string // Diff 返回的 diff 文本
	writeBackup string // WriteFile 返回的备份路径
	// S7.11 备份端点相关。
	backups       []protocol.ComposeBackup // ListBackups 返回的清单
	backupsErr    error                    // ListBackups 错误注入
	backupContent map[string]string        // ReadBackup 按名字返回正文
	backupErr     error                    // ReadBackup/RestoreBackup 错误注入
	restoreNew    string                   // RestoreBackup 返回的「恢复前新备份文件名」
	lastBackup    string                   // 记录最近一次读/恢复用的备份名
	writeErr      error                    // WriteFile 错误注入
	wrote         []string                 // 记录写入调用，验证 file|content 透传
}

func (f *fakeComposeOp) ListProjects(ctx context.Context) ([]protocol.ComposeProject, error) {
	return f.projects, f.listErr
}

func (f *fakeComposeOp) ResolveProject(ctx context.Context, name string) (protocol.ComposeProject, bool) {
	for _, p := range f.projects {
		if p.Name == name {
			return p, true
		}
	}
	return protocol.ComposeProject{}, false
}

func (f *fakeComposeOp) Up(ctx context.Context, file string) (string, error) {
	f.called = append(f.called, "up")
	return f.outputs[file+"|up"], nil
}
func (f *fakeComposeOp) Down(ctx context.Context, file string) (string, error) {
	f.called = append(f.called, "down")
	return f.outputs[file+"|down"], nil
}
func (f *fakeComposeOp) Restart(ctx context.Context, file string) (string, error) {
	f.called = append(f.called, "restart")
	return f.outputs[file+"|restart"], nil
}
func (f *fakeComposeOp) Pull(ctx context.Context, file string) (string, error) {
	f.called = append(f.called, "pull")
	return f.outputs[file+"|pull"], nil
}
func (f *fakeComposeOp) Logs(ctx context.Context, file string, follow bool) (io.ReadCloser, error) {
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	return io.NopCloser(strings.NewReader(f.logsData)), nil
}

func (f *fakeComposeOp) ReadFile(ctx context.Context, file string) (string, error) {
	return f.fileContent, f.fileErr
}
func (f *fakeComposeOp) Diff(ctx context.Context, file string, content string) (string, error) {
	return f.diffOut, f.fileErr
}
func (f *fakeComposeOp) WriteFile(ctx context.Context, file string, content string) (string, error) {
	f.wrote = append(f.wrote, file+"|"+content)
	return f.writeBackup, f.writeErr
}

// S7.11：备份三件套的假实现。backups 是预置清单；backupContent 按名字返回正文
// （空 map 时返回空串）；restoreNew 是 RestoreBackup 返回的「恢复前新备份文件名」。
func (f *fakeComposeOp) ListBackups(ctx context.Context, file string) ([]protocol.ComposeBackup, error) {
	if f.backupsErr != nil {
		return nil, f.backupsErr
	}
	return f.backups, nil
}

func (f *fakeComposeOp) ReadBackup(ctx context.Context, file, name string) (string, error) {
	if f.backupErr != nil {
		return "", f.backupErr
	}
	f.lastBackup = name
	return f.backupContent[name], nil
}

func (f *fakeComposeOp) RestoreBackup(ctx context.Context, file, name string) (string, error) {
	if f.backupErr != nil {
		return "", f.backupErr
	}
	f.lastBackup = name
	return f.restoreNew, nil
}

func agentWithCompose(t *testing.T, op dockercli.ComposeOperator) *httptest.Server {
	t.Helper()
	return httptest.NewServer(buildHandler(smokeToken, smokeCollector(t), nil, newContainersAPI("", "", nil, nil, nil), newImagesAPI(""), &composeAPI{op: op}, nil, newResourcesAPI("")))
}

func TestComposeAgentList(t *testing.T) {
	op := &fakeComposeOp{projects: []protocol.ComposeProject{
		{Name: "web", Path: "/opt/web", ConfigFile: "/opt/web/docker-compose.yml", Status: "running", Services: []protocol.ComposeService{{Name: "web", State: "running"}}},
		{Name: "db", Path: "/opt/db", ConfigFile: "/opt/db/docker-compose.yml", Status: "stopped"},
	}}
	srv := agentWithCompose(t, op)
	defer srv.Close()

	resp, body := hGET(t, srv, "/api/v1/compose")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list 状态 %d", resp.StatusCode)
	}
	var got []protocol.ComposeProject
	if err := json.Unmarshal(dataOf(t, body), &got); err != nil {
		t.Fatalf("解码 list: %v (body=%s)", err, body)
	}
	if len(got) != 2 || got[0].Name != "web" || got[1].Name != "db" {
		t.Errorf("list 返回 %+v", got)
	}
}

func TestComposeAgentActionBadAndUnknown(t *testing.T) {
	op := &fakeComposeOp{projects: []protocol.ComposeProject{
		{Name: "web", ConfigFile: "/opt/web/docker-compose.yml"},
	}}
	srv := agentWithCompose(t, op)
	defer srv.Close()

	// 白名单外的动作（如 rm）→ 400，绝不透传给 shell。
	resp, _ := hDo(t, srv, http.MethodPost, "/api/v1/compose/web/rm", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法动作应 400，得 %d", resp.StatusCode)
	}
	// 不在白名单内的项目名（含穿越尝试）→ 404。
	for _, name := range []string{"ghost", "../etc"} {
		resp, _ := hDo(t, srv, http.MethodPost, "/api/v1/compose/"+name+"/up", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("项目 %q 应 404，得 %d", name, resp.StatusCode)
		}
	}
}

func TestComposeAgentActionForwards(t *testing.T) {
	cfg := "/opt/web/docker-compose.yml"
	op := &fakeComposeOp{
		projects: []protocol.ComposeProject{{Name: "web", ConfigFile: cfg}},
		outputs: map[string]string{
			cfg + "|up":      "Container web-1 Started",
			cfg + "|down":    "Container web-1 Removed",
			cfg + "|restart": "Container web-1 Restarted",
			cfg + "|pull":    "Pulling web ... done",
		},
	}
	srv := agentWithCompose(t, op)
	defer srv.Close()

	cases := []struct {
		action, want string
	}{
		{"up", "Container web-1 Started"},
		{"down", "Container web-1 Removed"},
		{"restart", "Container web-1 Restarted"},
		{"pull", "Pulling web ... done"},
	}
	for _, c := range cases {
		resp, body := hDo(t, srv, http.MethodPost, "/api/v1/compose/web/"+c.action, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态 %d body=%s", c.action, resp.StatusCode, body)
		}
		var r protocol.ComposeActionResult
		if err := json.Unmarshal(dataOf(t, body), &r); err != nil {
			t.Fatalf("%s 解码: %v", c.action, err)
		}
		if r.Output != c.want {
			t.Errorf("%s 输出 = %q，期望 %q", c.action, r.Output, c.want)
		}
	}
	// 四个动作各被调用一次，证明分发正确。
	if len(op.called) != 4 {
		t.Errorf("动作调用次数 = %d，期望 4（%v）", len(op.called), op.called)
	}
}

func TestComposeAgentLogs(t *testing.T) {
	op := &fakeComposeOp{
		projects: []protocol.ComposeProject{{Name: "web", ConfigFile: "/opt/web/docker-compose.yml"}},
		logsData: "web-1  | hello\nweb-1  | world\n",
	}
	srv := agentWithCompose(t, op)
	defer srv.Close()

	resp, body := hGET(t, srv, "/api/v1/compose/web/logs")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs 状态 %d", resp.StatusCode)
	}
	if string(body) != op.logsData {
		t.Errorf("logs 流 = %q，期望 %q", body, op.logsData)
	}
}

// ---- S4.3：Compose YAML 文件读写三端点 ----

func TestComposeAgentFileGet(t *testing.T) {
	op := &fakeComposeOp{
		projects:    []protocol.ComposeProject{{Name: "web", ConfigFile: "/opt/web/docker-compose.yml"}},
		fileContent: "services:\n  web:\n",
	}
	srv := agentWithCompose(t, op)
	defer srv.Close()

	resp, body := hGET(t, srv, "/api/v1/compose/web/file")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("file get 状态 %d body=%s", resp.StatusCode, body)
	}
	var got protocol.ComposeFile
	if err := json.Unmarshal(dataOf(t, body), &got); err != nil {
		t.Fatalf("解码 file: %v body=%s", err, body)
	}
	if got.ConfigFile != "/opt/web/docker-compose.yml" || got.Content != "services:\n  web:\n" {
		t.Errorf("file get 返回 %+v", got)
	}

	// 白名单外/未知项目 → 404（不读文件）。
	if r2, _ := hGET(t, srv, "/api/v1/compose/evil/file"); r2.StatusCode != http.StatusNotFound {
		t.Errorf("未知项目 file get 应 404, got %d", r2.StatusCode)
	}
}

func TestComposeAgentFileDiff(t *testing.T) {
	op := &fakeComposeOp{
		projects: []protocol.ComposeProject{{Name: "web", ConfigFile: "/opt/web/docker-compose.yml"}},
		diffOut:  "@@ -1,1 +1,1 @@\n-a\n+b\n",
	}
	srv := agentWithCompose(t, op)
	defer srv.Close()

	resp, body := hDo(t, srv, http.MethodPost, "/api/v1/compose/web/diff", `{"content":"b\n"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("diff 状态 %d body=%s", resp.StatusCode, body)
	}
	var got protocol.ComposeFileDiff
	if err := json.Unmarshal(dataOf(t, body), &got); err != nil {
		t.Fatalf("解码 diff: %v", err)
	}
	if got.Diff != "@@ -1,1 +1,1 @@\n-a\n+b\n" {
		t.Errorf("diff 返回 %q", got.Diff)
	}

	// 未知项目 → 404（在解 body 之前拦下，不浪费读）。
	if r2, _ := hDo(t, srv, http.MethodPost, "/api/v1/compose/evil/diff", `{"content":"b\n"}`); r2.StatusCode != http.StatusNotFound {
		t.Errorf("未知项目 diff 应 404, got %d", r2.StatusCode)
	}
	// 已知项目 + 未知字段 body → 400（DisallowUnknownFields）。
	if r3, _ := hDo(t, srv, http.MethodPost, "/api/v1/compose/web/diff", `{"bogus":1}`); r3.StatusCode != http.StatusBadRequest {
		t.Errorf("坏 body diff 应 400, got %d", r3.StatusCode)
	}
}

func TestComposeAgentFilePut(t *testing.T) {
	op := &fakeComposeOp{
		projects:    []protocol.ComposeProject{{Name: "web", ConfigFile: "/opt/web/docker-compose.yml"}},
		writeBackup: "/opt/web/docker-compose.yml.bak.2026-10-15T00:00:00+08:00",
	}
	srv := agentWithCompose(t, op)
	defer srv.Close()

	const next = "services:\n  web:\n    image: nginx:1.27\n"
	resp, body := hDo(t, srv, http.MethodPut, "/api/v1/compose/web/file", `{"content":"`+`services:\n  web:\n    image: nginx:1.27\n`+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put 状态 %d body=%s", resp.StatusCode, body)
	}
	var got protocol.ComposeFileWritten
	if err := json.Unmarshal(dataOf(t, body), &got); err != nil {
		t.Fatalf("解码 put: %v", err)
	}
	if got.BackupFile != "/opt/web/docker-compose.yml.bak.2026-10-15T00:00:00+08:00" {
		t.Errorf("put 返回备份 %q", got.BackupFile)
	}
	// op 收到白名单路径 + 原文透传。
	want := "/opt/web/docker-compose.yml|" + next
	if len(op.wrote) != 1 || op.wrote[0] != want {
		t.Errorf("wrote 记录不符: %#v", op.wrote)
	}

	// 未知项目 → 404，且不触发写入。
	before := len(op.wrote)
	if r2, _ := hDo(t, srv, http.MethodPut, "/api/v1/compose/evil/file", `{"content":"x"}`); r2.StatusCode != http.StatusNotFound {
		t.Errorf("未知项目 put 应 404, got %d", r2.StatusCode)
	}
	if len(op.wrote) != before {
		t.Errorf("未知项目不应触发写入")
	}
}
