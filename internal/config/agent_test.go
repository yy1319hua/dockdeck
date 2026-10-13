// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package config

import (
	"net"
	"os"
	"path/filepath"
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
