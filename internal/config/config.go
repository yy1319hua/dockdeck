package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// TLSMode 控制 Hub 的对外传输。
type TLSMode string

const (
	TLSModeSelf TLSMode = "self" // 自签证书，指纹显示在设置页
	TLSModeNone TLSMode = "none" // 明文，仅允许回环监听（反代场景）
)

// Config 是 Hub 的运行期配置。
type Config struct {
	ListenAddr string
	// AllowInsecureBind 显式允许绑定 0.0.0.0（监听所有网卡）。
	// 用于「内网多网卡、且将来可能增删网卡」的场景；默认 false = 硬拒。
	// 开启后面板会暴露到内网与 docker 网桥，启动必须打告警。
	AllowInsecureBind bool
	DataDir           string
	DBPath            string
	MasterKeyPath     string
	TLSMode           TLSMode
	// MetricsIntervalSec 是轮询各 Agent 采集指标的周期（秒）。
	MetricsIntervalSec int
	// MetricsRetentionDays 是指标历史（metrics_snapshots）的保留天数。
	MetricsRetentionDays int
	// AuditRetentionDays 是审计日志（audit_logs）的保留天数，0 = 永久保留。
	AuditRetentionDays int
	// DumpOpenAPI 非空时，Hub 只把 OpenAPI 文档生成到该目录后退出，不启动服务。
	DumpOpenAPI string
}

// 安全默认值：绑定回环，绝不误绑公网 0.0.0.0。
func defaults() Config {
	dataDir := os.Getenv("DD_DATA_DIR")
	if dataDir == "" {
		dataDir = "/srv/dockdeck/data"
	}
	return Config{
		ListenAddr:           "127.0.0.1:8443",
		DataDir:              dataDir,
		TLSMode:              TLSModeSelf,
		MasterKeyPath:        filepath.Join(filepath.Dir(dataDir), "secrets", "master.key"),
		MetricsIntervalSec:   30,
		MetricsRetentionDays: 14,
	}
}

// Load 先用环境变量兜底，再用命令行 flag 覆盖。
func Load(args []string) (Config, error) {
	c := defaults()
	if v := os.Getenv("DD_LISTEN"); v != "" {
		c.ListenAddr = v
	}
	if envFlag("DD_ALLOW_INSECURE_BIND") {
		c.AllowInsecureBind = true
	}
	if v := os.Getenv("DD_TLS_MODE"); v != "" {
		m := TLSMode(strings.ToLower(v))
		if m != TLSModeSelf && m != TLSModeNone {
			return Config{}, fmt.Errorf("DD_TLS_MODE 取值非法: %q（需 self|none）", v)
		}
		c.TLSMode = m
	}
	if v := os.Getenv("DD_METRICS_INTERVAL_SEC"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("DD_METRICS_INTERVAL_SEC 需为正整数秒，得到 %q", v)
		}
		c.MetricsIntervalSec = n
	}
	if v := os.Getenv("DD_METRICS_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("DD_METRICS_RETENTION_DAYS 需为正整数天，得到 %q", v)
		}
		c.MetricsRetentionDays = n
	}

	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "监听地址 host:port，必须为 tailnet / 内网 / 回环的具体 IP（禁止 0.0.0.0）")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "数据目录")
	fs.StringVar(&c.MasterKeyPath, "master-key", c.MasterKeyPath, "机器主密钥文件路径")
	fs.BoolVar(&c.AllowInsecureBind, "allow-insecure-bind", c.AllowInsecureBind,
		"允许绑定 0.0.0.0（危险：面板会监听所有网卡，含内网与 docker 网桥）")
	var tlsMode string
	fs.StringVar(&tlsMode, "tls", string(c.TLSMode), "TLS 模式：self|none")
	fs.IntVar(&c.MetricsIntervalSec, "metrics-interval-sec", c.MetricsIntervalSec, "指标采集周期（秒）")
	fs.IntVar(&c.MetricsRetentionDays, "metrics-retention-days", c.MetricsRetentionDays, "指标历史保留天数")
	fs.IntVar(&c.AuditRetentionDays, "audit-retention-days", c.AuditRetentionDays, "审计日志保留天数（0 = 永久保留）")
	fs.StringVar(&c.DumpOpenAPI, "dump-openapi", c.DumpOpenAPI, "生成 OpenAPI 文档（openapi.json / openapi.yaml）到指定目录后退出，不启动服务")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if tlsMode != "" {
		m := TLSMode(strings.ToLower(tlsMode))
		if m != TLSModeSelf && m != TLSModeNone {
			return Config{}, fmt.Errorf("-tls 取值非法: %q（需 self|none）", tlsMode)
		}
		c.TLSMode = m
	}

	c.DBPath = filepath.Join(c.DataDir, "dockdeck.db")
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate 强制「明文只允许回环」——防止把无 TLS 的监听暴露到不可信网络。
func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("监听地址格式应为 host:port，得到 %q: %w", c.ListenAddr, err)
	}
	if port == "" {
		return fmt.Errorf("监听地址缺少端口: %q", c.ListenAddr)
	}
	ip := net.ParseIP(host)
	if host == "" || ip == nil {
		return fmt.Errorf("监听地址非法: %q", c.ListenAddr)
	}
	// 未指定地址（0.0.0.0 / ::）默认硬拒；只有显式 --allow-insecure-bind 才放行。
	// 放行的代价是面板暴露到所有网卡（含内网与 docker 网桥），启动时打告警。
	if ip.IsUnspecified() && !c.AllowInsecureBind {
		return fmt.Errorf("拒绝绑定 %q：DockDeck 只允许绑定具体 IP（tailnet / 内网 / 回环）；"+
			"确需监听所有网卡请显式加 --allow-insecure-bind 或 DD_ALLOW_INSECURE_BIND=1", c.ListenAddr)
	}
	if c.TLSMode == TLSModeNone && !ip.IsLoopback() {
		return fmt.Errorf("TLS_MODE=none 仅允许回环监听，当前为 %q；对外请用 self", host)
	}
	// 采集周期与保留期都必须是正数：0 会让 ticker 直接 panic，
	// 负保留期会把所有历史一次性清空。
	if c.MetricsIntervalSec <= 0 {
		return fmt.Errorf("指标采集周期须为正整数秒，得到 %d", c.MetricsIntervalSec)
	}
	if c.MetricsRetentionDays <= 0 {
		return fmt.Errorf("指标保留天数须为正整数，得到 %d", c.MetricsRetentionDays)
	}
	return nil
}

// EnsureDirs 创建数据与密钥目录，密钥目录权限收紧到 0700。
func (c Config) EnsureDirs() error {
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.MasterKeyPath), 0o700); err != nil {
		return err
	}
	return nil
}
