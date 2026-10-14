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
