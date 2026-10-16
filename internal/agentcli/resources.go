// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentcli

import (
	"context"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 本文件是卷与网络两个**只读**端点（S7.9）的 Hub→Agent 客户端。
//
// 只有读：写端点不存在，故这里也不提供——少一个方法就少一处误调。

// VolumeList 取 Agent 侧的数据卷清单（GET /api/v1/volumes）。
// 响应是裸数组（Agent 直接转发 dockercli.ListVolumes 的结果）。
func (c *Client) VolumeList(ctx context.Context) ([]protocol.VolumeSummary, error) {
	out := []protocol.VolumeSummary{}
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/volumes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NetworkList 取 Agent 侧的网络清单（GET /api/v1/networks）。
// 每项自带 Containers 映射，Hub 与前端都不需要再补请求。
func (c *Client) NetworkList(ctx context.Context) ([]protocol.NetworkSummary, error) {
	out := []protocol.NetworkSummary{}
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/networks", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
