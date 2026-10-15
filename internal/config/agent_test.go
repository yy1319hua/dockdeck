// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package config

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// withIfaces 替换网卡枚举，让白名单分支在没有任何真实 tailscale 网卡的机器上也可测。
func withIfaces(t *testing.T, m map[string][]net.IP) {
	t.Helper()
	orig := ifaceAddrs
	ifaceAddrs = func() (map[string][]net.IP, error) { return m, nil }
	t.Cleanup(func() { ifaceAddrs = orig })
}

func TestLoadAgent_RequiresToken(t *testing.T) {
	t.Setenv("DD_AGENT_TOKEN", "")
	if _, err := LoadAgent([]string{"-listen", "127.0.0.1:9527"}); err == nil {
		t.Fatal("缺少 token 时应报错，不能退化成无鉴权")
	} else if !strings.Contains(err.Error(), "token") {
		t.Errorf("错误信息应提示 token，got %v", err)
	}
}

func TestLoadAgent_RejectsWildcardBind(t *testing.T) {
	t.Setenv("DD_AGENT_TOKEN", "tok")
	for _, addr := range []string{"0.0.0.0:9527", ":9527", "[::]:9527"} {
		if _, err := LoadAgent([]string{"-listen", addr}); err == nil {
			t.Errorf("绑定 %q 应被拒绝", addr)
		}
	}
}

func TestLoadAgent_TLSNoneRequiresLoopback(t *testing.T) {
	withIfaces(t, map[string][]net.IP{"tailscale0": {net.ParseIP("100.64.0.2")}})
	t.Setenv("DD_AGENT_TOKEN", "tok")
	if _, err := LoadAgent([]string{"-listen", "100.64.0.2:9527", "-tls", "none"}); err == nil {
		t.Fatal("明文模式绑定非回环应被拒绝")
	}
	// 回环 + none 放行。
	if _, err := LoadAgent([]string{"-listen", "127.0.0.1:9527", "-tls", "none"}); err != nil {
		t.Errorf("回环 + none 应放行，got %v", err)
	}
}

func TestLoadAgent_RejectsUnknownTLSMode(t *testing.T) {
	t.Setenv("DD_AGENT_TOKEN", "tok")
	if _, err := LoadAgent([]string{"-tls", "weird"}); err == nil {
		t.Fatal("非法 TLS 模式应报错")
	}
}

func TestLoadAgent_AllowsLoopbackEvenWithoutIface(t *testing.T) {
	withIfaces(t, map[string][]net.IP{})
	t.Setenv("DD_AGENT_TOKEN", "tok")
	if _, err := LoadAgent([]string{"-listen", "127.0.0.1:9527"}); err != nil {
		t.Errorf("回环应始终放行，got %v", err)
	}
}

func TestLoadAgent_AllowsListedInterface(t *testing.T) {
	withIfaces(t, map[string][]net.IP{"tailscale0": {net.ParseIP("100.64.0.2")}})
	t.Setenv("DD_AGENT_TOKEN", "tok")
	c, err := LoadAgent([]string{"-listen", "100.64.0.2:9527"})
	if err != nil {
		t.Fatalf("白名单网卡上的地址应放行，got %v", err)
	}
	if c.ListenAddr != "100.64.0.2:9527" {
		t.Errorf("ListenAddr = %q", c.ListenAddr)
	}
}

// 非 tailscale 网络（WireGuard、ZeroTier、局域网）靠 --bind-ifaces 纳入，
// 这是把默认网卡从「协议要求」降为「默认值」的目的所在。
func TestLoadAgent_AllowsAlternateInterface(t *testing.T) {
	withIfaces(t, map[string][]net.IP{"wg0": {net.ParseIP("10.9.0.7")}})
	t.Setenv("DD_AGENT_TOKEN", "tok")
	if _, err := LoadAgent([]string{"-listen", "10.9.0.7:9527", "-bind-ifaces", "wg0"}); err != nil {
		t.Errorf("显式指定 wg0 后应放行，got %v", err)
	}
}

func TestLoadAgent_AllowsExtraCIDR(t *testing.T) {
	withIfaces(t, map[string][]net.IP{})
	t.Setenv("DD_AGENT_TOKEN", "tok")
	if _, err := LoadAgent([]string{"-listen", "192.168.1.50:9527", "-allow-cidr", "192.168.1.0/24"}); err != nil {
		t.Errorf("命中 allow-cidr 应放行，got %v", err)
	}
	if _, err := LoadAgent([]string{"-listen", "10.0.0.5:9527", "-allow-cidr", "192.168.1.0/24"}); err == nil {
		t.Error("未命中 CIDR 的地址应被拒绝")
	}
}

func TestLoadAgent_RejectsForeignAddress(t *testing.T) {
	withIfaces(t, map[string][]net.IP{"tailscale0": {net.ParseIP("100.64.0.2")}})
	t.Setenv("DD_AGENT_TOKEN", "tok")
	_, err := LoadAgent([]string{"-listen", "203.0.113.9:9527"})
	if err == nil {
		t.Fatal("不属于任何白名单的地址应被拒绝")
	}
	// 错误里必须带出可用的补救参数，否则用户不知道下一步怎么办。
	for _, hint := range []string{"--bind-ifaces", "--allow-cidr", "--allow-insecure-bind"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("错误信息应提示 %s，got %v", hint, err)
		}
	}
}

func TestLoadAgent_AllowInsecureBindBypasses(t *testing.T) {
	withIfaces(t, map[string][]net.IP{})
	t.Setenv("DD_AGENT_TOKEN", "tok")
	c, err := LoadAgent([]string{"-listen", "203.0.113.9:9527", "-allow-insecure-bind"})
	if err != nil {
		t.Fatalf("显式破例应放行，got %v", err)
	}
	if !c.AllowInsecureBind {
		t.Error("AllowInsecureBind 应置为 true，供启动时打告警")
	}
}

func TestLoadAgent_TokenFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.token")
	if err := os.WriteFile(p, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DD_AGENT_TOKEN", "")
	c, err := LoadAgent([]string{"-listen", "127.0.0.1:9527", "-token-file", p})
	if err != nil {
		t.Fatalf("从文件读 token 应成功，got %v", err)
	}
	if c.Token != "file-token" {
		t.Errorf("token 应去掉首尾空白，got %q", c.Token)
	}
}

func TestLoadAgent_EnvOverridesDefaults(t *testing.T) {
	withIfaces(t, map[string][]net.IP{"tailscale0": {net.ParseIP("100.64.0.2")}})
	t.Setenv("DD_AGENT_TOKEN", "env-token")
	t.Setenv("DD_AGENT_LISTEN", "100.64.0.2:9527")
	c, err := LoadAgent(nil)
	if err != nil {
		t.Fatalf("环境变量配置应可用，got %v", err)
	}
	if c.Token != "env-token" || c.ListenAddr != "100.64.0.2:9527" {
		t.Errorf("got token=%q addr=%q", c.Token, c.ListenAddr)
	}
	if c.TLSMode != TLSModeSelf {
		t.Errorf("默认应为 self，got %q", c.TLSMode)
	}
}

func TestSplitHostPort(t *testing.T) {
	host, port, err := SplitHostPort("100.64.0.2:9527")
	if err != nil || host != "100.64.0.2" || port != 9527 {
		t.Errorf("got %q %d %v", host, port, err)
	}
	if _, _, err := SplitHostPort("100.64.0.2"); err == nil {
		t.Error("缺端口应报错")
	}
	if _, _, err := SplitHostPort("100.64.0.2:notaport"); err == nil {
		t.Error("非数字端口应报错")
	}
}

func TestLoadAgent_DiskMounts(t *testing.T) {
	t.Setenv("DD_AGENT_TOKEN", "tok")
	c, err := LoadAgent([]string{"-disk-mounts", "/,/data"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.DiskMounts) != 2 || c.DiskMounts[0] != "/" || c.DiskMounts[1] != "/data" {
		t.Errorf("DiskMounts = %v，want [/, /data]", c.DiskMounts)
	}
	// 未指定时只统计根，不要把整机的挂载点全捞一遍。
	def, err := LoadAgent(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.DiskMounts) != 1 || def.DiskMounts[0] != "/" {
		t.Errorf("默认 DiskMounts = %v，want [/]", def.DiskMounts)
	}
}

// clearFsEnv 清空文件管理相关环境变量，避免宿主/CI 环境串味。
func clearFsEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DD_AGENT_FS_ROOTS", "DD_AGENT_FS_MAX_FILE", "DD_AGENT_FS_MAX_DEPTH",
		"DD_AGENT_FS_MAX_ENTRIES", "DD_AGENT_FS_DENY",
	} {
		t.Setenv(k, "")
	}
}

// 未配 --fs-root 时模块必须整体关闭（FsRoots 空 → cmd/agent 不注册路由），
// 但上限与黑名单仍要有默认值——它们是「万一配了根目录」时的兜底。
func TestLoadAgent_FsDefaults(t *testing.T) {
	clearFsEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "tok")
	c, err := LoadAgent(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.FsRoots) != 0 {
		t.Errorf("未配置时 FsRoots 应为空（模块关闭），got %v", c.FsRoots)
	}
	if c.FsMaxFile != DefaultFsMaxFile || c.FsMaxDepth != DefaultFsMaxDepth || c.FsMaxEntries != DefaultFsMaxEntries {
		t.Errorf("上限默认值不对：file=%d depth=%d entries=%d",
			c.FsMaxFile, c.FsMaxDepth, c.FsMaxEntries)
	}
	// 默认黑名单必须在：配置为空不能变成「谁都能读私钥」。
	for _, want := range []string{"id_rsa", "*.key", ".env", ".ssh", "master.key"} {
		if !slices.Contains(c.FsDeny, want) {
			t.Errorf("默认黑名单缺少 %q，got %v", want, c.FsDeny)
		}
	}
}

func TestLoadAgent_FsRootsAndLimits(t *testing.T) {
	clearFsEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "tok")
	c, err := LoadAgent([]string{
		"-fs-root", "/data/,/srv/www/",
		"-fs-max-file", "1048576",
		"-fs-max-depth", "3",
		"-fs-max-entries", "50",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 根目录要 Clean 到规范形态：前缀比对依赖它，留着尾斜杠会让边界判定出错。
	want := []string{"/data", "/srv/www"}
	if !slices.Equal(c.FsRoots, want) {
		t.Errorf("FsRoots = %v，want %v", c.FsRoots, want)
	}
	if c.FsMaxFile != 1048576 || c.FsMaxDepth != 3 || c.FsMaxEntries != 50 {
		t.Errorf("上限未生效：file=%d depth=%d entries=%d", c.FsMaxFile, c.FsMaxDepth, c.FsMaxEntries)
	}
}

func TestLoadAgent_FsRootsRequireAbsolutePath(t *testing.T) {
	clearFsEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "tok")
	for _, bad := range []string{"data", "./data", "../data", "data/sub"} {
		if _, err := LoadAgent([]string{"-fs-root", bad}); err == nil {
			t.Errorf("相对路径根 %q 应被拒绝（绝对路径是前缀比对的前提）", bad)
		}
	}
}

func TestLoadAgent_FsDenyOnlyAppends(t *testing.T) {
	clearFsEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "tok")
	c, err := LoadAgent([]string{"-fs-deny", "*.log,/var/lib/secret.txt"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"*.log", "/var/lib/secret.txt"} {
		if !slices.Contains(c.FsDeny, want) {
			t.Errorf("追加的 pattern %q 未生效，got %v", want, c.FsDeny)
		}
	}
	// 关键断言：追加配置不能把默认保护挤掉。
	for _, want := range []string{"id_rsa", "*.key", ".env", ".ssh"} {
		if !slices.Contains(c.FsDeny, want) {
			t.Errorf("追加后默认 pattern %q 丢失，got %v", want, c.FsDeny)
		}
	}
}

func TestLoadAgent_FsLimitsRejectNonPositive(t *testing.T) {
	clearFsEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "tok")
	cases := [][]string{
		{"-fs-max-file", "0"},
		{"-fs-max-file", "-1"},
		{"-fs-max-depth", "0"},
		{"-fs-max-entries", "-5"},
		{"-fs-max-entries", "abc"},
	}
	for _, args := range cases {
		if _, err := LoadAgent(args); err == nil {
			t.Errorf("%v 应被拒绝：上限必须为正整数", args)
		}
	}
}

func TestLoadAgent_FsEnv(t *testing.T) {
	clearFsEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "tok")
	t.Setenv("DD_AGENT_FS_ROOTS", "/srv/data, /srv/backup")
	t.Setenv("DD_AGENT_FS_MAX_FILE", "2048")
	t.Setenv("DD_AGENT_FS_MAX_DEPTH", "2")
	t.Setenv("DD_AGENT_FS_MAX_ENTRIES", "10")
	t.Setenv("DD_AGENT_FS_DENY", "*.bak")
	c, err := LoadAgent(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.FsRoots, []string{"/srv/data", "/srv/backup"}) {
		t.Errorf("FsRoots = %v", c.FsRoots)
	}
	if c.FsMaxFile != 2048 || c.FsMaxDepth != 2 || c.FsMaxEntries != 10 {
		t.Errorf("环境变量上限未生效：file=%d depth=%d entries=%d", c.FsMaxFile, c.FsMaxDepth, c.FsMaxEntries)
	}
	if !slices.Contains(c.FsDeny, "*.bak") || !slices.Contains(c.FsDeny, "id_rsa") {
		t.Errorf("FsDeny = %v，应含 *.bak 与默认 id_rsa", c.FsDeny)
	}
}

func TestLoadAgent_FsEnvRejectsBadLimit(t *testing.T) {
	clearFsEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "tok")
	t.Setenv("DD_AGENT_FS_MAX_FILE", "0")
	if _, err := LoadAgent(nil); err == nil {
		t.Error("环境变量给了非正上限应报错")
	}
}
