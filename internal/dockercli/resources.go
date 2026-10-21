// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"log"
	"net/url"
	"sync"

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
// 接入容器名单（Containers 映射）**不在清单响应里**：Engine 29 起
// GET /networks 只给网络元信息，Containers 一律为 null；接入名单要逐个调
// GET /networks/{id} 详情端点才拿得到（实测 Engine 29.1.3：清单端点与
// ?verbose=1 变体都返回 null，详情端点才有 komari/komari-agent 等接入记录）。
// 旧实现在清单端点直接读 Containers，于是面板一律显示「未接入容器」——
// 连确有容器接入的 host 网络也误报。
//
// 故本实现两段走：清单拿元信息 + 逐个详情补 Containers。
// 详情请求并发执行（一个网络一次请求，数量级为个位数，无需限流）。
// 单个详情失败**不拖垮整次列举**——名单缺失只影响该行的展示，
// 网络元信息（名称/子网/驱动）仍要拿得到。
func (c *Client) ListNetworks(ctx context.Context) ([]protocol.NetworkSummary, error) {
	out := []protocol.NetworkSummary{}
	if err := c.getJSON(ctx, "/networks", &out); err != nil {
		return nil, err
	}
	c.fillNetworkContainers(ctx, out)
	return out, nil
}

// fillNetworkContainers 逐个取网络详情，补齐 Containers 映射。
// 并发执行，单个失败只记日志不中断（名单缺失不致命，元信息才是主线）。
func (c *Client) fillNetworkContainers(ctx context.Context, list []protocol.NetworkSummary) {
	if len(list) == 0 {
		return
	}
	var wg sync.WaitGroup
	for i := range list {
		if list[i].ID == "" {
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var detail protocol.NetworkSummary
			path := "/networks/" + url.PathEscape(list[idx].ID)
			if err := c.getJSON(ctx, path, &detail); err != nil {
				// 详情拿不到就保留清单里的原值（通常是 nil map，
				// 前端显示「未接入容器」——诚实反映「没取到」而非编造）。
				log.Printf("dockercli: 取网络 %s 详情失败，接入名单缺失: %v", list[idx].ID, err)
				return
			}
			list[idx].Containers = detail.Containers
		}(i)
	}
	wg.Wait()
}
