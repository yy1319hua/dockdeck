// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBindIfaces 是默认的允许绑定网卡。
//
// 选 tailscale0 只因为它是当前环境的接入方式，不是协议要求：
// 用户后续可能纳入 WireGuard、ZeroTier 或纯局域网机器，届时用 --bind-ifaces
// 或 --allow-cidr 追加即可，无需改代码。
const DefaultBindIfaces = "tailscale0"

// AgentConfig 是 Agent 的运行期配置。
type AgentConfig struct {
	// ListenAddr 形如 host:port。
	ListenAddr string
	// Token 是 Hub→Agent 的共享密钥明文，同时用于 Bearer 校验与 HMAC 签名。
	// 它只存在于内存里：Hub 侧的库中副本由机器主密钥加密存储。
	Token string
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

	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "监听地址 host:port")
	fs.StringVar(&c.CertDir, "cert-dir", c.CertDir, "自签证书目录")
	bindIfaces := fs.String("bind-ifaces", strings.Join(c.BindIfaces, ","), "允许绑定的网卡名，逗号分隔")
	allowCIDR := fs.String("allow-cidr", "", "额外允许绑定的 IP 段，逗号分隔 CIDR")
	diskMounts := fs.String("disk-mounts", strings.Join(c.DiskMounts, ","), "统计容量的挂载点，逗号分隔")
	dockerSocket := fs.String("docker-socket", c.DockerSocket, "Docker Engine unix socket 路径")
	selfContainer := fs.String("self-container", c.SelfContainer, "Agent 自身容器 ID/名字（写入保护，默认取 hostname）")
	tokenFile := fs.String("token-file", "", "读取 Agent token 的文件（优先用环境变量 DD_AGENT_TOKEN）")
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
	if *insecure {
		c.AllowInsecureBind = true
	}

	token, err := loadAgentToken(*tokenFile)
	if err != nil {
		return AgentConfig{}, err
	}
	c.Token = token

	if err := c.Validate(); err != nil {
		return AgentConfig{}, err
	}
	return c, nil
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
