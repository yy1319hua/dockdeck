// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import (
	"strings"
	"testing"
)

func TestValidRequestID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},                      // 空 = 缺省，由 Hub 生成，不算「带的键合法」
		{"a", true},                      // 单字符下限
		{"e2e-key-42", true},             // 常规形态
		{"A.b_c-d", true},                // 允许的标点
		{strings.Repeat("x", 64), true},  // 长度上限
		{strings.Repeat("x", 65), false}, // 超长
		{"bad key", false},               // 空格
		{"bad!", false},                  // 标点
		{"键", false},                     // 非 ASCII
		{"a\r\nX-Evil: 1", false},        // 响应拆分向量
		{"a\nb", false},                  // 裸换行
		{"tab\there", false},             // 制表符
		{"semi;colon", false},            // 分号
	}
	for _, tc := range cases {
		if got := ValidRequestID(tc.in); got != tc.want {
			t.Errorf("ValidRequestID(%q) = %v，want %v", tc.in, got, tc.want)
		}
	}
}
