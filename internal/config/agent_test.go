// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package config

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// agentBaseEnv 置好「能通过校验」的最小环境：token + Hub 地址。
// v0.2.6 起 Agent 只有反向长连接一种形态，Validate 的硬前提就是这两样；
// 顺带清掉历史遗留的正向变量，证明它们已被彻底无视。
func agentBaseEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DD_AGENT_LISTEN", "DD_AGENT_CERT_DIR", "DD_AGENT_TLS_MODE",
		"DD_AGENT_BIND_IFACES", "DD_AGENT_ALLOW_CIDR", "DD_AGENT_ALLOW_INSECURE_BIND",
		"DD_AGENT_REVERSE", "DD_AGENT_JOIN_SECRET",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("DD_AGENT_TOKEN", "tok")
	t.Setenv("DD_HUB_URL", "https://hub.example:443")
}

func TestLoadAgent_RequiresToken(t *testing.T) {
	agentBaseEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "")
	if _, err := LoadAgent(nil); err == nil {
		t.Fatal("缺少 token 时应报错，不能退化成无鉴权")
	} else if !strings.Contains(err.Error(), "token") {
		t.Errorf("错误信息应提示 token，got %v", err)
	}
}

func TestLoadAgent_RequiresHubURL(t *testing.T) {
	agentBaseEnv(t)
	t.Setenv("DD_HUB_URL", "")
	_, err := LoadAgent(nil)
	if err == nil {
		t.Fatal("没有 Hub 地址时应报错：反向形态下连接目标是唯一命脉")
	}
	if !strings.Contains(err.Error(), "Hub 地址") {
		t.Errorf("错误信息应提示 Hub 地址，got %v", err)
	}
}

func TestLoadAgent_RejectsBadHubURL(t *testing.T) {
	agentBaseEnv(t)
	for _, bad := range []string{"hub.example:443", "ftp://hub.example", "reverse://x", "://hub"} {
		t.Setenv("DD_HUB_URL", bad)
		if _, err := LoadAgent(nil); err == nil {
			t.Errorf("Hub 地址 %q 应被拒绝", bad)
		}
	}
}

// 正向模式的旗标必须整个消失：残留一个可解析的 -listen，
// 就有人会照着旧文档填出一个静默失效的配置。
func TestLoadAgent_RejectsListenFlag(t *testing.T) {
	agentBaseEnv(t)
	if _, err := LoadAgent([]string{"-listen", "127.0.0.1:9527"}); err == nil {
		t.Fatal("-listen 应不再被识别（正向模式已删除）")
	}
}

func TestLoadAgent_RejectsReverseFlag(t *testing.T) {
	agentBaseEnv(t)
	if _, err := LoadAgent([]string{"-reverse"}); err == nil {
		t.Fatal("-reverse 应不再被识别：反向已是唯一形态，不需要开关")
	}
	// 遗留环境变量同样必须被无视，不能悄悄改行为。
	t.Setenv("DD_AGENT_REVERSE", "1")
	c, err := LoadAgent(nil)
	if err != nil {
		t.Fatalf("遗留 DD_AGENT_REVERSE 不应导致失败，got %v", err)
	}
	if c.HubURL != "https://hub.example:443" {
		t.Errorf("HubURL = %q", c.HubURL)
	}
}

func TestLoadAgent_TokenFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.token")
	if err := os.WriteFile(p, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agentBaseEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "")
	c, err := LoadAgent([]string{"-token-file", p})
	if err != nil {
		t.Fatalf("从文件读 token 应成功，got %v", err)
	}
	if c.Token != "file-token" {
		t.Errorf("token 应去掉首尾空白，got %q", c.Token)
	}
}

func TestLoadAgent_EnvOverridesDefaults(t *testing.T) {
	agentBaseEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "env-token")
	c, err := LoadAgent(nil)
	if err != nil {
		t.Fatalf("环境变量配置应可用，got %v", err)
	}
	if c.Token != "env-token" {
		t.Errorf("token = %q", c.Token)
	}
	if c.LinkPingInterval <= 0 || c.LinkMaxInflight <= 0 {
		t.Errorf("反向连接默认参数应有效：ping=%v inflight=%d", c.LinkPingInterval, c.LinkMaxInflight)
	}
}

// 只配邀请未配 token 时，join_secret 即本机长期共享令牌初值：
// 「只给一次性邀请」也要能启动（Hub 侧存的就是这把 secret 的密文）。
func TestLoadAgent_TokenDerivedFromJoinSecret(t *testing.T) {
	agentBaseEnv(t)
	t.Setenv("DD_AGENT_TOKEN", "")
	t.Setenv("DD_AGENT_JOIN_SECRET", "secret-123")
	c, err := LoadAgent(nil)
	if err != nil {
		t.Fatalf("只有 join_secret 时应能启动，got %v", err)
	}
	if c.Token != "secret-123" {
		t.Errorf("token 应派生自 join_secret，got %q", c.Token)
	}
}

func TestLoadAgent_DiskMounts(t *testing.T) {
	agentBaseEnv(t)
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
	agentBaseEnv(t)
	clearFsEnv(t)
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
	agentBaseEnv(t)
	clearFsEnv(t)
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
	agentBaseEnv(t)
	clearFsEnv(t)
	for _, bad := range []string{"data", "./data", "../data", "data/sub"} {
		if _, err := LoadAgent([]string{"-fs-root", bad}); err == nil {
			t.Errorf("相对路径根 %q 应被拒绝（绝对路径是前缀比对的前提）", bad)
		}
	}
}

func TestLoadAgent_FsDenyOnlyAppends(t *testing.T) {
	agentBaseEnv(t)
	clearFsEnv(t)
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
	agentBaseEnv(t)
	clearFsEnv(t)
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
	agentBaseEnv(t)
	clearFsEnv(t)
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
	agentBaseEnv(t)
	clearFsEnv(t)
	t.Setenv("DD_AGENT_FS_MAX_FILE", "0")
	if _, err := LoadAgent(nil); err == nil {
		t.Error("环境变量给了非正上限应报错")
	}
}

// 单文件上限不能超过 protocol 的共享常量：它同时是单请求内存占用的上界，
// 无界等于给 Agent 装了个内存放大器。
func TestLoadAgent_FsMaxFileHasHardCeiling(t *testing.T) {
	agentBaseEnv(t)
	clearFsEnv(t)
	over := strconv.FormatInt(protocol.MaxFileUploadBytes+1, 10)
	if _, err := LoadAgent([]string{"-fs-max-file", over}); err == nil {
		t.Fatal("超过共享上限应报错")
	}
	at := strconv.FormatInt(protocol.MaxFileUploadBytes, 10)
	c, err := LoadAgent([]string{"-fs-max-file", at})
	if err != nil {
		t.Fatalf("恰好等于上限应放行，got %v", err)
	}
	if c.FsMaxFile != protocol.MaxFileUploadBytes {
		t.Errorf("FsMaxFile = %d，want %d", c.FsMaxFile, protocol.MaxFileUploadBytes)
	}
}
