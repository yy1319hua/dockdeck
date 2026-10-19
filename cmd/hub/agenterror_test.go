// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import "testing"

// TestHumanizeAgentMessage 回归 v0.2.2 #6：剥包名前缀、去相邻重复段。
func TestHumanizeAgentMessage(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fsapi: 目录非空: /data/x", "目录非空: /data/x"},
		{"compose up 失败: dockercli: compose up 失败: exec: \"docker\" 不可执行", "compose up 失败: \"docker\" 不可执行"},
		{"dockercli: 容器 web 不存在 (404)", "容器 web 不存在 (404)"},
		{"正常的错误，无前缀", "正常的错误，无前缀"},
		{"a: a: a", "a"},
	}
	for _, c := range cases {
		if got := humanizeAgentMessage(c.in); got != c.want {
			t.Errorf("humanize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
