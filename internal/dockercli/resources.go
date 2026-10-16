// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 本文件是卷与网络两个**只读** Engine 调用（S7.9）。
//
// 刻意不实现写方法（create/delete/prune/connect/disconnect）：
// 删除卷即删数据，prune 的 dangling 判定由 Engine 做出、误判不可逆——
// 与本仓库「第一要务是不把现有容器和数据搞坏」正面冲突。
// 需要清理时走宿主机 docker CLI（有人工判断在场），不经面板。

// ListVolumes 取数据卷清单（Engine GET /volumes）。
//
// 响应是**对象包数组**（{"Volumes":[...],"Warnings":...}），与 /networks 的
// 裸数组不同；两处解码模板不通用，勿相互抄。
//
// 不带 filters：只读页展示全部卷（含未被使用的），不做 dangling 过滤——
// 过滤是 prune 的预演，本页不做删除，故不需要那个参数。
func (c *Client) ListVolumes(ctx context.Context) ([]protocol.VolumeSummary, error) {
	var out protocol.VolumesList
	if err := c.getJSON(ctx, "/volumes", &out); err != nil {
		return nil, err
	}
	return out.Volumes, nil
}

// ListNetworks 取网络清单（Engine GET /networks），响应是**裸数组**。
//
// 每项自带 Containers 映射（接入容器名 + IPv4/Mac），故「哪些容器接入了」
// 不需要额外请求——这是与卷最大的形态差别，前端据此直接展示名单。
func (c *Client) ListNetworks(ctx context.Context) ([]protocol.NetworkSummary, error) {
	out := []protocol.NetworkSummary{}
	if err := c.getJSON(ctx, "/networks", &out); err != nil {
		return nil, err
	}
	return out, nil
}
