// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 容器写操作（S3.2 下半）。红线映射：
//   - 每个方法只针对单个容器，本包不提供任何批量入口；
//   - ID 前缀防注入：id 里的非法字符直接拒绝，不拼进 URL；
//   - remove 是破坏性动作，调用方（Agent 层）负责保护名单与名字确认，
//     本包只保证「不擅自加 force/v 参数」——由调用方显式传入；
//   - 不改 restart policy、不 exec、不 update——那些不在 S3.2 清单里。

package dockercli

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// idSafe 校验容器 ID/名字的合法字符。Engine 接受 hex ID、ID 前缀与名字
// （名字含 [a-zA-Z0-9][a-zA-Z0-9_.-]）；其余一律拒，杜绝路径拼接注入。
func idSafe(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		ch := id[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '_' || ch == '.' || ch == '-':
		default:
			return false
		}
	}
	return true
}

// action 对容器执行无请求体的生命周期动作：start/stop/restart/pause/unpause。
// extraQuery 是可选的查询串（如 stop 的 t=超时秒），须以 ? 或空开头。
func (c *Client) action(ctx context.Context, method, action, id, extraQuery string) error {
	if !idSafe(id) {
		return fmt.Errorf("dockercli: 非法容器标识 %q", id)
	}
	path := "/containers/" + id + "/" + action + extraQuery
	status, body, _, err := c.do(ctx, method, path)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent, http.StatusNotModified:
		// 304 = 目标状态已达成（如 start 已运行的容器），按 Docker CLI 惯例视为成功。
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("dockercli: 容器 %s 不存在 (%d)", id, status)
	case http.StatusConflict:
		return fmt.Errorf("dockercli: 容器 %s 状态不允许该操作 (%d): %.200s", id, status, body)
	default:
		return fmt.Errorf("dockercli: %s %s 返回 %d: %.200s", action, id, status, body)
	}
}

// Start 启动容器。
func (c *Client) Start(ctx context.Context, id string) error {
	return c.action(ctx, http.MethodPost, "start", id, "")
}

// Stop 停止容器，timeoutSec 是 SIGTERM 后等待秒数（nil 用引擎默认 10s）。
func (c *Client) Stop(ctx context.Context, id string, timeoutSec *int) error {
	q := ""
	if timeoutSec != nil {
		q = "?t=" + strconv.Itoa(*timeoutSec)
	}
	return c.action(ctx, http.MethodPost, "stop", id, q)
}

// Restart 重启容器。
func (c *Client) Restart(ctx context.Context, id string) error {
	return c.action(ctx, http.MethodPost, "restart", id, "")
}

// Pause 暂停容器（冻结进程，不杀）。
func (c *Client) Pause(ctx context.Context, id string) error {
	return c.action(ctx, http.MethodPost, "pause", id, "")
}

// Unpause 恢复暂停的容器。
func (c *Client) Unpause(ctx context.Context, id string) error {
	return c.action(ctx, http.MethodPost, "unpause", id, "")
}

// Kill 向容器主进程发信号，signal 形如 SIGTERM/SIGKILL（空用引擎默认 SIGKILL）。
// 白名单校验：信号是攻击者可注入的字符串，不在已知集合内的直接拒绝。
func (c *Client) Kill(ctx context.Context, id, signal string) error {
	if signal == "" {
		return c.action(ctx, http.MethodPost, "kill", id, "")
	}
	switch strings.ToUpper(signal) {
	case "SIGTERM", "SIGKILL", "SIGINT", "SIGHUP", "SIGQUIT", "SIGUSR1", "SIGUSR2":
	default:
		return fmt.Errorf("dockercli: 不允许的信号 %q", signal)
	}
	return c.action(ctx, http.MethodPost, "kill", id, "?signal="+strings.ToUpper(signal))
}

// Remove 删除容器。force 强删运行中的；v 连同匿名卷删除。
// 具名卷与 bind 目录不归 Engine 管，调用方走文件模块或显式 volume 删除。
func (c *Client) Remove(ctx context.Context, id string, force, volumes bool) error {
	if !idSafe(id) {
		return fmt.Errorf("dockercli: 非法容器标识 %q", id)
	}
	q := "?"
	if force {
		q += "force=1&"
	}
	if volumes {
		q += "v=1&"
	}
	q = strings.TrimSuffix(q, "&")
	status, body, _, err := c.do(ctx, http.MethodDelete, "/containers/"+id+"/"+q)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("dockercli: 容器 %s 不存在 (%d)", id, status)
	case http.StatusConflict:
		return fmt.Errorf("dockercli: 容器 %s 删除冲突 (%d): %.200s", id, status, body)
	default:
		return fmt.Errorf("dockercli: remove %s 返回 %d: %.200s", id, status, body)
	}
}

// Resize 调整 TTY 尺寸（配合日志/exec 终端用）。h/w 为行列数，须为正。
func (c *Client) Resize(ctx context.Context, id string, height, width int) error {
	if height <= 0 || width <= 0 {
		return fmt.Errorf("dockercli: resize 行列数必须为正 (%d,%d)", height, width)
	}
	return c.action(ctx, http.MethodPost, "resize", id,
		"?h="+strconv.Itoa(height)+"&w="+strconv.Itoa(width))
}
