// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import "context"

// Mount 是容器的一个挂载点（Engine /containers/json 的 Mounts 元素）。
//
// 只声明零挂载访问层用得到的字段：Type 区分 bind/volume/tmpfs，Source 是
// 宿主侧路径（bind 时是真实目录，volume 时是 Docker 管理的卷目录），
// Destination 是容器内路径，RW 表示容器内是否可写。
type Mount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// ContainerMounts 是「容器 + 挂载表」的内部投影。
//
// 不复用 protocol.ContainerSummary：后者是 /containers 的 API 契约，
// 给它加 Mounts 会把挂载信息塞进所有客户端响应；选取器只是服务端内部查询。
type ContainerMounts struct {
	ID     string
	Names  []string
	State  string
	Mounts []Mount
}

// ListMounts 列出全部容器（含已停止）及其挂载点。
//
// 刻意不复用 List：List 返回的 protocol.ContainerSummary 是给前端契约用的，
// 没有 Mounts 字段；为内部查询改公共结构，等于让每个客户端都为这个功能付费。
func (c *Client) ListMounts(ctx context.Context) ([]ContainerMounts, error) {
	var raw []struct {
		ID     string   `json:"Id"`
		Names  []string `json:"Names"`
		State  string   `json:"State"`
		Mounts []Mount  `json:"Mounts"`
	}
	if err := c.getJSON(ctx, "/containers/json?all=true", &raw); err != nil {
		return nil, err
	}
	out := make([]ContainerMounts, 0, len(raw))
	for _, r := range raw {
		out = append(out, ContainerMounts{
			ID:     r.ID,
			Names:  r.Names,
			State:  r.State,
			Mounts: r.Mounts,
		})
	}
	return out, nil
}
