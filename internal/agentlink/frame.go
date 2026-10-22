// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package agentlink 实现 Agent 主动连 Hub 的反向长连接：
// 帧编解码、连接注册表、请求多路复用。
//
// 设计约束：
//   - 连接始终由 Agent 主动拨出（wss），Hub 从不反连——这是 NAT 后 Agent 可用的前提；
//   - 一条连接上多路复用多个请求，用 ID 区分；
//   - M1 只承载「请求 → 完整响应」，流式帧（resp_start/data/end）是 M2 预留。
package agentlink

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion 是帧协议主版本。只有不兼容变更才递增。
const ProtocolVersion = 1

// Body 上限，与 HTTP 路径保持同一口径：
//   - 请求 1 MiB = agenthttp.DefaultMaxBodyBytes（验签需整包，不能无限放大内存）
//   - 响应 8 MiB = agentcli 读取完整响应时的 LimitReader
//
// 超限直接报错、不截断：截断会让调用方拿到「看起来成功但内容不全」的结果，
// 比明确失败更难排查。
const (
	MaxReqBody  = 1 << 20
	MaxRespBody = 8 << 20
	// MaxChunkedBody 是分块请求体允许的最大总量（超出拒绝）。比 MaxReqBody 大得多，
	// 但仍有上限：避免恶意/异常客户端无限灌内存。单块大小远小于此，见 chunkSize。
	MaxChunkedBody = 32 << 20 // 32 MiB
)

// Type 是帧类型。
type Type string

const (
	TypeHello   Type = "hello"    // Agent → Hub：连接就绪，声明版本与能力
	TypeHelloOK Type = "hello_ok" // Hub → Agent：协商结果
	TypeReq     Type = "req"      // Hub → Agent：一个 HTTP 请求
	TypeResp    Type = "resp"     // Agent → Hub：完整响应（M1）
	TypeErr     Type = "err"      // 双向：请求级（ID≠0）或连接级（ID=0）错误
	TypeCancel  Type = "cancel"   // Hub → Agent：取消某请求
	TypePing    Type = "ping"     // 双向：心跳
	TypePong    Type = "pong"
	TypeGoAway  Type = "goaway" // Hub → Agent：顶号或关停，Agent 应断开（M1 不重连）

	// M2 流式预留：M1 收到这些类型一律回 err unsupported。
	TypeRespStart Type = "resp_start"
	TypeData      Type = "data"
	TypeEnd       Type = "end"
	TypeReqEnd    Type = "req_end"
)

// 能力标识。M1 的 Agent 至少声明 buffered 与 ping。
const (
	CapBuffered   = "buffered"    // 支持完整响应帧
	CapStream     = "stream"      // 支持 resp_start/data/end（M2）
	CapPing       = "ping"        // 支持心跳
	CapChunkedReq = "chunked_req" // 支持分块请求体（M2）
)

// Frame 是线上帧。
//
// B 用 []byte：encoding/json 会自动做 base64，因此不需要额外的长度字段，
// len(B) 就是载荷长度（协议文档里的 bl 即由此推出）。
type Frame struct {
	T    Type              `json:"t"`
	ID   uint64            `json:"id,omitempty"`
	V    int               `json:"v,omitempty"`
	M    string            `json:"m,omitempty"` // HTTP method
	P    string            `json:"p,omitempty"` // RequestURI（含查询串，签名口径）
	H    map[string]string `json:"h,omitempty"` // 仅转发白名单头
	B    []byte            `json:"b,omitempty"`
	S    bool              `json:"s,omitempty"`  // 流式标记（M2）
	St   int               `json:"st,omitempty"` // HTTP 状态码
	Code string            `json:"code,omitempty"`
	Msg  string            `json:"msg,omitempty"`
	Caps []string          `json:"caps,omitempty"`
	Seq  uint64            `json:"seq,omitempty"`
	Host string            `json:"host,omitempty"` // hello 帧携带的 Agent 服务器名
}

// Encode 序列化并校验上限。
func Encode(f Frame) ([]byte, error) {
	if err := validate(f); err != nil {
		return nil, err
	}
	return json.Marshal(f)
}

// Decode 反序列化并校验。
func Decode(raw []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return Frame{}, fmt.Errorf("agentlink: 帧解析失败: %w", err)
	}
	if err := validate(f); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// HasCap 判断对方是否声明了某项能力。
func (f Frame) HasCap(cap string) bool {
	for _, c := range f.Caps {
		if c == cap {
			return true
		}
	}
	return false
}

func validate(f Frame) error {
	switch f.T {
	case TypeHello, TypeHelloOK, TypePing, TypePong, TypeGoAway, TypeCancel, TypeErr:
		// 无载荷约束。
	case TypeReq:
		if f.M == "" || f.P == "" {
			return fmt.Errorf("agentlink: req 帧缺少方法或路径")
		}
		if len(f.B) > MaxReqBody {
			return fmt.Errorf("agentlink: 请求体 %d 字节超过上限 %d", len(f.B), MaxReqBody)
		}
	case TypeResp:
		if len(f.B) > MaxRespBody {
			return fmt.Errorf("agentlink: 响应体 %d 字节超过上限 %d", len(f.B), MaxRespBody)
		}
	case TypeRespStart:
		// 流起始帧：只带状态码与响应头，无载荷约束。
	case TypeData:
		if len(f.B) > MaxRespBody {
			return fmt.Errorf("agentlink: 数据帧 %d 字节超过上限 %d", len(f.B), MaxRespBody)
		}
	case TypeEnd, TypeReqEnd:
		// 流结束帧：无载荷约束。
	case "":
		return fmt.Errorf("agentlink: 帧缺少类型")
	default:
		return fmt.Errorf("agentlink: 未知帧类型 %q", f.T)
	}
	return nil
}
