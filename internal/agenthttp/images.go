// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.1：Agent 镜像接口。读（list）直接转发 dockercli；写（pull/delete/prune）
// 转发前由 dockercli 过 ValidImageRef（Hub 侧也会拒，Agent 是最后一道）。
//
// 与容器侧的两个差别，都有据：
//   - 无「自身镜像」守卫：Agent 的自身是容器（S3.2 已守），镜像没有
//     对应概念——删掉 Agent 镜像不影响运行中的 Agent 进程， Engine 对
//     「删正在被容器使用的镜像」自己会 409，不劳我们代劳；
//   - pull 的响应不是 ok:true 而是 PullResult：Engine 把拉取结果放在
//     流里（汇总式），HTTP 200 也可能业务失败，按结果分派状态码。

package agenthttp

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// imagesAPI 聚合镜像端点的依赖。
type imagesAPI struct {
	dc *dockercli.Client
}

// NewImagesAPI 构造。socket 为空时用默认 socket 路径。
func NewImagesAPI(socket string) *imagesAPI {
	if socket == "" {
		socket = dockercli.DefaultSocketPath
	}
	return &imagesAPI{dc: dockercli.New(socket)}
}

// list 实现 GET /api/v1/images?all=&dangling=。
// 口径与容器侧 list 同款：all 只认 "true"，dangling 只认 "1"——
// 拼错按缺省（false）处理而非报错，与 containersAPI.list 逐字对齐。
func (a *imagesAPI) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := a.dc.ListImages(r.Context(), q.Get("all") == "true", q.Get("dangling") == "1")
	if err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// pullBody 是 POST /api/v1/images/pull 的请求体。
type pullBody struct {
	Ref string `json:"ref"`
}

// pull 实现 POST /api/v1/images/pull，body {"ref":"alpine:3.20"}。
// ref 走 body 不走路径：含 "/" ":" "@" 的引用放路径段要靠尾通配符
// 兜底，body 没有这层歧义（delete 用路径是 ARCHITECTURE 定的，此处从 body）。
func (a *imagesAPI) pull(w http.ResponseWriter, r *http.Request) {
	var b pullBody
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		if len(raw) > 0 && json.Unmarshal(raw, &b) != nil {
			httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不是合法 JSON")
			return
		}
	}
	if b.Ref == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "ref 不能为空")
		return
	}
	res, err := a.dc.Pull(r.Context(), b.Ref)
	if err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	if res.Err != "" {
		// HTTP 200 但流内业务失败（如仓库不存在）。502=上游失败，
		// 文案带 Engine 原话——调用方看到的与 docker pull 控制台一致。
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "拉取失败: "+res.Err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// delete 实现 DELETE /api/v1/images/{ref...}。
// 尾通配符 {ref...}：镜像引用可含 "/"（library/nginx），单段 {ref}
// 匹配不到多段路径（Go mux 语义）。ref 的字符集由 ValidImageRef 在
// dockercli 层兜底，通配符不放大攻击面。
func (a *imagesAPI) delete(w http.ResponseWriter, r *http.Request) {
	writeAction(w, r, a.dc.Delete(r.Context(), r.PathValue("ref")))
}

// prune 实现 POST /api/v1/images/prune。
// dry-run 两段式的第二段：第一段（list?dangling=1 展示清单）是
// Hub/UI 层的事，到这里的请求就是真删。返回 Engine 的清理清单与回收空间。
func (a *imagesAPI) prune(w http.ResponseWriter, r *http.Request) {
	res, err := a.dc.Prune(r.Context())
	if err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
