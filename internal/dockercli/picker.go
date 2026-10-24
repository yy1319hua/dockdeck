// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// ErrNoBorrower 表示没有任何容器的 bind 挂载覆盖目标路径——文件访问不可达。
var ErrNoBorrower = errors.New("dockercli: 没有容器挂载覆盖该路径")

// Borrow 是一次「借道」的落点：选中哪个容器、目标路径在容器内的位置。
type Borrow struct {
	ContainerID   string
	ContainerPath string
	// ReadOnly 表示命中的挂载在容器内是只读的（:ro）：写操作必须先拒绝，
	// 而不是让 docker cp 写进去再报错（那时可能已经落了半个文件）。
	ReadOnly bool
}

// PickBorrower 选一个 bind 挂载覆盖 hostPath 的容器。
//
// 选取规则（P2.3）：
//   - **只认 bind 挂载**：volume 的 Source 是 Docker 管理的宿主目录（/var/lib/docker/volumes/...），
//     与用户看到的项目路径不是同一套命名，拿它匹配只会制造误判；tmpfs 无宿主路径，直接排除。
//   - **最长 Source 前缀优先**：/opt 与 /opt/bbcc 同时命中时取更具体的那个，
//     否则父目录挂载会把所有子项目的访问都引到一个错误的容器。
//   - **running 优先于其它状态**：容器停着时 docker cp 仍可用，但 running 的文件系统
//     更可能是「正在用的那一份」。
//   - **同优先级按容器 ID 字典序**：同样输入永远选同一个容器，避免面板刷新一次
//     就换一个借道对象（排查时最怕这种漂移）。
//   - 没有任何 bind 挂载覆盖 → ErrNoBorrower，由调用方转成「不可达」提示。
func (c *Client) PickBorrower(ctx context.Context, hostPath string) (Borrow, error) {
	list, err := c.ListMounts(ctx)
	if err != nil {
		return Borrow{}, err
	}
	return PickFrom(list, hostPath)
}

// PickFrom 是纯函数版选取器，便于单测覆盖各种挂载组合（不依赖 Engine）。
func PickFrom(list []ContainerMounts, hostPath string) (Borrow, error) {
	hostPath = strings.TrimSpace(hostPath)
	if !path.IsAbs(hostPath) {
		return Borrow{}, fmt.Errorf("dockercli: 借道路径必须是绝对路径，得到 %q", hostPath)
	}
	hostPath = path.Clean(hostPath)

	type candidate struct {
		borrow  Borrow
		srcLen  int
		running bool
		contID  string
	}
	var cands []candidate
	for _, c := range list {
		for _, m := range c.Mounts {
			// 空 Type 视为未知来源，不参与借道；只认 bind。
			if m.Type != "bind" || m.Source == "" || m.Destination == "" {
				continue
			}
			src := path.Clean(m.Source)
			if hostPath != src && !strings.HasPrefix(hostPath, src+"/") {
				continue
			}
			rel := strings.TrimPrefix(hostPath, src)
			cands = append(cands, candidate{
				borrow: Borrow{
					ContainerID:   c.ID,
					ContainerPath: path.Clean(path.Join(m.Destination, rel)),
					ReadOnly:      !m.RW,
				},
				srcLen:  len(src),
				running: c.State == "running",
				contID:  c.ID,
			})
		}
	}
	if len(cands) == 0 {
		return Borrow{}, ErrNoBorrower
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].running != cands[j].running {
			return cands[i].running
		}
		if cands[i].srcLen != cands[j].srcLen {
			return cands[i].srcLen > cands[j].srcLen
		}
		return cands[i].contID < cands[j].contID
	})
	return cands[0].borrow, nil
}
