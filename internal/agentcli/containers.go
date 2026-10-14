// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器接口的 Hub 侧客户端（S3.4）：上半读（list/inspect），下半写
//（start/stop/restart/pause/unpause/kill/resize/remove）——读写分开，
// 写不重试、不带批量入口，与 Agent 侧红线逐条对应。
//
// 路径与 Agent 侧路由一字不差：查询串（all/force/v/h/w）与请求体
//（timeout_sec/signal）都参与 HMAC 签名——do() 统一走
// agenthttp.CanonicalPath 与 SignRequest，这里只负责拼路径与本地校验，
// 不重复造签名口径。

package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

// ---- 写操作（S3.4）----
//
// 每个方法只针对单个容器，不提供任何批量入口（与 Agent 侧同一条红线）。
// 写策略由 do() 保证：POST/DELETE 不重试、5s 短超时——Agent 侧写操作
// 尚无幂等去重（X-DD-Request-Id 是独立后续步），盲目重试可能把一次
// stop 变成两次。本地校验（ID 字符集、信号白名单、行列正数）一律在
// 发请求之前完成：该拒的请求不该出门。

// lifecycleOpts 是 stop/kill 的可选请求体，字段按 Agent 侧 lifecycleBody 对齐。
// 只序列化非零字段：没有选项就不发请求体，不发 {"timeout_sec":0} 这种噪音。
type lifecycleOpts struct {
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	Signal     string `json:"signal,omitempty"`
}

// containerWrite 执行一次容器写请求。action 为空表示针对容器本身（DELETE remove）。
// ID 校验与路径拼接集中在这里，8 个写方法不各写一遍。
func (c *Client) containerWrite(ctx context.Context, method, action, id, query string, body []byte) error {
	if !protocol.ValidContainerID(id) {
		return fmt.Errorf("agentcli: 非法容器标识 %q", id)
	}
	path := "/api/v1/containers/" + id
	if action != "" {
		path += "/" + action
	}
	if query != "" {
		path += "?" + query
	}
	_, err := c.do(ctx, method, path, body, nil)
	return err
}

// ContainerStart 启动容器。
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.containerWrite(ctx, http.MethodPost, "start", id, "", nil)
}

// ContainerStop 停止容器。timeoutSec 是 SIGTERM 后等待秒数，
// nil 用引擎默认（10s）；走请求体，与 Agent 侧口径一致。
func (c *Client) ContainerStop(ctx context.Context, id string, timeoutSec *int) error {
	var body []byte
	if timeoutSec != nil {
		body, _ = json.Marshal(lifecycleOpts{TimeoutSec: *timeoutSec})
	}
	return c.containerWrite(ctx, http.MethodPost, "stop", id, "", body)
}

// ContainerRestart 重启容器。
func (c *Client) ContainerRestart(ctx context.Context, id string) error {
	return c.containerWrite(ctx, http.MethodPost, "restart", id, "", nil)
}

// ContainerPause 暂停容器（冻结进程，不杀）。
func (c *Client) ContainerPause(ctx context.Context, id string) error {
	return c.containerWrite(ctx, http.MethodPost, "pause", id, "", nil)
}

// ContainerUnpause 恢复暂停的容器。
func (c *Client) ContainerUnpause(ctx context.Context, id string) error {
	return c.containerWrite(ctx, http.MethodPost, "unpause", id, "", nil)
}

// ContainerKill 向容器主进程发信号。signal 形如 SIGTERM/SIGKILL
// （大小写不敏感，空用引擎默认 SIGKILL）；白名单在 protocol 单点定义，
// 非法信号本地就拒，不出请求。
func (c *Client) ContainerKill(ctx context.Context, id, signal string) error {
	norm, ok := protocol.NormalizeSignal(signal)
	if !ok {
		return fmt.Errorf("agentcli: 不允许的信号 %q", signal)
	}
	var body []byte
	if norm != "" {
		body, _ = json.Marshal(lifecycleOpts{Signal: norm})
	}
	return c.containerWrite(ctx, http.MethodPost, "kill", id, "", body)
}

// ContainerResize 调整 TTY 尺寸（配合日志/exec 终端用）。
// 行列数走查询串（Engine 原生约定），必须为正。
func (c *Client) ContainerResize(ctx context.Context, id string, height, width int) error {
	if height <= 0 || width <= 0 {
		return fmt.Errorf("agentcli: resize 行列数必须为正 (%d,%d)", height, width)
	}
	q := url.Values{}
	q.Set("h", strconv.Itoa(height))
	q.Set("w", strconv.Itoa(width))
	return c.containerWrite(ctx, http.MethodPost, "resize", id, q.Encode(), nil)
}

// ContainerRemove 删除容器。force 强删运行中的；volumes 连同匿名卷删除。
// 具名卷与 bind 目录不归 Engine 管，调用方走文件模块或显式 volume 删除。
func (c *Client) ContainerRemove(ctx context.Context, id string, force, volumes bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	if volumes {
		q.Set("v", "1")
	}
	return c.containerWrite(ctx, http.MethodDelete, "", id, q.Encode(), nil)
}
