// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.2：Compose 客户端——宿主侧 `docker compose` v2 CLI 的薄封装。
//
// 与镜像/容器不同，Compose 没有 daemon API，全部走 CLI。因此这里不走
// Engine socket（Client.do），而是 exec `docker compose`。为可测，执行被
// 收进 composeRunner 接口：Captured 捕获 stdout+stderr 合流（ps/up/down/...
// 这类要拿输出或判成败的），Streamed 返回 stdout 管道（logs --follow）。
//
// 白名单是安全边界：dirs 是运维显式配置的目录，discover 只在这些目录内
// 递归找 compose 文件；resolveProject 把「项目名」映射到发现到的真实路径，
// 调用方永远拿不到白名单外的路径去 exec——路径穿越在入口就被挡掉。

package dockercli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// composeRunner 抽象 `docker compose` 的执行，测试可注入假实现。
type composeRunner interface {
	// Captured 执行并捕获 stdout+stderr 合流，命令退出即返回。
	Captured(ctx context.Context, args ...string) ([]byte, error)
	// Streamed 启动命令并返回 stdout 管道（调用方读至 EOF）。
	Streamed(ctx context.Context, args ...string) (io.ReadCloser, error)
}

// execComposeRunner 是生产用的默认实现，直接 exec `docker compose`。
type execComposeRunner struct{}

func (execComposeRunner) Captured(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

func (execComposeRunner) Streamed(ctx context.Context, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return stdout, nil
}

// ComposeOperator 是 Agent 端点依赖的抽象——真实实现是 ComposeClient（exec
// `docker compose`），单测里换成假实现。把接口定义在 dockercli 让两侧共用，
// Agent 不感知 exec 细节，只认这组方法。
type ComposeOperator interface {
	ListProjects(ctx context.Context) ([]protocol.ComposeProject, error)
	ResolveProject(ctx context.Context, name string) (protocol.ComposeProject, bool)
	Up(ctx context.Context, file string) (string, error)
	Down(ctx context.Context, file string) (string, error)
	Restart(ctx context.Context, file string) (string, error)
	Pull(ctx context.Context, file string) (string, error)
	Logs(ctx context.Context, file string, follow bool) (io.ReadCloser, error)
}

// ComposeClient 操作白名单目录内的 compose 项目。
type ComposeClient struct {
	dirs   []string
	runner composeRunner
}

// NewComposeClient 用配置好的白名单目录装配客户端。dirs 为空则 list 永远
// 返回空（相当于 Compose 功能未启用）。
func NewComposeClient(dirs []string) *ComposeClient {
	return &ComposeClient{dirs: dirs, runner: execComposeRunner{}}
}

// composeFileNames 是被识别为项目入口的文件名（大小写敏感，避免误吞无关 yml）。
var composeFileNames = map[string]bool{
	"docker-compose.yml":  true,
	"docker-compose.yaml": true,
	"compose.yml":         true,
	"compose.yaml":        true,
}

// discover 在白名单目录内递归找 compose 文件，返回绝对路径切片（已去重）。
func (c *ComposeClient) discover() ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	for _, dir := range c.dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// 单个目录不可读（权限/不存在）不应拖垮整次列举。
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if composeFileNames[d.Name()] {
				ap, aerr := filepath.Abs(path)
				if aerr == nil && !seen[ap] {
					seen[ap] = true
					out = append(out, ap)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// projectFromFile 由 compose 文件路径推出项目元信息。
func projectFromFile(file string) protocol.ComposeProject {
	dir := filepath.Dir(file)
	return protocol.ComposeProject{
		Name:       filepath.Base(dir),
		Path:       dir,
		ConfigFile: file,
		Status:     "unknown",
	}
}

// composePsRow 是 `docker compose ps --format json` 输出的一行（按字段名解析，
// 对大小写不敏感由 encoding/json 的字段匹配兜底）。
type composePsRow struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Image   string `json:"Image"`
}

// status 取单个项目的运行状态。解析失败时调用方把项目标为 unknown，
// 不阻断整个列举。
func (c *ComposeClient) status(ctx context.Context, file string) (protocol.ComposeProject, error) {
	raw, err := c.runner.Captured(ctx, "-f", file, "ps", "--format", "json")
	if err != nil {
		return protocol.ComposeProject{}, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		// 无输出等同无容器 → stopped（避免把「命令成功但无内容」误判为 unknown）。
		proj := projectFromFile(file)
		proj.Status = "stopped"
		return proj, nil
	}
	var rows []composePsRow
	// v2 的 --format json 输出一个 JSON 数组（单行）；若解析失败再逐行尝试。
	if jerr := json.Unmarshal(raw, &rows); jerr != nil {
		rows = nil
		for _, line := range strings.Split(trimmed, "\n") {
			var r composePsRow
			if err2 := json.Unmarshal([]byte(line), &r); err2 == nil {
				rows = append(rows, r)
			}
		}
		// 数组解析失败 且 逐行也零命中（如 "not-json"）→ 输出不可解析，
		// 返回 error 让调用方标 unknown，而不是被当成「无容器 = stopped」。
		if len(rows) == 0 {
			return protocol.ComposeProject{}, fmt.Errorf("dockercli: 解析 compose ps 输出失败: %.200s", trimmed)
		}
	}
	proj := projectFromFile(file)
	svcs := make([]protocol.ComposeService, 0, len(rows))
	for _, r := range rows {
		svcs = append(svcs, protocol.ComposeService{Name: r.Service, State: r.State, Image: r.Image})
	}
	proj.Services = svcs
	proj.Status = summarizeStatus(svcs)
	return proj, nil
}

// summarizeStatus 由服务状态推出项目级状态。
func summarizeStatus(svcs []protocol.ComposeService) string {
	if len(svcs) == 0 {
		return "stopped"
	}
	running := 0
	for _, s := range svcs {
		if s.State == "running" {
			running++
		}
	}
	switch {
	case running == len(svcs):
		return "running"
	case running > 0:
		return "partial"
	default:
		return "stopped"
	}
}

// ListProjects 列举白名单内所有 compose 项目及其运行状态。空清单返回
// 非 nil 空切片（与镜像 list 同口径：[] 而非 null）。
func (c *ComposeClient) ListProjects(ctx context.Context) ([]protocol.ComposeProject, error) {
	files, err := c.discover()
	if err != nil {
		return nil, err
	}
	out := make([]protocol.ComposeProject, 0, len(files))
	for _, f := range files {
		proj := projectFromFile(f)
		st, serr := c.status(ctx, f)
		if serr == nil {
			proj.Status = st.Status
			proj.Services = st.Services
		}
		out = append(out, proj)
	}
	return out, nil
}

// resolveProject 把项目名映射到白名单内发现的真实路径。找不到（含任何
// 白名单外的猜测）一律返回 false——这是路径穿越的唯一关卡。
func (c *ComposeClient) resolveProject(ctx context.Context, name string) (protocol.ComposeProject, bool) {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return protocol.ComposeProject{}, false
	}
	for _, p := range projects {
		if p.Name == name {
			return p, true
		}
	}
	return protocol.ComposeProject{}, false
}

// actionArgs 把白名单动作翻译成 `docker compose` 参数（永远带 -f 显式指定
// 文件，绝不依赖 CWD，避免跨项目串扰）。
func actionArgs(action, file string) []string {
	switch protocol.ComposeAction(action) {
	case protocol.ComposeUp:
		return []string{"-f", file, "up", "-d"}
	case protocol.ComposeDown:
		return []string{"-f", file, "down"}
	case protocol.ComposeRestart:
		return []string{"-f", file, "restart"}
	case protocol.ComposePull:
		return []string{"-f", file, "pull"}
	default:
		return nil
	}
}

// runAction 执行一个写动作并返回 compose 的原生输出（stdout+stderr 合流）。
// compose 的业务失败（如端口冲突）体现在退出码与非空 stderr，这里把输出
// 一起带回，调用方原样透传给用户，不另造文案。
func (c *ComposeClient) runAction(ctx context.Context, file, action string) (string, error) {
	args := actionArgs(action, file)
	if args == nil {
		return "", fmt.Errorf("dockercli: 不支持的 compose 动作 %q", action)
	}
	out, err := c.runner.Captured(ctx, args...)
	if err != nil {
		return string(out), fmt.Errorf("dockercli: compose %s 失败: %w (输出: %s)", action, err, string(out))
	}
	return string(out), nil
}

// Up / Down / Restart / Pull —— 四个写动作，语义同上。
func (c *ComposeClient) Up(ctx context.Context, file string) (string, error) {
	return c.runAction(ctx, file, string(protocol.ComposeUp))
}
func (c *ComposeClient) Down(ctx context.Context, file string) (string, error) {
	return c.runAction(ctx, file, string(protocol.ComposeDown))
}
func (c *ComposeClient) Restart(ctx context.Context, file string) (string, error) {
	return c.runAction(ctx, file, string(protocol.ComposeRestart))
}
func (c *ComposeClient) Pull(ctx context.Context, file string) (string, error) {
	return c.runAction(ctx, file, string(protocol.ComposePull))
}

// Logs 打开项目日志流。follow=true 时持续推送（Agent 侧逐帧 Flush），
// 返回读流由调用方关闭。纯文本，调用方传 --no-color 避免 ANSI 噪声。
func (c *ComposeClient) Logs(ctx context.Context, file string, follow bool) (io.ReadCloser, error) {
	args := []string{"-f", file, "logs", "--no-color"}
	if follow {
		args = append(args, "--follow")
	}
	rc, err := c.runner.Streamed(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("dockercli: 打开 compose 日志流失败: %w", err)
	}
	return rc, nil
}
