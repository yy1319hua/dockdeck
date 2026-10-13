// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package protocol 定义 Hub 与 Agent 共享的数据传输对象（DTO）与统一错误码。
//
// Hub 与 Agent 是两个独立二进制，同属一个 module，靠本包共享结构定义，
// 避免两侧各写一份导致字段漂移——字段漂移正是本项目的核心风险之一。
//
// 约定：
//   - JSON 字段统一 snake_case；
//   - 时间统一 RFC3339（time.Time 在 encoding/json 下的默认格式）；
//   - 单位：字节用 byte，时长用 second，百分比是 0–100 的浮点数；
//   - 计数类指标（网络流量、CPU jiffies）只报累计值，速率由 Hub 按两次快照的时间差推算，
//     这样 Agent 保持无状态、可随意重装。
//
// 信封：Hub 对外与 Hub↔Agent 之间都使用 {"data": ..., "error": {code, message}}，
// 该结构由 internal/httpx 提供，本包不重复定义，以免 Agent 侧被迫依赖 Hub 专属的 HTTP 封装。
package protocol

// ProtocolVersion 是 Hub 与 Agent 之间的协议版本。
//
// Agent 的 /info 会带上它。Hub 遇到不认识的主版本应拒绝纳管并给出明确提示，
// 而不是带着字段歧义继续采集——宁可不显示，也不要显示错的。
const ProtocolVersion = "1"
