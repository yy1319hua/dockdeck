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
)

// ContainerSummary 对应 GET /containers/json 的条目，按需取字段。
// Names 是数组（一个容器可有多个名字），取用方自己决定取哪个；
// 本包不做「取第一个名字」这种裁剪——那属于语义猜测。
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
}

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
