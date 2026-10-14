// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 镜像接口的 Hub 侧客户端（S4.1）。路径与 Agent 侧路由一字不差：
// 查询串与请求体都参与 HMAC 签名（do/once 统一走 CanonicalPath 与
// SignRequest），这里只负责拼路径、本地校验与超时策略。
//
// 与容器写操作的两个差别：
//   - pull 是长操作（拉大镜像以分钟计），不能套用 5s 写超时——本文件
//     单独持有 ImagePullTimeout，并经 once 直调（写语义天然不重试，
//     不走 do 的读重试分级）；
//   - ref 字符集校验（ValidImageRef）在出门前完成，与容器 ID 同哲学：
//     该拒的请求不该出门。

package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// ImagePullTimeout 是 pull 的超时，与 dockercli.PullTimeout 同量级：
// 两层超时是「Hub→Agent」与「Agent→Engine」两跳，各管一段，
// 都按大镜像经内网拉取的现实量级取 10 分钟。
const ImagePullTimeout = 10 * time.Minute

// ImageList 拉取镜像清单。all 显式进查询串（同 ContainerList 口径：
// 不依赖「不带参数算 false」的隐式约定，查询串同时是签名路径的一部分）；
// dangling=true 传 "1"（Agent 侧只认 "1"，缺省即 false）。
func (c *Client) ImageList(ctx context.Context, all, dangling bool) ([]protocol.ImageSummary, error) {
	q := url.Values{}
	q.Set("all", strconv.FormatBool(all))
	if dangling {
		q.Set("dangling", "1")
	}
	out := []protocol.ImageSummary{}
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/images?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ImagePull 拉取镜像，返回 Agent 汇总的结果（信封内层）。
// ref 先过 ValidImageRef；流内业务失败由 Agent 映射成 502 信封错误
// 经 once 原样透传（*protocol.Error），调用方看到的口径与直连 Agent 一致。
func (c *Client) ImagePull(ctx context.Context, ref string) (*protocol.PullResult, error) {
	if !protocol.ValidImageRef(ref) {
		return nil, fmt.Errorf("agentcli: 非法镜像引用 %q", ref)
	}
	body, _ := json.Marshal(map[string]string{"ref": ref})
	timeout := c.PullTimeout
	if timeout == 0 {
		timeout = ImagePullTimeout
	}
	var res protocol.PullResult
	if _, err := c.once(ctx, http.MethodPost, "/api/v1/images/pull", body, &res, timeout); err != nil {
		return nil, normalize(err)
	}
	return &res, nil
}

// ImageDelete 删除镜像。ref 直拼路径：ValidImageRef 的字符集
// [a-zA-Z0-9_.:/@-] 在 URL path 里全合法，转义反让两侧签名口径劈叉。
func (c *Client) ImageDelete(ctx context.Context, ref string) error {
	if !protocol.ValidImageRef(ref) {
		return fmt.Errorf("agentcli: 非法镜像引用 %q", ref)
	}
	_, err := c.do(ctx, http.MethodDelete, "/api/v1/images/"+ref, nil, nil)
	return err
}

// ImagePrune 清理悬空镜像，返回 Agent 转发的 Engine 清理清单与回收空间。
func (c *Client) ImagePrune(ctx context.Context) (*protocol.PruneResult, error) {
	var res protocol.PruneResult
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/images/prune", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
