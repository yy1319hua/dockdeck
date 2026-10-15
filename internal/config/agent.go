// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// DefaultBindIfaces 是默认的允许绑定网卡。
//
// 选 tailscale0 只因为它是当前环境的接入方式，不是协议要求：
// 用户后续可能纳入 WireGuard、ZeroTier 或纯局域网机器，届时用 --bind-ifaces
// 或 --allow-cidr 追加即可，无需改代码。
const DefaultBindIfaces = "tailscale0"

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
type AgentConfig struct {
	// ListenAddr 形如 host:port。
	ListenAddr string
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
	// CertDir 存放自签证书。
	CertDir string
	// TLSMode 与 Hub 侧同义：self 自签、none 明文（仅回环）。
	TLSMode TLSMode
	// BindIfaces 允许绑定的网卡名，命中其中任一网卡的地址即放行。
	BindIfaces []string
	// AllowCIDR 额外放行的 IP 段，供不在 BindIfaces 里的网络使用。
	AllowCIDR []net.IPNet
	// AllowInsecureBind 跳过网卡白名单校验。
	// 仅用于「明确知道自己在做什么」的场景，启动时必须留下告警日志。
	AllowInsecureBind bool
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
	ComposeDirs []string
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

// ifaceAddrs 返回「网卡名 → 该网卡上的 IP 列表」。
//
// 抽成包级变量是为了可测：开发沙箱与 CI 里都没有 tailscale0，
// 不注入就无法覆盖白名单命中的分支。
var ifaceAddrs = func() (map[string][]net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]net.IP, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		var ips []net.IP
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				ips = append(ips, v.IP)
			case *net.IPAddr:
				ips = append(ips, v.IP)
			}
		}
		out[iface.Name] = ips
	}
	return out, nil
}

func agentDefaults() AgentConfig {
	dataDir := os.Getenv("DD_AGENT_DIR")
	if dataDir == "" {
		dataDir = "/srv/dockdeck/agent"
	}
	return AgentConfig{
		// 安全默认值：先绑回环，绝不误绑到所有接口。
		ListenAddr: "127.0.0.1:9527",
		CertDir:    filepath.Join(dataDir, "certs"),
		TLSMode:    TLSModeSelf,
		BindIfaces: []string{DefaultBindIfaces},
		DiskMounts: []string{"/"},
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

	if v := os.Getenv("DD_AGENT_LISTEN"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("DD_AGENT_CERT_DIR"); v != "" {
		c.CertDir = v
	}
	if v := os.Getenv("DD_AGENT_TLS_MODE"); v != "" {
		m, err := parseTLSMode(v)
		if err != nil {
			return AgentConfig{}, fmt.Errorf("DD_AGENT_TLS_MODE %w", err)
		}
		c.TLSMode = m
	}
	if v := os.Getenv("DD_AGENT_BIND_IFACES"); v != "" {
		c.BindIfaces = splitList(v)
	}
	if v := os.Getenv("DD_AGENT_ALLOW_CIDR"); v != "" {
		nets, err := parseCIDRs(v)
		if err != nil {
			return AgentConfig{}, err
		}
		c.AllowCIDR = nets
	}
	if v := os.Getenv("DD_AGENT_DISK_MOUNTS"); v != "" {
		c.DiskMounts = splitList(v)
	}
	if envFlag("DD_AGENT_ALLOW_INSECURE_BIND") {
		c.AllowInsecureBind = true
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
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "监听地址 host:port")
	fs.StringVar(&c.CertDir, "cert-dir", c.CertDir, "自签证书目录")
	bindIfaces := fs.String("bind-ifaces", strings.Join(c.BindIfaces, ","), "允许绑定的网卡名，逗号分隔")
	allowCIDR := fs.String("allow-cidr", "", "额外允许绑定的 IP 段，逗号分隔 CIDR")
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
	insecure := fs.Bool("allow-insecure-bind", c.AllowInsecureBind, "跳过网卡白名单校验（不安全，仅调试）")
	var tlsModeRaw string
	fs.StringVar(&tlsModeRaw, "tls", string(c.TLSMode), "TLS 模式：self|none")
	if err := fs.Parse(args); err != nil {
		return AgentConfig{}, err
	}

	if tlsModeRaw != "" {
		m, err := parseTLSMode(tlsModeRaw)
		if err != nil {
			return AgentConfig{}, fmt.Errorf("-tls %w", err)
		}
		c.TLSMode = m
	}
	if *bindIfaces != "" {
		c.BindIfaces = splitList(*bindIfaces)
	}
	if *allowCIDR != "" {
		nets, err := parseCIDRs(*allowCIDR)
		if err != nil {
			return AgentConfig{}, err
		}
		c.AllowCIDR = nets
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
	if *insecure {
		c.AllowInsecureBind = true
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

	if err := c.normalizeFs(); err != nil {
		return AgentConfig{}, err
	}
	if err := c.Validate(); err != nil {
		return AgentConfig{}, err
	}
	return c, nil
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

// Validate 校验监听地址与凭据是否可用。
//
// 与 Hub 侧 Config.Validate 保持同一套约束：不绑全零地址、明文只走回环，
// 再加 Agent 特有的网卡白名单。
func (c AgentConfig) Validate() error {
	if strings.TrimSpace(c.Token) == "" {
		return fmt.Errorf("缺少 Agent token：请设置环境变量 DD_AGENT_TOKEN 或用 --token-file 指定")
	}
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("监听地址格式应为 host:port，得到 %q: %w", c.ListenAddr, err)
	}
	if port == "" {
		return fmt.Errorf("监听地址缺少端口: %q", c.ListenAddr)
	}
	ip := net.ParseIP(host)
	if host == "" || ip == nil || ip.IsUnspecified() {
		return fmt.Errorf("拒绝绑定 %q：不得暴露到所有接口，请指定具体 IP", c.ListenAddr)
	}
	if c.TLSMode == TLSModeNone && !ip.IsLoopback() {
		return fmt.Errorf("TLS 模式为 none 时仅允许回环监听，当前为 %q；对外请用 self", host)
	}
	if c.AllowInsecureBind {
		// 破例放行，由调用方负责打告警日志。
		return nil
	}
	return c.checkBindAllowed(ip)
}

// checkBindAllowed 判断 IP 是否落在允许绑定的范围内：
// 回环、任一 BindIfaces 网卡的地址、或任一 AllowCIDR 网段。
func (c AgentConfig) checkBindAllowed(ip net.IP) error {
	if ip.IsLoopback() {
		return nil
	}
	addrs, err := ifaceAddrs()
	if err != nil {
		return fmt.Errorf("枚举网卡失败，无法校验绑定地址: %w", err)
	}
	for _, name := range c.BindIfaces {
		for _, owned := range addrs[name] {
			if owned.Equal(ip) {
				return nil
			}
		}
	}
	for _, n := range c.AllowCIDR {
		if n.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("拒绝绑定 %q：地址不属于允许范围（网卡 %s%s）；"+
		"若确实需要请加 --bind-ifaces / --allow-cidr，或用 --allow-insecure-bind 破例",
		ip, strings.Join(c.BindIfaces, ","), cidrHint(c.AllowCIDR))
}

// SplitHostPort 拆出 host 与数字端口，供 cmd/agent 生成证书时复用，
// 避免各处各写一遍解析逻辑。
func SplitHostPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return "", 0, fmt.Errorf("端口非法: %q", portStr)
	}
	return host, port, nil
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

func parseCIDRs(v string) ([]net.IPNet, error) {
	var out []net.IPNet
	for _, part := range splitList(v) {
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("CIDR 非法 %q: %w", part, err)
		}
		if n != nil {
			out = append(out, *n)
		}
	}
	return out, nil
}

// loadAgentToken 取得 token：环境变量优先，其次文件。
// 两者都没有时报错，绝不退化成空 token——那等于关掉鉴权。
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
	return "", fmt.Errorf("缺少 Agent token")
}

func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func cidrHint(nets []net.IPNet) string {
	if len(nets) == 0 {
		return ""
	}
	parts := make([]string, 0, len(nets))
	for _, n := range nets {
		parts = append(parts, n.String())
	}
	return "，额外网段 " + strings.Join(parts, ",")
}
