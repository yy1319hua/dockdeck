// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import "time"

// Hub→Agent 请求头（线缆协议常量）。
//
// 这些头名与签名窗口是 Hub 侧与 Agent 侧必须一致遵守的「线缆契约」，
// 因此放在 protocol 层由两侧共用，而不是埋在某一方里——否则 Agent 的
// 反向连接（agentlink）要 import agenthttp、agenthttp 又要 import agentlink，
// 形成导入环。
const (
	// HeaderTimestamp 是 Unix 秒时间戳，参与签名并用于防重放。
	HeaderTimestamp = "X-DD-Timestamp"
	// HeaderSignature 是 HMAC-SHA256 的十六进制小写结果。
	HeaderSignature = "X-DD-Signature"
)

// MaxClockSkew 是允许的时间戳偏移。超出即判为重放或时钟异常。
const MaxClockSkew = 120 * time.Second
