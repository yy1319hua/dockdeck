package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	ListenAddr    string
	DataDir       string
	DBPath        string
	MasterKeyPath string
	TLSMode       TLSMode
}

// 安全默认值：绑定回环，绝不误绑公网 0.0.0.0。
func defaults() Config {
	dataDir := os.Getenv("DD_DATA_DIR")
	if dataDir == "" {
		dataDir = "/srv/dockdeck/data"
	}
	return Config{
		ListenAddr:    "127.0.0.1:8443",
		DataDir:       dataDir,
		TLSMode:       TLSModeSelf,
		MasterKeyPath: filepath.Join(filepath.Dir(dataDir), "secrets", "master.key"),
	}
}

// Load 先用环境变量兜底，再用命令行 flag 覆盖。
func Load(args []string) (Config, error) {
	c := defaults()
	if v := os.Getenv("DD_LISTEN"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("DD_TLS_MODE"); v != "" {
		m := TLSMode(strings.ToLower(v))
		if m != TLSModeSelf && m != TLSModeNone {
			return Config{}, fmt.Errorf("DD_TLS_MODE 取值非法: %q（需 self|none）", v)
		}
		c.TLSMode = m
	}

	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "监听地址 host:port，必须为 tailscale 网卡 IP 或回环")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "数据目录")
	fs.StringVar(&c.MasterKeyPath, "master-key", c.MasterKeyPath, "机器主密钥文件路径")
	var tlsMode string
	fs.StringVar(&tlsMode, "tls", string(c.TLSMode), "TLS 模式：self|none")
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

// Validate 强制「明文只允许回环」——防止把无 TLS 的监听暴露到 tailnet 之外。
func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("监听地址格式应为 host:port，得到 %q: %w", c.ListenAddr, err)
	}
	if port == "" {
		return fmt.Errorf("监听地址缺少端口: %q", c.ListenAddr)
	}
	ip := net.ParseIP(host)
	// host 为空或 IP 为未指定地址（0.0.0.0 / ::）→ 拒绝，绝不绑所有接口。
	if host == "" || ip == nil || ip.IsUnspecified() {
		return fmt.Errorf("拒绝绑定 %q：DockDeck 只允许绑定 tailscale 网卡 IP 或回环，不得暴露到所有接口", c.ListenAddr)
	}
	if c.TLSMode == TLSModeNone && !ip.IsLoopback() {
		return fmt.Errorf("TLS_MODE=none 仅允许回环监听，当前为 %q；对外请用 self", host)
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
