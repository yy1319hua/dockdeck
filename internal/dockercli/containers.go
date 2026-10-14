// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器只读接口（S3.2 上半）：list / inspect。
// 写操作（start/stop/.../remove/resize）在 operations.go——读写分开落盘，
// 是为了让「只读转发」与「带守卫的写转发」在代码上泾渭分明。

package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// ContainerSummary 是容器清单条目的类型别名，真定义在 internal/protocol：
// 这个形状出现在 Hub↔Agent 线路上（Agent 原样转发 Engine 字段，Hub 解码），
// 两侧共用同一份定义，各自 struct 必有字段悄悄对不上的一天。
type ContainerSummary = protocol.ContainerSummary

// List 列出容器。all=true 含已停止的。
// 查询参数只有 all——filters 等高级功能等 UI 真需要时再加，不给攻击面留提前量。
func (c *Client) List(ctx context.Context, all bool) ([]ContainerSummary, error) {
	out := []ContainerSummary{}
	if err := c.getJSON(ctx, "/containers/json?all="+strconv.FormatBool(all), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Inspect 取容器详情（GET /containers/{id}/json）。
//
// 返回原始 JSON 而非结构体：inspect 的字段有几百个且随版本膨胀，
// Agent 的职责是转发不是裁剪——Typed 结构在这里只会制造「字段悄悄丢失」。
// 结构化解读是 Hub/UI 层按需做的事。
func (c *Client) Inspect(ctx context.Context, id string) (json.RawMessage, error) {
	if id == "" {
		return nil, fmt.Errorf("dockercli: 容器 ID 不能为空")
	}
	status, body, _, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/json")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("dockercli: inspect %s 返回 %d: %.200s", id, status, body)
	}
	return json.RawMessage(body), nil
}
