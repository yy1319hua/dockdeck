// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agenthttp

import (
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// S7.9：Agent 卷与网络接口，**只读两条**。
//
// 与镜像侧的两处差别，都有据：
//   - 无「自身守卫」：卷与网络不是容器，删掉也不影响 Agent 进程；
//     且本模块不提供任何写操作，没有需要拦的目标；
//   - 无参数：清单不过滤、不分页（宿主机上卷与网络的量级是几十条，
//     不是几千条），与容器侧 list 的 all 开关不同，这里不需要开关。
//
// 写操作（create/delete/prune/connect/disconnect）刻意不实现：删除卷即删数据，
// 与本仓库「第一要务是不把现有容器和数据搞坏」冲突。

// resourcesAPI 聚合卷与网络端点的依赖。
type resourcesAPI struct {
	dc *dockercli.Client
}

// NewResourcesAPI 构造。socket 为空时用默认 socket 路径（与 imagesAPI 同口径）。
func NewResourcesAPI(socket string) *resourcesAPI {
	if socket == "" {
		socket = dockercli.DefaultSocketPath
	}
	return &resourcesAPI{dc: dockercli.New(socket)}
}

// volumes 实现 GET /api/v1/volumes：数据卷清单（只读）。
func (a *resourcesAPI) volumes(w http.ResponseWriter, r *http.Request) {
	out, err := a.dc.ListVolumes(r.Context())
	if err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// networks 实现 GET /api/v1/networks：网络清单（只读）。
func (a *resourcesAPI) networks(w http.ResponseWriter, r *http.Request) {
	out, err := a.dc.ListNetworks(r.Context())
	if err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
