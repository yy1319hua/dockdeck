// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器只读接口的 Hub 侧客户端（S3.4）。
//
// 路径与 Agent 侧路由一字不差：查询串（all）参与 HMAC 签名——
// do() 统一走 agenthttp.CanonicalPath（RequestURI 含查询串），这里只负责
// 拼路径与本地校验，不重复造签名口径。
//
// 写操作（ContainerAction/ContainerRemove）另见后续小步：写不重试、
// 不带批量入口，与读在代码上分开。

package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// ContainerList 拉取容器清单。all=true 含已停止的。
//
// all 显式进查询串而非缺省：Agent 侧只认字面 "true"，显式传值不依赖
// 「不带参数算 false」这种隐式约定——查询串同时是签名路径的一部分，
// 两侧看到的必须一致。
func (c *Client) ContainerList(ctx context.Context, all bool) ([]protocol.ContainerSummary, error) {
	out := []protocol.ContainerSummary{}
	path := "/api/v1/containers?all=" + strconv.FormatBool(all)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ContainerInspect 取容器详情，返回 Agent 在信封内层原样转发的 Engine JSON。
//
// Hub 不做结构化解读：inspect 字段几百个且随版本膨胀，裁剪即丢信息
// （「不猜语义」红线）；结构化解读是 UI 层按需做的事。
//
// ID 先过 protocol.ValidContainerID：它会拼进 URL 路径，不校验等于把
// 路径拼接入口敞开。Agent 侧也会拒，但 Hub 该在本地就挡住，不发这请求。
func (c *Client) ContainerInspect(ctx context.Context, id string) (json.RawMessage, error) {
	if !protocol.ValidContainerID(id) {
		return nil, fmt.Errorf("agentcli: 非法容器标识 %q", id)
	}
	var raw json.RawMessage
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/containers/"+id, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
