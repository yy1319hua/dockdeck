// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 镜像破坏性写（S4.1 下半）：delete / prune。
//
// 红线映射：
//   - 每个方法单一目标，本文件没有任何批量删除入口；
//   - delete 无 force 参数——「强删运行中容器依赖的镜像」的 Docker 语义
//     是先处理容器再删镜像，那是调用方（Hub/UI 确认链）的决策，本包不代劳；
//   - prune 无 dry-run 参数——Engine 压根没有这个开关。两段式
//     （先 list?dangling=1 展示清单、确认后才发 prune）是 Hub/UI 层的事，
//     本包只保证「发出去的请求就是真删」；
//   - prune 的 filters 从简：只做 Engine 默认的 dangling。until/label
//     过滤等 UI 真要时再加，不给攻击面留提前量（同容器侧 List 的取舍）。

package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// PruneResult 对应 Engine POST /images/prune 的响应体。
type PruneResult struct {
	// ImagesDeleted 是清理清单。每条是 Deleted（镜像本体删掉）或
	// Untagged（只解了引用，多 tag 镜像的本体还活着）——UI 展示时
	// 两者都给用户看，「删了 3 个镜像」与「解了 2 个 tag」不是一回事。
	ImagesDeleted []PrunedImage `json:"ImagesDeleted"`
	// SpaceReclaimed 是回收的字节数。
	SpaceReclaimed int64 `json:"SpaceReclaimed"`
}

// PrunedImage 是 prune 清单的一条（Engine 原样两个字段二选一）。
type PrunedImage struct {
	Deleted  string `json:"Deleted"`
	Untagged string `json:"Untagged"`
}

// Delete 删除镜像。ref 形同 pull（name:tag 或 ID）。
//
// ref 直接拼 URL 路径段（不过 url.PathEscape）：ValidImageRef 的字符集
// 已排除 # ? 空格与控制字符，剩下的 [a-zA-Z0-9_.:/@-] 在 HTTP path 里
// 全是合法字符，转义反而会让 Engine 收到编码形态对不上。校验即防护，
// 与容器侧 Remove 同一哲学。
func (c *Client) Delete(ctx context.Context, ref string) error {
	if !protocol.ValidImageRef(ref) {
		return fmt.Errorf("dockercli: 非法镜像引用 %q", ref)
	}
	status, body, _, err := c.do(ctx, http.MethodDelete, "/images/"+ref)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		// 文案口径与容器侧逐字对齐：Agent 层 errToStatus 靠
		// 「不存在 (404)」这个子串映射 404，改文案等于改映射。
		return fmt.Errorf("dockercli: 镜像 %s 不存在 (404)", ref)
	case http.StatusConflict:
		// 镜像被容器占用（含已停止容器）。强删是调用方的决策链。
		return fmt.Errorf("dockercli: 镜像 %s 删除冲突 (%d): %.200s", ref, status, body)
	default:
		return fmt.Errorf("dockercli: delete %s 返回 %d: %.200s", ref, status, body)
	}
}

// Prune 清理悬空镜像（Engine 默认 dangling 口径），返回删除清单与回收空间。
func (c *Client) Prune(ctx context.Context) (*PruneResult, error) {
	status, body, _, err := c.do(ctx, http.MethodPost, "/images/prune")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("dockercli: prune 返回 %d: %.200s", status, body)
	}
	var res PruneResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("dockercli: prune 响应解码失败: %w", err)
	}
	return &res, nil
}
