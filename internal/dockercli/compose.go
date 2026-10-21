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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/diff"
	"github.com/yy1319hua/dockdeck/internal/guard"
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

	// ReadFile 读取 compose 文件当前内容（file 由 ResolveProject 给出，已在白名单内）。
	ReadFile(ctx context.Context, file string) (string, error)
	// Diff 计算 content 相对当前文件的 unified diff（预览用，不落盘、不产生备份）。
	Diff(ctx context.Context, file string, content string) (string, error)
	// WriteFile 写前把当前内容备份为 <file>.bak.<RFC3339>，再覆盖写入 content，
	// 返回备份文件路径（供回滚）。
	WriteFile(ctx context.Context, file string, content string) (string, error)

	// S7.11 备份三件套（实现在 compose_backup.go）：
	//   ListBackups  列举写前备份（最新在前）；
	//   ReadBackup   读备份正文（名字过三道路径关卡）；
	//   RestoreBackup 用备份覆盖当前文件——恢复本身也是覆盖写，故同样先备份
	//   当前内容，否则回滚是单向门。**不提供删除**：备份只增不减。
	ListBackups(ctx context.Context, file string) ([]protocol.ComposeBackup, error)
	ReadBackup(ctx context.Context, file, name string) (string, error)
	RestoreBackup(ctx context.Context, file, name string) (string, error)
}

// ComposeClient 操作白名单目录内的 compose 项目。
type ComposeClient struct {
	dirs   []string
	runner composeRunner
	// selfContainer 是 Agent 自身容器的 ID 或名字（可空）。
	// 用于自保护：down/restart 一个「含自身容器」的 compose 项目会让承载
	// Agent 的容器被停掉——Agent 随即失联，面板再也操作不了这台机器，
	// 且没有任何办法从面板侧恢复（容器已被 compose 删除/停掉）。
	// 故这类动作在 Agent 侧直接拒绝，让操作者回到宿主机手工处理。
	selfContainer string
}

// NewComposeClient 用配置好的白名单目录装配客户端。dirs 为空则 list 永远
// 返回空（相当于 Compose 功能未启用）。
func NewComposeClient(dirs []string) *ComposeClient {
	return &ComposeClient{dirs: dirs, runner: execComposeRunner{}}
}

// NewComposeClientWithSelf 在 NewComposeClient 的基础上带上自身容器标识，
// 开启「不 down/restart 承载自己的 compose 项目」自保护。
// self 为空表示未配自身身份，此时不做自保护（与容器模块的 selfContainer
// 缺省口径一致：未配即不启用）。
func NewComposeClientWithSelf(dirs []string, self string) *ComposeClient {
	c := NewComposeClient(dirs)
	c.selfContainer = self
	return c
}

// composeFileNames 是被识别为项目入口的基础文件名（大小写敏感，避免误吞无关 yml）。
var composeFileNames = map[string]bool{
	"docker-compose.yml":  true,
	"docker-compose.yaml": true,
	"compose.yml":         true,
	"compose.yaml":        true,
}

// composeOverrideSuffix 是 compose 生态里「叠加文件」的后缀：docker-compose.override.yml
// 是叠加在主文件之上的覆盖层，不是独立项目。把它当项目列出会让面板对同一组容器
// 操作两次（一次主文件一次覆盖层），故显式排除。
const composeOverrideSuffix = "override"

// composeSuffixOf 判断一个文件名是否为 compose 入口文件，并给出它的「角色后缀」。
//
// 识别两类形态：
//   - 基础名：docker-compose.yml / docker-compose.yaml / compose.yml / compose.yaml
//     → 后缀为空串；
//   - 带角色后缀：docker-compose.<suffix>.yml|yaml、compose.<suffix>.yml|yaml
//     → 后缀为 <suffix>（如 docker-compose.agent.yml → "agent"）。
//
// 为什么放开带后缀的形态：v0.2.6 起 Agent 拆出独立的 docker-compose.agent.yml，
// 它与 docker-compose.yml 同处一个目录，是两个必须分别可见、分别操作的项目。
// 早期只认四个精确名，agent 那份永远被跳过——面板看不到自己。
//
// 后缀规则：非空、不含路径分隔符、且不是 override（叠加文件不是独立项目）。
// 备份文件名形如 <file>.bak.<时间戳>，不以 .yml/.yaml 结尾，天然不匹配。
func composeSuffixOf(name string) (string, bool) {
	if composeFileNames[name] {
		return "", true
	}
	base := ""
	for _, e := range []string{".yml", ".yaml"} {
		if strings.HasSuffix(name, e) {
			base = strings.TrimSuffix(name, e)
			break
		}
	}
	if base == "" {
		return "", false
	}
	for _, prefix := range []string{"docker-compose.", "compose."} {
		if !strings.HasPrefix(base, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(base, prefix)
		if suffix == "" || suffix == composeOverrideSuffix {
			return "", false
		}
		if strings.ContainsAny(suffix, "/\\") {
			return "", false
		}
		return suffix, true
	}
	return "", false
}

// projectNameFor 由项目目录与 compose 文件的角色后缀推出项目名。
//
// 命名规则：目录名（带后缀时为「目录名-后缀」）。这样同一目录下的多个 compose
// 文件天然区分（dockdeck 与 dockdeck-agent），且与 docker compose 自己认的项目名
// 一致——compose 优先用文件顶层的 name:，无 name: 时退回目录名。
//
// 面板名不必等于 compose 项目名：所有写动作都以 `-f <绝对路径>` 执行，由 compose
// 自己按文件里的 name: 决定操作哪组容器，故面板名只需在白名单内唯一可解析。
func projectNameFor(dir string, suffix string) string {
	base := filepath.Base(dir)
	if suffix == "" {
		return base
	}
	return base + "-" + suffix
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
			if _, ok := composeSuffixOf(d.Name()); ok {
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
	suffix, _ := composeSuffixOf(filepath.Base(file))
	return protocol.ComposeProject{
		Name:       projectNameFor(dir, suffix),
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
	// Name 是容器名（v2 的 ps --format json 给出 "Name"；老版本只给逗号分隔的
	// "Names"）。两者都留，Container 取其一即可。
	Name string `json:"Name"`
	// Names 是逗号分隔的容器名列表（老版本 compose 的形态）。
	Names string `json:"Names"`
	// ID 是容器短 ID，与容器名一起用于「这个项目是否含 Agent 自身容器」的判定。
	ID string `json:"ID"`
}

// containerName 取这一行对应的容器名：优先 Name，空则取 Names 的第一段。
func (r composePsRow) containerName() string {
	if r.Name != "" {
		return r.Name
	}
	if r.Names == "" {
		return ""
	}
	if i := strings.IndexByte(r.Names, ','); i >= 0 {
		return strings.TrimSpace(r.Names[:i])
	}
	return strings.TrimSpace(r.Names)
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
		svcs = append(svcs, protocol.ComposeService{
			Name:        r.Service,
			State:       r.State,
			Image:       r.Image,
			Container:   r.containerName(),
			ContainerID: r.ID,
		})
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
	// 项目名是白名单内的唯一标识（前端拿它做路由段与列表 key），
	// 故重名必须消解：同名者依次追加 -2、-3 后缀。
	// 早前同名的后者「不可达」——面板永远操作不到那个项目，是真 bug。
	seen := make(map[string]int, len(files))
	taken := make(map[string]bool, len(files))
	for _, f := range files {
		proj := projectFromFile(f)
		seen[proj.Name]++
		if seen[proj.Name] > 1 {
			proj.Name = fmt.Sprintf("%s-%d", proj.Name, seen[proj.Name])
			for taken[proj.Name] {
				seen[proj.Name]++
				proj.Name = fmt.Sprintf("%s-%d", proj.Name, seen[proj.Name])
			}
		}
		taken[proj.Name] = true
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
func (c *ComposeClient) ResolveProject(ctx context.Context, name string) (protocol.ComposeProject, bool) {
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
	// 自保护：停掉承载 Agent 自己的 compose 项目 = 把自己的脚砍了。
	// Agent 随即失联、面板再也操作不了这台机器，且无法从面板侧恢复。
	// 故 down/restart 前先确认目标项目不含自身容器。
	if err := c.checkNotSelf(ctx, file, action); err != nil {
		return "", err
	}
	out, err := c.runner.Captured(ctx, args...)
	if err != nil {
		return string(out), fmt.Errorf("dockercli: compose %s 失败: %w (输出: %s)", action, err, string(out))
	}
	return string(out), nil
}

// checkNotSelf 是 compose 侧的「不碰自身」自保护。
//
// 只拦 down 与 restart：这两个动作会停掉/删除容器，而 up 与 pull 不停容器
// （up 在容器已在跑时是幂等的，pull 只拉镜像）。若目标项目里含 Agent 自身
// 容器，停掉它意味着 Agent 自己失联——面板此后无法再操作这台机器，也无从
// 恢复（容器已被 compose 移除）。故这类动作直接拒绝，请操作者到宿主机手工执行。
//
// 判定复用 guard.IsSelf 的比较语义（大小写不敏感的双向前缀匹配）：容器名与
// 容器 ID 都比一遍——compose 的 ps 输出两者都给，任一命中即算自身。
func (c *ComposeClient) checkNotSelf(ctx context.Context, file, action string) error {
	if c.selfContainer == "" {
		return nil
	}
	switch protocol.ComposeAction(action) {
	case protocol.ComposeDown, protocol.ComposeRestart:
	default:
		return nil
	}
	g, err := guard.New(c.selfContainer)
	if err != nil {
		// 自身标识构造失败（理论上不会：guard.New 只对 patterns 报错，
		// selfID 空/非法不会失败）——按最保守处理：拦下本次动作。
		return fmt.Errorf("dockercli: 自身容器标识无效，拒绝执行 compose %s: %w", action, err)
	}
	proj, serr := c.status(ctx, file)
	if serr != nil {
		// 取不到项目状态就放行：这是「查不到」而非「确认命中自身」，
		// 拦住会误伤正常操作；真命中自身的情形在 status 能取到时必被拦。
		return nil
	}
	for _, sv := range proj.Services {
		if g.IsSelf(sv.Container) || (sv.ContainerID != "" && g.IsSelf(sv.ContainerID)) {
			return fmt.Errorf("dockercli: 拒绝执行 compose %s——该项目含 Agent 自身容器 %s，停掉它会让 Agent 失联且无法从面板恢复；请在宿主机手工执行", action, sv.Container)
		}
	}
	return nil
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

// ReadFile 读取 compose 文件当前内容。file 由 ResolveProject 给出，已在白名单内，
// 故这里不再做路径校验（穿越在解析项目名那一步就挡住了）。
func (c *ComposeClient) ReadFile(ctx context.Context, file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("dockercli: 读取 compose 文件失败: %w", err)
	}
	return string(data), nil
}

// Diff 计算 content 相对当前文件的 unified diff（预览用，不落盘、不产生备份）。
func (c *ComposeClient) Diff(ctx context.Context, file string, content string) (string, error) {
	cur, err := c.ReadFile(ctx, file)
	if err != nil {
		return "", err
	}
	return diff.Unified(cur, content), nil
}

// WriteFile 写前把当前内容备份为 <file>.bak.<定宽奈秒时间戳>，再覆盖写入 content，
// 返回备份文件路径供用户回滚。备份先落盘成功才动原文件——任一失败即中止，
// 不给"原文件已改但无备份"留口子（对应红线「备份优先于变更」）。
//
// 备份名的时间戳格式见 backupTimeLayout（compose_backup.go）：定宽奈秒 +
// writeBackupExclusive 的 O_EXCL，保证同名不覆盖。写成功后跑一次轮转
// （rotateComposeBackups），把备份数压回 maxComposeBackups 以内。
func (c *ComposeClient) WriteFile(ctx context.Context, file string, content string) (string, error) {
	cur, err := c.ReadFile(ctx, file)
	if err != nil {
		return "", err
	}
	backup, err := writeBackupExclusive(file+".bak."+time.Now().Format(backupTimeLayout), []byte(cur))
	if err != nil {
		return "", fmt.Errorf("dockercli: 备份 compose 文件失败: %w", err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("dockercli: 写入 compose 文件失败: %w", err)
	}
	// 轮转放在写入成功之后：只在前一次完整成功的基础上做清理，失败不轮转。
	// 轮转本身尽力而为（见其注释），删不掉也不该让已成功的写入报错。
	rotateComposeBackups(file)
	return backup, nil
}

// writeBackupExclusive 以 O_CREATE|O_EXCL 原子落一份新备份：目标已存在则返回
// ErrExist，由本函数自动换 <base>.N 后缀重试（N 有上限，防病态无限循环），
// 返回实际落盘的名字。
//
// 为什么用 O_EXCL 而不是「先查存在、再 os.WriteFile」：后者在查与写之间有
// 窗口，且 time.RFC3339 是秒级——同一秒两次写（或粗时钟服务器上奈秒都撞）会
// 算出同一个名，后一次直接清空重写前一份，用户静默丢一个备份。O_EXCL 把
// 「绝不覆盖既有备份」变成内核保证，不依赖时钟精度：备份是回滚点，被静默
// 覆盖等于偷走用户一条退路（S7.11 红线「备份优先于变更」）。
func writeBackupExclusive(base string, data []byte) (string, error) {
	name := base
	for n := 1; n <= 1000; n++ {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			if _, werr := f.Write(data); werr != nil {
				f.Close()
				return "", werr
			}
			return name, f.Close()
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		name = fmt.Sprintf("%s.%d", base, n)
	}
	return "", fmt.Errorf("dockercli: 备份名冲突过多: %s", base)
}
