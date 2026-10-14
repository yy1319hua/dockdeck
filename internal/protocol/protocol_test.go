// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// allCodes 列出本包定义的全部错误码常量。
// 新增常量必须同步这里与 codeStatus，否则 TestStatusForCodeMappingComplete 会红。
var allCodes = []string{
	CodeBadRequest,
	CodeUnauthorized,
	CodeAgentSignatureInvalid,
	CodeForbidden,
	CodeNotFound,
	CodeConflict,
	CodeRateLimited,
	CodeTooLarge,
	CodePathEscape,
	CodePathDenied,
	CodeUpstreamUnavailable,
	CodeUpstreamError,
	CodeInternal,
}

func TestStatusForCode(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{CodeBadRequest, http.StatusBadRequest},
		{CodeUnauthorized, http.StatusUnauthorized},
		{CodeAgentSignatureInvalid, http.StatusUnauthorized},
		{CodeForbidden, http.StatusForbidden},
		{CodeNotFound, http.StatusNotFound},
		{CodeConflict, http.StatusConflict},
		{CodeRateLimited, http.StatusTooManyRequests},
		{CodeTooLarge, http.StatusRequestEntityTooLarge},
		{CodePathEscape, http.StatusBadRequest},
		{CodePathDenied, http.StatusForbidden},
		{CodeUpstreamUnavailable, http.StatusBadGateway},
		{CodeUpstreamError, http.StatusBadGateway},
		{CodeInternal, http.StatusInternalServerError},
		// 未知码落到 500：宁可按服务端故障处理，也不要伪装成客户端错误。
		{"totally_unknown_code", http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := StatusForCode(c.code); got != c.want {
			t.Errorf("StatusForCode(%q) = %d，want %d", c.code, got, c.want)
		}
	}
}

func TestStatusForCodeMappingComplete(t *testing.T) {
	if len(codeStatus) != len(allCodes) {
		t.Errorf("映射表 %d 项、常量 %d 项：新增错误码必须同步 codeStatus", len(codeStatus), len(allCodes))
	}
	for _, c := range allCodes {
		if _, ok := codeStatus[c]; !ok {
			t.Errorf("错误码 %q 缺少 HTTP 状态映射", c)
		}
	}
}

func TestErrorIsJSONSerializable(t *testing.T) {
	e := NewError(CodeUpstreamUnavailable, "agent 不可达")
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if raw["code"] != CodeUpstreamUnavailable || raw["message"] != "agent 不可达" {
		t.Errorf("序列化结果不符：%s", b)
	}
}

func TestErrorString(t *testing.T) {
	if got := NewError(CodeNotFound, "").Error(); got != CodeNotFound {
		t.Errorf("message 为空时应只返回 code，got %q", got)
	}
	if got := NewError(CodeNotFound, "server 7").Error(); got != "not_found: server 7" {
		t.Errorf("got %q", got)
	}
	var nilErr *Error
	if got := nilErr.Error(); got != "" {
		t.Errorf("nil Error 应返回空串，got %q", got)
	}
}

// requireKeys 断言 JSON 对象里含有这些字段，用于锁住 snake_case 命名。
func requireKeys(t *testing.T, b []byte, keys []string) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("不是 JSON 对象：%v", err)
	}
	for _, k := range keys {
		if _, ok := raw[k]; !ok {
			t.Errorf("缺少字段 %q（实际：%+v）", k, keysOf(raw))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestHostMetricsRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := HostMetrics{
		CollectedAt: now,
		CPU: CPUStat{
			Cores: 4, UsagePercent: 12.5,
			User: 1000, System: 500, IOWait: 20, Steal: 0, Idle: 8000,
		},
		Memory: MemoryStat{
			Total: 8 << 30, Used: 3 << 30, Available: 5 << 30, Free: 4 << 30,
			Buffers: 1 << 20, Cached: 1 << 30, SwapTotal: 2 << 30, SwapUsed: 0,
		},
		Disks:   []DiskStat{{Mount: "/", FSType: "ext4", Total: 1000, Used: 400, Free: 600, UsedPercent: 40}},
		Network: []NetStat{{Interface: "eth0", RxBytes: 1234, TxBytes: 5678, RxPackets: 10, TxPackets: 11, RxErrors: 0, TxErrors: 1}},
		Load:    LoadStat{One: 0.5, Five: 0.4, Fifteen: 0.3},
		// Processes 与 UptimeSec 来自 /proc 计数与 /proc/uptime。
		Processes: 210,
		UptimeSec: 86400,
	}

	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	requireKeys(t, b, []string{"collected_at", "cpu", "memory", "disks", "network", "load", "processes", "uptime_sec"})

	var got HostMetrics
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if !got.CollectedAt.Equal(want.CollectedAt) {
		t.Errorf("collected_at 往返不一致：got %v，want %v", got.CollectedAt, want.CollectedAt)
	}
	if got.CPU != want.CPU {
		t.Errorf("cpu 往返不一致：got %+v，want %+v", got.CPU, want.CPU)
	}
	if got.Memory != want.Memory {
		t.Errorf("memory 往返不一致：got %+v，want %+v", got.Memory, want.Memory)
	}
	if got.Load != want.Load {
		t.Errorf("load 往返不一致：got %+v，want %+v", got.Load, want.Load)
	}
	if got.Processes != want.Processes || got.UptimeSec != want.UptimeSec {
		t.Errorf("processes/uptime_sec 往返不一致：got %d/%d", got.Processes, got.UptimeSec)
	}
	if !reflect.DeepEqual(got.Disks, want.Disks) {
		t.Errorf("disks 往返不一致：got %+v，want %+v", got.Disks, want.Disks)
	}
	if !reflect.DeepEqual(got.Network, want.Network) {
		t.Errorf("network 往返不一致：got %+v，want %+v", got.Network, want.Network)
	}
}

func TestCPUSnakeCaseKeys(t *testing.T) {
	b, err := json.Marshal(CPUStat{Cores: 2, UsagePercent: 1, User: 1, System: 1, IOWait: 1, Steal: 1, Idle: 1})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	requireKeys(t, b, []string{"cores", "usage_percent", "user", "system", "iowait", "steal", "idle"})
}

func TestAgentInfoRoundTrip(t *testing.T) {
	boot := time.Now().UTC().Truncate(time.Second).Add(-72 * time.Hour)
	want := AgentInfo{
		Hostname: "node-a", OS: "linux", Arch: "arm64", Kernel: "6.8.0",
		AgentVersion: "dev", ProtocolVersion: ProtocolVersion,
		Cores: 4, MemoryTotal: 8 << 30, BootTime: boot,
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	requireKeys(t, b, []string{"hostname", "os", "arch", "kernel", "agent_version", "protocol_version", "cores", "memory_total", "boot_time"})

	var got AgentInfo
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("往返不一致：got %+v，want %+v", got, want)
	}
}

func TestContainerRoundTrip(t *testing.T) {
	created := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	want := ContainerInfo{
		ID: "abc123", Name: "web", Image: "nginx:1.27", ImageID: "sha256:deadbeef",
		State: "running", Status: "Up 3 hours", CreatedAt: created,
		Labels: map[string]string{"com.example.role": "web"},
		Ports:  []PortBinding{{HostIP: "0.0.0.0", HostPort: "8080", ContainerPort: "80", Protocol: "tcp"}},
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	requireKeys(t, b, []string{"id", "name", "image", "image_id", "state", "status", "created_at", "labels", "ports"})

	var got ContainerInfo
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("往返不一致：got %+v，want %+v", got, want)
	}

	// 未设上限时 MemLimit 为 0，MemPercent 无意义——这里只保证它能往返，不做语义假设。
	stat := ContainerStat{ID: "abc123", Name: "web", CPUPercent: 3.25, MemUsage: 128 << 20, MemLimit: 0, MemPercent: 0, NetRx: 1, NetTx: 2, PIDS: 7}
	sb, err := json.Marshal(stat)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	requireKeys(t, sb, []string{"id", "name", "cpu_percent", "mem_usage", "mem_limit", "mem_percent", "net_rx", "net_tx", "pids"})
	var gotStat ContainerStat
	if err := json.Unmarshal(sb, &gotStat); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if gotStat != stat {
		t.Errorf("ContainerStat 往返不一致：got %+v", gotStat)
	}
}

func TestContainerInfoOmitsEmptyOptionalFields(t *testing.T) {
	b, err := json.Marshal(ContainerInfo{ID: "x", Name: "y"})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	for _, k := range []string{"labels", "ports"} {
		if _, ok := raw[k]; ok {
			t.Errorf("字段 %q 为空时不应出现在 JSON 里：%s", k, b)
		}
	}
}

func TestFileRoundTrip(t *testing.T) {
	mod := time.Now().UTC().Truncate(time.Second)
	entry := FileEntry{Name: "a.txt", Path: "/data/a.txt", IsDir: false, Size: 1024, Mode: "0644", ModTime: mod}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	requireKeys(t, b, []string{"name", "path", "is_dir", "size", "mode", "mod_time"})
	var gotEntry FileEntry
	if err := json.Unmarshal(b, &gotEntry); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if gotEntry != entry {
		t.Errorf("FileEntry 往返不一致：got %+v", gotEntry)
	}

	meta := FileMeta{Path: "/data/a.txt", Size: 4096, ContentType: "text/plain", ModTime: mod, Truncated: true}
	mb, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	requireKeys(t, mb, []string{"path", "size", "content_type", "mod_time", "truncated"})
	var gotMeta FileMeta
	if err := json.Unmarshal(mb, &gotMeta); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if gotMeta != meta {
		t.Errorf("FileMeta 往返不一致：got %+v", gotMeta)
	}

	// rename 带 dest，其余动作不带。
	act := FileAction{Action: ActionRename, Path: "/data/a.txt", Dest: "/data/b.txt"}
	ab, err := json.Marshal(act)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	requireKeys(t, ab, []string{"action", "path", "dest"})
	var gotAct FileAction
	if err := json.Unmarshal(ab, &gotAct); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if gotAct != act {
		t.Errorf("FileAction 往返不一致：got %+v", gotAct)
	}

	// mkdir 不应带出 dest。
	mb2, err := json.Marshal(FileAction{Action: ActionMkdir, Path: "/data/new"})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(mb2, &raw); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if _, ok := raw["dest"]; ok {
		t.Errorf("dest 为空时不应出现：%s", mb2)
	}
}

func TestValidFileAction(t *testing.T) {
	for _, a := range []string{ActionMkdir, ActionDelete, ActionRename} {
		if !ValidFileAction(a) {
			t.Errorf("%q 应判定为合法动作", a)
		}
	}
	// 大小写变形与近义动词一律拒绝，避免拼错却落到默认分支。
	for _, a := range []string{"", "rm", "MKDIR", "move", "delete "} {
		if ValidFileAction(a) {
			t.Errorf("%q 应判定为非法动作", a)
		}
	}
}

// TestValidContainerID 表驱动覆盖容器 ID 字符集：合法形态（hex ID、前缀、
// 名字）放行，注入与畸形形态本地就拒——这个校验是 Hub/Agent 两侧
// 拼 URL 路径前的第一道门，字符集单点定义在 protocol。
func TestValidContainerID(t *testing.T) {
	for _, id := range []string{"abc123def456", "abc", "my-container_1.2", "web", strings.Repeat("a", 128)} {
		if !ValidContainerID(id) {
			t.Errorf("%q 应判定为合法", id)
		}
	}
	for _, id := range []string{
		"", "..", ".", "a/b", "a b", "x?y=1", "../etc/passwd", "/web",
		"a\x00b", "容器", strings.Repeat("a", 129),
		"-dash", "_under", "...", // 首字符必须字母数字（Engine 规则 + 拒 dot-segment）
	} {
		if ValidContainerID(id) {
			t.Errorf("%q 应判定为非法", id)
		}
	}
}
