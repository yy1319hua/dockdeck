// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

// HeaderRequestID 是幂等键/关联键的规范头名，对应 ARCHITECTURE.md 的承诺：
// 「写操作全部幂等键 X-DD-Request-Id 落审计」。它横跨两段链路：
//   - 客户端 → Hub：可选带入站；缺省由 Hub 生成；响应头原样回显。
//   - Hub → Agent：agentcli 原样转发，Agent 的错误日志带它落行。
//
// 旧头 X-Request-Id 仅作入站兼容（历史客户端），不再作为规范名。
const HeaderRequestID = "X-DD-Request-Id"

// MaxRequestIDLen 是幂等键长度上限。审计列、响应头、转发头三处都容得下，
// 但没有理由放任客户端把任意长度塞进来。
const MaxRequestIDLen = 64

// ValidRequestID 校验幂等键形态：1..64 字符，限 [A-Za-z0-9._-]。
// 字符集是硬约束——这个值会原样进审计库、进响应头、转发给 Agent，
// 放进 CR/LF 等于给响应拆分开门；放进超长值是把审计库当垃圾场。
func ValidRequestID(s string) bool {
	if s == "" || len(s) > MaxRequestIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
