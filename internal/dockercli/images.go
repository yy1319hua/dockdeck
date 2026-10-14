// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 镜像只读接口（S4.1 上半）：list。
// pull 是长写操作+流式响应（images_pull.go 单独落盘），
// delete/prune 是破坏性写（images_ops.go）——三分开与容器侧
// containers.go / operations.go 同哲学：只读与写在代码上泾渭分明。

package dockercli

import (
	"context"
	"net/url"
	"strconv"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// ImageSummary 是镜像清单条目的类型别名，真定义在 internal/protocol
// （与 ContainerSummary 同一理由：形状出现在 Hub↔Agent 线路上）。
type ImageSummary = protocol.ImageSummary

// ListImages 列出镜像。all=false 时只列顶层镜像（Engine 默认行为，
// 中间层不出现）；dangling=true 时只列无 tag 的悬空镜像——
// 它是 prune dry-run 的数据源（Hub/UI 先展示清单、确认后才发 prune）。
//
// filters 参数只此一个且值固定（{"dangling":["true"]}）：构造点在本函数内，
// 调用方只能传布尔开关，注入面为零。容器侧 List 刻意不做 filters
// （「不给攻击面留提前量」），这里破例是因为 prune dry-run 非得要它——
// 功能刚需，且固定值构造把风险关在笼子里。
func (c *Client) ListImages(ctx context.Context, all, dangling bool) ([]ImageSummary, error) {
	q := url.Values{}
	q.Set("all", strconv.FormatBool(all))
	if dangling {
		q.Set("filters", `{"dangling":["true"]}`)
	}
	out := []ImageSummary{}
	if err := c.getJSON(ctx, "/images/json?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out, nil
}
