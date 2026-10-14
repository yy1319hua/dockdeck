// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import (
	"strings"
	"testing"
)

func TestValidImageRef(t *testing.T) {
	// 64 位小写 hex（digest 与全 ID 的合法形态）。
	hex64 := strings.Repeat("a", 63) + "f"
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		// ---- 合法形态 ----
		{"裸名字", "alpine", true},
		{"名字+tag", "alpine:3.20", true},
		{"官方库路径", "library/nginx", true},
		{"带端口 registry", "registry.example.com:5000/team/app:v1.2.3", true},
		{"多段路径+tag", "a/b/c/d:latest", true},
		{"大写与下划线点划", "My_App-1.0:rel_1.0", true},
		{"digest 形态", "alpine@sha256:" + hex64, true},
		{"全 ID", hex64, true},
		{"短 ID 12 位", "a1b2c3d4e5f6", true},
		{"单字符", "a", true},
		{"数字开头", "1nginx", true},
		{"255 字节上限内", "a" + strings.Repeat("b", 254), true},

		// ---- 注入与遍历 ----
		{"空串", "", false},
		{"点", ".", false},
		{"双点", "..", false},
		{"路径遍历前缀", "../etc/passwd", false},
		{"路径遍历中段", "a/../b", false},
		{"空路径段", "a//b", false},
		{"以斜杠开头", "/alpine", false},
		{"以斜杠结尾", "alpine/", false},
		{"以冒号开头", ":latest", false},
		{"以冒号结尾", "alpine:", false},
		{"以 at 结尾", "alpine@", false},

		// ---- 字符集 ----
		{"空格", "alpine latest", false},
		{"分号", "alpine;rm", false},
		{"换行注入", "alpine\nx", false},
		{"CR 注入", "alpine\rx", false},
		{"百分号编码", "alpine%2e%2e", false},
		{"问号", "alpine?x", false},
		{"井号", "alpine#x", false},
		{"与号", "a&b", false},
		{"反引号", "a`b", false},
		{"中文", "镜像", false},

		// ---- digest 形态强校验 ----
		{"裸 at", "alpine@x", false},
		{"digest 算法错", "alpine@sha512:" + hex64, false},
		{"digest 短一位", "alpine@sha256:" + strings.Repeat("a", 63), false},
		{"digest 长一位", "alpine@sha256:" + strings.Repeat("a", 65), false},
		{"digest 非 hex", "alpine@sha256:" + strings.Repeat("g", 64), false},
		{"digest 大写 hex", "alpine@sha256:" + strings.Repeat("A", 64), false},
		{"双 at", "alpine@sha256:" + hex64 + "@sha256:" + hex64, false},

		// ---- 长度 ----
		{"超 255 字节", "a" + strings.Repeat("b", 255), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidImageRef(tc.ref); got != tc.want {
				t.Errorf("ValidImageRef(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}
