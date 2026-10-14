// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.2：Compose 动作校验的表驱动测试 + 变异验证。
// 变异哲学（同 S4.1）：撤掉白名单里任意一个分支，应有且仅有一个合法用例变红——
// 证明测试不是「总是绿」，而是真的钉住了每个允许的动词。

package protocol

import "testing"

func TestValidComposeAction(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"up", true},
		{"down", true},
		{"restart", true},
		{"pull", true},
		// 非法：logs 走独立 GET 流端点，不在此白名单；其余一律拒。
		{"logs", false},
		{"", false},
		{"UP", false}, // 大小写敏感
		{"rm", false}, // 危险动词绝不透传
		{"exec", false},
		{"restart;rm -rf", false}, // 注入
		{"../../etc", false},
		{"up ", false}, // 尾空格
	}
	for _, c := range cases {
		if got := ValidComposeAction(c.in); got != c.want {
			t.Errorf("ValidComposeAction(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
