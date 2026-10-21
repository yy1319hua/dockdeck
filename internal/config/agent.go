// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 文件管理模块的默认上限。
//
// 三个上限都可配，但都必须有默认值：文件管理是写能力，默认值要能兜住
// 「没配也不出事」——单文件过大、目录过深、目录内条目过多，
// 任一失控都会把 Agent 的内存或宿主机带宽吃掉。
const (
	// DefaultFsMaxFile 是单文件读写上限（字节）。读超限截断并标记，
	// 写超限直接 413，不落半个文件。
	DefaultFsMaxFile = 16 << 20
	// DefaultFsMaxDepth 是相对白名单根的目录深度上限，根的直接子项为 1 层。
	DefaultFsMaxDepth = 8
	// DefaultFsMaxEntries 是单次目录列表返回的条目上限。
	DefaultFsMaxEntries = 2000
)

// DefaultFsDeny 是敏感文件黑名单的内置默认值。
//
// 配置只能往它上面追加，删不掉——把默认保护做成「可被一个参数关掉」，
// 等于给它留了后门。
//
// 匹配语义（由 internal/fsapi 实现）：
//   - 对路径的每一级名字做 path.Match，任一命中即拒——故 ".env" 在任何层级都失效；
//   - 对相对根的完整路径再做一次 path.Match——带 "/" 的模式（如 "etc/shadow"）由此生效。
//
// 判据是「宁可多拦」：漏拦一次私钥的代价，远大于多拦一个同名文件。
var DefaultFsDeny = []string{
	"id_rsa", "id_rsa.*", "id_ed25519", "id_ed25519.*", "id_ecdsa", "id_ecdsa.*",
	"*.pem", "*.key", "*.p12", "*.pfx",
	"*credential*", "*secret*", "*.token",
	".env", ".env.*", ".netrc", ".pgpass",
	"shadow", "shadow-", "gshadow", "passwd",
	"master.key", "authorized_keys",
	".ssh", ".aws", ".gnupg", ".git-credentials", ".docker/config.json",
}

// AgentConfig 是 Agent 的运行期配置。
//
// 2026-10-20（v0.2.6）起 Agent 只有反向长连接一种形态：不监听任何端口，
// 启动后主动连 Hub 的 WebSocket。原先的正向模式（--listen + 网卡白名单 +
// 自签证书）整套删除，相关 flag / 环境变量 / 配置字段一并移除。
type AgentConfig struct {
	// LinkPingInterval 是反向连接的心跳间隔，默认 25s。
	// 必须明显小于反代（Cloudflare 约 100s）的空闲超时，否则连接会被静默掐断。
	LinkPingInterval time.Duration
	// LinkMaxInflight 是单条反向连接的并发请求上限。
	LinkMaxInflight int
	// Token 是 Hub→Agent 的共享密钥明文，同时用于 Bearer 校验与 HMAC 签名。
	// 它只存在于内存里：Hub 侧的库中副本由机器主密钥加密存储。
	Token string
	// HubURL 是 Hub 基址（如 https://hub.example:443）。非空且配了 JoinSecret 时，
	// Agent 启动会向其自注册（GAP-1 纳管）。空 = 不自注册。
	HubURL string
	// JoinSecret 是一次性纳管密钥：Agent 启动时凭它向 Hub 的 /servers/enroll 认领身份，
	// 认领后它即成为本机的长期共享令牌（与 Token 同值）。空 = 不自注册。
	JoinSecret string
	// EnrollInsecure 自注册调用 Hub 时是否跳过 TLS 校验。Hub 用自签证书时为 true（默认），
	// 生产可用 --enroll-insecure=false 强制校验（需 Hub 证书受信）。
	EnrollInsecure bool
	// DiskMounts 是要统计容量的挂载点，默认只统计根。
	DiskMounts []string
	// DockerSocket 是 Docker Engine API 的 unix socket 路径。
	// 空 = 默认 /var/run/docker.sock；容器内部署时挂载宿主机 socket 后传入。
	DockerSocket string
	// SelfContainer 是 Agent 自身容器的 ID 或名字（红线「不碰自身」）。
	// 空 = 用 os.Hostname()（容器内 HOSTNAME 即短 ID）。写入操作命中即 403。
	SelfContainer string
	// ComposeDirs 是 Compose 项目白名单目录：只有这些目录内（含递归）的
	// compose 文件才会被列举与操作。空 = Compose 功能关闭（list 永远空）。
	// 安全边界：路径穿越在 resolveProject 入口被挡，绝不出白名单。
	//
	// 相对路径在入口被归一化成绝对路径（按进程 CWD 解析）——compose 目录
	// 写相对路径时，扫描范围取决于进程工作目录，这在容器里通常是 "/",
	// 容易扫出一堆意料之外的东西。归一化后语义确定，并在
	// ComposeDirWarnings 里给出提醒。
	ComposeDirs []string
	// ComposeDirWarnings 是 compose 目录配置里的可疑项提示（相对路径、
	// 归一化后指向根目录等）。启动时打印给运维看——配置错了要能被看见，
	// 而不是安静地扫了个寂寞。
	ComposeDirWarnings []string
	// FsRoots 是文件管理模块的根白名单目录（可多个，绝对路径）。
	// 空 = 模块关闭：cmd/agent 不注册任何 /files 路由，功能在网络层就不存在
	// （红线「未配 --fs-root 时该模块不注册路由」）。
	// 安全边界：所有路径先 realpath 解引用再做前缀比对，绝不出白名单。
	FsRoots []string
	// FsMaxFile 是单文件读写上限（字节）。
	FsMaxFile int64
	// FsMaxDepth 是目录深度上限（相对根，根的直接子项为 1 层）。
	FsMaxDepth int
	// FsMaxEntries 是单次目录列表的条目上限。
	FsMaxEntries int
	// FsDeny 是敏感文件黑名单 pattern（path.Match 语法）。
	// 它 = DefaultFsDeny + 配置追加，配置删不掉默认保护。
	FsDeny []string
}

func agentDefaults() AgentConfig {
	return AgentConfig{
		LinkPingInterval: 25 * time.Second,
		LinkMaxInflight:  64,
		DiskMounts:       []string{"/"},
		// 文件管理的上限与黑名单先落默认值；FsRoots 故意留空——
		// 「没配根目录」就是要让模块整体关闭。
		FsMaxFile:    DefaultFsMaxFile,
		FsMaxDepth:   DefaultFsMaxDepth,
		FsMaxEntries: DefaultFsMaxEntries,
		FsDeny:       append([]string(nil), DefaultFsDeny...),
		// Hub 自签证书场景默认放行自注册的 TLS 校验；生产可显式关。
		EnrollInsecure: true,
	}
}

// LoadAgent 解析 Agent 配置：环境变量兜底，命令行 flag 覆盖。
//
// token 不接受明文命令行参数——那会出现在 ps 输出里，
// 只能来自 DD_AGENT_TOKEN 环境变量或 --token-file 指向的文件。
func LoadAgent(args []string) (AgentConfig, error) {
	c := agentDefaults()

	if v := os.Getenv("DD_AGENT_DISK_MOUNTS"); v != "" {
		c.DiskMounts = splitList(v)
	}
	if v := os.Getenv("DD_AGENT_DOCKER_SOCKET"); v != "" {
		c.DockerSocket = v
	}
	if v := os.Getenv("DD_AGENT_SELF_CONTAINER"); v != "" {
		c.SelfContainer = v
	}
	if v := os.Getenv("DD_AGENT_COMPOSE_DIRS"); v != "" {
		c.ComposeDirs = splitList(v)
	}
	if v := os.Getenv("DD_AGENT_LINK_PING"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return AgentConfig{}, fmt.Errorf("DD_AGENT_LINK_PING 非法: %w", err)
		}
		c.LinkPingInterval = d
	}
	if v := os.Getenv("DD_AGENT_LINK_INFLIGHT"); v != "" {
		n, err := parsePositiveInt64("DD_AGENT_LINK_INFLIGHT", v)
		if err != nil {
			return AgentConfig{}, err
		}
		c.LinkMaxInflight = int(n)
	}
	if v := os.Getenv("DD_AGENT_FS_ROOTS"); v != "" {
		c.FsRoots = splitList(v)
	}
	if v := os.Getenv("DD_AGENT_FS_MAX_FILE"); v != "" {
		n, err := parsePositiveInt64("DD_AGENT_FS_MAX_FILE", v)
		if err != nil {
			return AgentConfig{}, err
		}
		c.FsMaxFile = n
	}
	if v := os.Getenv("DD_AGENT_FS_MAX_DEPTH"); v != "" {
		n, err := parsePositiveInt("DD_AGENT_FS_MAX_DEPTH", v)
		if err != nil {
			return AgentConfig{}, err
		}
		c.FsMaxDepth = n
	}
	if v := os.Getenv("DD_AGENT_FS_MAX_ENTRIES"); v != "" {
		n, err := parsePositiveInt("DD_AGENT_FS_MAX_ENTRIES", v)
		if err != nil {
			return AgentConfig{}, err
		}
		c.FsMaxEntries = n
	}
	if v := os.Getenv("DD_AGENT_FS_DENY"); v != "" {
		c.FsDeny = append(c.FsDeny, splitList(v)...)
	}
	if v := os.Getenv("DD_HUB_URL"); v != "" {
		c.HubURL = strings.TrimSpace(v)
	}
	if v := os.Getenv("DD_AGENT_JOIN_SECRET"); v != "" {
		c.JoinSecret = strings.TrimSpace(v)
	}
	if v := os.Getenv("DD_AGENT_ENROLL_INSECURE"); v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			c.EnrollInsecure = b
		}
	}

	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	diskMounts := fs.String("disk-mounts", strings.Join(c.DiskMounts, ","), "统计容量的挂载点，逗号分隔")
	dockerSocket := fs.String("docker-socket", c.DockerSocket, "Docker Engine unix socket 路径")
	selfContainer := fs.String("self-container", c.SelfContainer, "Agent 自身容器 ID/名字（写入保护，默认取 hostname）")
	composeDirs := fs.String("compose-dirs", strings.Join(c.ComposeDirs, ","), "Compose 白名单目录，逗号分隔（递归扫描其内 compose 文件）")
	fsRoots := fs.String("fs-root", strings.Join(c.FsRoots, ","), "文件管理根白名单目录，逗号分隔（留空 = 关闭文件管理模块）")
	fsDeny := fs.String("fs-deny", "", "追加的敏感文件黑名单 pattern，逗号分隔（内置默认名单不可删）")
	fs.Int64Var(&c.FsMaxFile, "fs-max-file", c.FsMaxFile, "单文件读写上限（字节）")
	fs.IntVar(&c.FsMaxDepth, "fs-max-depth", c.FsMaxDepth, "目录深度上限（相对白名单根）")
	fs.IntVar(&c.FsMaxEntries, "fs-max-entries", c.FsMaxEntries, "单次目录列表条目上限")
	tokenFile := fs.String("token-file", "", "读取 Agent token 的文件（优先用环境变量 DD_AGENT_TOKEN）")
	hubURL := fs.String("hub-url", c.HubURL, "Hub 基址（https://host）；非空且配了 join_secret 时启动自注册纳管")
	joinSecretFile := fs.String("join-secret-file", "", "读取纳管 join_secret 的文件（优先用环境变量 DD_AGENT_JOIN_SECRET）")
	insecureEnroll := fs.Bool("enroll-insecure", c.EnrollInsecure, "自注册调用 Hub 时跳过 TLS 校验（Hub 自签证书场景默认开）")
	fs.DurationVar(&c.LinkPingInterval, "link-ping", c.LinkPingInterval, "反向连接心跳间隔（默认 25s，须小于反代空闲超时）")
	fs.IntVar(&c.LinkMaxInflight, "link-inflight", c.LinkMaxInflight, "反向连接并发请求上限")
	if err := fs.Parse(args); err != nil {
		return AgentConfig{}, err
	}

	if *diskMounts != "" {
		c.DiskMounts = splitList(*diskMounts)
	}
	if *dockerSocket != "" {
		c.DockerSocket = *dockerSocket
	}
	if *selfContainer != "" {
		c.SelfContainer = *selfContainer
	}
	if *composeDirs != "" {
		c.ComposeDirs = splitList(*composeDirs)
	}
	if *fsRoots != "" {
		c.FsRoots = splitList(*fsRoots)
	}
	if *fsDeny != "" {
		c.FsDeny = append(c.FsDeny, splitList(*fsDeny)...)
	}
	c.HubURL = strings.TrimSpace(*hubURL)
	c.EnrollInsecure = *insecureEnroll
	if c.JoinSecret == "" && *joinSecretFile != "" {
		b, err := os.ReadFile(*joinSecretFile)
		if err != nil {
			return AgentConfig{}, fmt.Errorf("读取 join_secret 文件失败: %w", err)
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			c.JoinSecret = t
		} else {
			return AgentConfig{}, fmt.Errorf("join_secret 文件为空: %s", *joinSecretFile)
		}
	}

	token, err := loadAgentToken(*tokenFile)
	if err != nil {
		return AgentConfig{}, err
	}
	c.Token = token
	// join_secret 即本机长期共享令牌初值：只配邀请未配 token 时据此派生，
	// 让「只给一次性邀请」也能启动（Hub 侧存的就是这把 secret 的密文）。
	if strings.TrimSpace(c.Token) == "" && c.JoinSecret != "" {
		c.Token = c.JoinSecret
	}

	c.normalizeComposeDirs()
	if err := c.normalizeFs(); err != nil {
		return AgentConfig{}, err
	}
	if err := c.Validate(); err != nil {
		return AgentConfig{}, err
	}
	return c, nil
}

// normalizeComposeDirs 归一化 compose 白名单目录：相对路径按进程 CWD 解析成
// 绝对路径，并把可疑项记进 ComposeDirWarnings。
//
// 为什么必须归一化：compose 白名单里的相对路径（如 "."）在容器里按进程
// CWD（通常是 "/"）解释，等于「递归扫描容器根文件系统」——扫出什么全看
// 运气，运维自己都说不清面板上那些项目是哪来的。归一化把语义变成确定的
// 绝对路径，并把「你写的是相对路径」这件事明确告诉运维。
//
// 为什么不直接拒绝相对路径：那会让既有配置直接启动失败。归一化 + 告警
// 保留了兼容性，同时把「隐式猜测」变成「显式且有日志的行为」。
func (c *AgentConfig) normalizeComposeDirs() {
	out := make([]string, 0, len(c.ComposeDirs))
	for _, raw := range c.ComposeDirs {
		d := strings.TrimSpace(raw)
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			abs, err := filepath.Abs(d)
			if err != nil {
				c.ComposeDirWarnings = append(c.ComposeDirWarnings,
					fmt.Sprintf("compose 目录 %q 无法解析为绝对路径，已忽略", raw))
				continue
			}
			c.ComposeDirWarnings = append(c.ComposeDirWarnings,
				fmt.Sprintf("compose 目录 %q 是相对路径，已按进程工作目录解析为 %q；建议直接写绝对路径", raw, abs))
			d = abs
		}
		d = filepath.Clean(d)
		if d == "/" {
			c.ComposeDirWarnings = append(c.ComposeDirWarnings,
				"compose 目录解析后为根目录 /，将递归扫描整个文件系统；建议改为具体项目目录")
		}
		out = append(out, d)
	}
	c.ComposeDirs = dedupeStrings(out)
}

// normalizeFs 归一化文件管理配置：根目录必须是绝对路径并去掉冗余段，
// 三个上限必须是正数，黑名单去重。
//
// 归一化放在入口而不是使用点：白名单前缀比对依赖「已 Clean 的绝对路径」这个前提，
// 让每个使用点各自记得先 Clean，迟早会漏一处。
func (c *AgentConfig) normalizeFs() error {
	roots := make([]string, 0, len(c.FsRoots))
	for _, raw := range c.FsRoots {
		r := strings.TrimSpace(raw)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			return fmt.Errorf("文件管理根目录必须是绝对路径: %q", raw)
		}
		roots = append(roots, filepath.Clean(r))
	}
	c.FsRoots = dedupeStrings(roots)

	if c.FsMaxFile <= 0 {
		return fmt.Errorf("单文件上限（--fs-max-file）必须是正整数，得到 %d", c.FsMaxFile)
	}
	// 上界由 protocol 的共享常量定：Agent 鉴权层要读完整个 body 才能验签，
	// 所以「单文件上限」实际等于「单个上传请求的最大内存占用」。
	if c.FsMaxFile > protocol.MaxFileUploadBytes {
		return fmt.Errorf("单文件上限（--fs-max-file）不得超过 %d 字节：鉴权层要对整个请求体验签，上限即单请求内存占用（见 protocol.MaxFileUploadBytes）",
			protocol.MaxFileUploadBytes)
	}
	if c.FsMaxDepth <= 0 {
		return fmt.Errorf("目录深度上限（--fs-max-depth）必须是正整数，得到 %d", c.FsMaxDepth)
	}
	if c.FsMaxEntries <= 0 {
		return fmt.Errorf("列表条目上限（--fs-max-entries）必须是正整数，得到 %d", c.FsMaxEntries)
	}
	c.FsDeny = dedupeStrings(c.FsDeny)
	return nil
}

// Validate 校验凭据与 Hub 地址是否可用。
//
// 2026-10-20（v0.2.6）起 Agent 只有反向一种形态：不监听端口，
// 监听地址 / 网卡白名单 / TLS 模式三组校验整套删除，只留反向连接的硬前提。
func (c AgentConfig) Validate() error {
	if strings.TrimSpace(c.Token) == "" {
		return fmt.Errorf("缺少 Agent token：请设置环境变量 DD_AGENT_TOKEN、用 --token-file 指定，或配置 DD_AGENT_JOIN_SECRET（一次性纳管密钥，认领后即本机长期令牌）")
	}
	if strings.TrimSpace(c.HubURL) == "" {
		return fmt.Errorf("必须配置 Hub 地址：--hub-url 或 DD_HUB_URL")
	}
	u, err := url.Parse(c.HubURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("Hub 地址必须是 http(s)://host，得到 %q", c.HubURL)
	}
	if c.LinkPingInterval <= 0 || c.LinkMaxInflight <= 0 {
		return fmt.Errorf("反向连接参数非法：link-ping / link-inflight 必须为正")
	}
	return nil
}

func parseTLSMode(v string) (TLSMode, error) {
	m := TLSMode(strings.ToLower(strings.TrimSpace(v)))
	if m != TLSModeSelf && m != TLSModeNone {
		return "", fmt.Errorf("取值非法: %q（需 self|none）", v)
	}
	return m, nil
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// dedupeStrings 保序去重，空串丢弃。
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// parsePositiveInt 解析正整数配置项，错误里带变量名，便于用户对上号。
func parsePositiveInt(name, v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s 必须是正整数，得到 %q", name, v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s 必须是正整数，得到 %d", name, n)
	}
	return n, nil
}

// parsePositiveInt64 同上，用于以字节计的上限（可能超过 32 位）。
func parsePositiveInt64(name, v string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是正整数，得到 %q", name, v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s 必须是正整数，得到 %d", name, n)
	}
	return n, nil
}

// loadAgentToken 取得 token：环境变量优先，其次文件。
// 两者都未配置时返回空串而不报错——「只给一次性 join_secret」的形态
// 也要能启动，token 由调用方从 join_secret 派生；派生后仍为空才由
// Validate 拒绝（绝不带着空 token 进运行期，那等于关掉鉴权）。
func loadAgentToken(tokenFile string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("DD_AGENT_TOKEN")); v != "" {
		return v, nil
	}
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("读取 token 文件失败: %w", err)
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("token 文件为空: %s", tokenFile)
	}
	return "", nil
}

func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
