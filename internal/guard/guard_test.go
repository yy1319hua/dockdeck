// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package guard

import (
	"strings"
	"testing"
)

func TestIsSelf(t *testing.T) {
	g, err := New("abc123def456")
	if err != nil {
		t.Fatal(err)
	}
	full64 := "abc123def4567890abcdef1234567890abcdef1234567890abcdef123456"
	for _, c := range []struct {
		target string
		want   bool
	}{
		{"abc123def456", true},
		{"ABC123DEF456", true},
		{full64, true},
		{"abc123def456_extra", true},
		{"abc123def", true}, // 反向前缀：保持 S3.2 isSelf 的双向语义
		{"", false},
		{"bbc123def456", false},
	} {
		if got := g.IsSelf(c.target); got != c.want {
			t.Errorf("IsSelf(%q) = %v，want %v", c.target, got, c.want)
		}
	}
	empty, _ := New("")
	if empty.IsSelf("anything") {
		t.Error("self 为空时不应命中")
	}
	var nilG *Guard
	if nilG.IsSelf("x") {
		t.Error("nil Guard 不应命中")
	}
}

func TestIsProtected(t *testing.T) {
	g, err := New("", `^dockdeck-`, `^watchtower$`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		want bool
	}{
		{"dockdeck-hub", true},
		{"dockdeck-agent-x", true},
		{"watchtower", true},
		{"WatchTower", false}, // 正则大小写敏感是本义，不偷偷加 (?i)
		{"my-watchtower", false},
		{"watchtower2", false},
		{"nginx", false},
		{"", false},
	} {
		if got := g.IsProtected(c.name); got != c.want {
			t.Errorf("IsProtected(%q) = %v，want %v", c.name, got, c.want)
		}
	}
	empty, _ := New("")
	if empty.IsProtected("dockdeck-hub") {
		t.Error("空名单不应命中")
	}
}

func TestNewBadPattern(t *testing.T) {
	if _, err := New("", "["); err == nil {
		t.Fatal("坏正则应报错")
	} else if !strings.Contains(err.Error(), "编译失败") {
		t.Errorf("错误文案应可读：%v", err)
	}
}

func TestConfirm(t *testing.T) {
	if err := Confirm("web", "web"); err != nil {
		t.Errorf("一致应放行：%v", err)
	}
	if err := Confirm("web", " web "); err != nil {
		t.Errorf("首尾空白应容忍：%v", err)
	}
	for _, bad := range []string{"", "WEB", "web2", "web x"} {
		if err := Confirm("web", bad); err == nil {
			t.Errorf("confirm=%q 应拒绝", bad)
		}
	}
}

func TestParseList(t *testing.T) {
	got := ParseList(` ^dockdeck- , ,,^watchtower$ `)
	if len(got) != 2 || got[0] != "^dockdeck-" || got[1] != "^watchtower$" {
		t.Errorf("ParseList 结果不符：%q", got)
	}
	if ParseList("") != nil {
		t.Error("空串应得 nil/空")
	}
}

// TestParseListJSON 锁 S7.8 起的 JSON 数组新格式与旧逗号数据兼容：
// 含逗号的正则（量词 {2,3}）在新格式下必须完整保留——旧格式按逗号切碎
// 正是本轮换存储格式的原因。
func TestParseListJSON(t *testing.T) {
	// 新格式：JSON 数组，含逗号正则不被切碎；空串与首尾空白元素被丢弃。
	got := ParseList(`["^dockdeck-", "^db-[0-9]{2,3}$", "", " ^watchtower$ "]`)
	if len(got) != 3 || got[0] != "^dockdeck-" || got[1] != "^db-[0-9]{2,3}$" || got[2] != "^watchtower$" {
		t.Errorf("JSON 数组解析不符：%q", got)
	}
	// 新格式里单条含逗号正则也完整保留（旧格式下它会被切成两段）。
	mixed := ParseList(`["^a{2,3}$"]`)
	if len(mixed) != 1 || mixed[0] != "^a{2,3}$" {
		t.Errorf("JSON 单条含逗号正则被切碎：%q", mixed)
	}
	// 空 JSON 数组 → 空名单（PUT 空数组即清除）。
	if empty := ParseList(`[]`); len(empty) != 0 {
		t.Errorf("空 JSON 数组应得空名单：%q", empty)
	}
	// 旧格式兼容：非 JSON 形态且含逗号 → 回落逗号分隔（S7.8 前存量数据）。
	old := ParseList("^a-,^b-")
	if len(old) != 2 || old[0] != "^a-" || old[1] != "^b-" {
		t.Errorf("旧逗号格式兼容解析不符：%q", old)
	}
	// 以 [ 开头但不是合法 JSON（如单条正则 [a-z]+ 存量）→ 回落整体一条，
	// 不被误判为 JSON 数组后吞掉。
	notJSON := ParseList("[a-z]+")
	if len(notJSON) != 1 || notJSON[0] != "[a-z]+" {
		t.Errorf("以 [ 开头的非 JSON 存量应整体保留：%q", notJSON)
	}
}
