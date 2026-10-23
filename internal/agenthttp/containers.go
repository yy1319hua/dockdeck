// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S3.2：Agent 容器接口。读转发给 dockercli；写转发前过「自我保护」守卫。
//
// 职责边界：保护名单与名字确认是 Hub/UI 层的三重门；Agent 只守最后一道
// 物理防线——绝不响应针对自身容器的写操作（红线「不碰自身」），
// 且每个操作只针对单个容器（本文件没有任何批量入口）。
//
// S4.4：守卫逻辑迁入 internal/guard（比较语义两侧共享，杜绝漂移）；
// remove 追加名字确认——Agent 与 Hub 同守一道，直连 Agent 的调用方
// （排障时）也绕不过确认门。

package agenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/fsapi"
	"github.com/yy1319hua/dockdeck/internal/guard"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/procscan"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// containersAPI 聚合容器端点的依赖。
type containersAPI struct {
	dc *dockercli.Client
	// guard 是写操作守卫：self 识别（保护名单属 Hub 侧知识，Agent 不带）。
	guard *guard.Guard
	// fs 是文件管理守卫，删容器联动清理部署目录走它。未启用文件管理时为 nil。
	fs *fsapi.Guard
	// comp 是 compose 操作器，联动清理时查项目冲突状态。未启用 compose 时为 nil。
	comp dockercli.ComposeOperator
	// composeDirs 是已配置的 compose 扫描目录，部署目录必须落在其中（防止误删白名单外的目录）。
	composeDirs []string
}

// compose 标签键（与 dockercli 保持一致，取 Docker 标准键名）。
const (
	labelComposeProject = "com.docker.compose.project"
	labelComposeWorkDir = "com.docker.compose.project.working_dir"
)

// NewContainersAPI 构造。self 为空时取 hostname（容器内 HOSTNAME 即短 ID，
// Docker 默认行为），拿不到就留空——没有 docker socket 的环境写操作自然全失败。
// fs/comp/composeDirs 透传自 main：文件管理或 compose 未启用时对应为 nil/空，
// remove 内部按 nil 安全处理，不要求二者一定存在。
func NewContainersAPI(socket, self string, fs *fsapi.Guard, comp dockercli.ComposeOperator, composeDirs []string) *containersAPI {
	if socket == "" {
		socket = dockercli.DefaultSocketPath
	}
	if self == "" {
		h, err := os.Hostname()
		if err == nil {
			self = h
		}
	}
	g, _ := guard.New(self) // 无名单正则，New 不会失败
	return &containersAPI{dc: dockercli.New(socket), guard: g, fs: fs, comp: comp, composeDirs: composeDirs}
}

// guardWrite 是写操作的统一守卫。返回 true 表示已写响应，调用方直接 return。
func (a *containersAPI) guardWrite(w http.ResponseWriter, id string) bool {
	if a.guard.IsSelf(id) {
		httpx.WriteError(w, http.StatusForbidden, protocol.CodeForbidden, "拒绝操作 Agent 自身容器（不碰自身红线）")
		return true
	}
	return false
}

// errToStatus 把 dockercli 的错误映射到 HTTP 状态码。
// 文案里带 Engine 的原始片段（截断过），排查时能看到 Docker 说了什么。
// 引擎错误同时落日志，带上 X-DD-Request-Id 幂等键——Hub 审计里存着同一份值，
// 两端日志能对上同一次写操作（键由 Hub 转发而来，Agent 不信任它做任何决策）。
func errToStatus(r *http.Request, err error) (int, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "不存在 (404)"):
		return http.StatusNotFound, msg
	case strings.Contains(msg, "(409)"):
		return http.StatusConflict, msg
	case strings.Contains(msg, "非法容器标识") || strings.Contains(msg, "非法镜像引用") || strings.Contains(msg, "不允许的信号"):
		return http.StatusBadRequest, msg
	default:
		log.Printf("agent: 容器操作引擎错误 (%s=%q): %v", protocol.HeaderRequestID, r.Header.Get(protocol.HeaderRequestID), err)
		return http.StatusBadGateway, "Docker 引擎请求失败"
	}
}

// list 实现 GET /api/v1/containers?all=。
func (a *containersAPI) list(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "true"
	out, err := a.dc.List(r.Context(), all)
	if err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// inspect 实现 GET /api/v1/containers/{id}。Engine JSON 原样进信封的 data 层——
// 字段不增不减，但必须走信封：Hub 侧 agentcli 只认信封格式，
// 裸写响应体会让它解出空信封、静默返回零值（S3.4 修，此前违反自家契约）。
func (a *containersAPI) inspect(w http.ResponseWriter, r *http.Request) {
	raw, err := a.dc.Inspect(r.Context(), r.PathValue("id"))
	if err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, json.RawMessage(raw))
}

// processes 实现 GET /api/v1/containers/{id}/processes（#91 容器进程明细）。
//
// 从宿主 /proc 读取归属该容器的进程——前置是 Agent 以 pid:host 运行
// （部署侧由面板「添加服务器」生成的命令/片段带上 --pid host / pid: host）。
//
// 安全口径：只在能定位到容器（拿到完整 ID）时才读；定位不到直接 404，
// 绝不退化成「返回宿主全量进程」——归属过滤在 procscan.List 内按 cgroup 完成。
func (a *containersAPI) processes(w http.ResponseWriter, r *http.Request) {
	sum, ok := a.findContainer(r.Context(), r.PathValue("id"))
	if !ok || sum.ID == "" {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "容器不存在")
		return
	}
	procs, err := procscan.List(r.Context(), procscan.DefaultProcRoot, sum.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "读取宿主进程失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, procs)
}

// processDetail 实现 GET /api/v1/containers/{id}/processes/{pid}（#91 明细 v2，只读）。
//
// 只返回**归属该容器**的进程 exe/cwd；pid 不存在或不属于该容器统一 404——
// 不区分二者，避免借「是否存在」探测宿主进程。归属校验在 procscan.Detail 内完成。
func (a *containersAPI) processDetail(w http.ResponseWriter, r *http.Request) {
	sum, ok := a.findContainer(r.Context(), r.PathValue("id"))
	if !ok || sum.ID == "" {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "容器不存在")
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil || pid <= 0 {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "非法进程 PID")
		return
	}
	d, err := procscan.Detail(r.Context(), procscan.DefaultProcRoot, sum.ID, pid)
	if err != nil {
		if errors.Is(err, procscan.ErrProcessNotFound) || errors.Is(err, procscan.ErrNotOwned) {
			httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "进程不存在")
			return
		}
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "读取进程明细失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

// lifecycleBody 是 stop/kill 的可选请求体。
type lifecycleBody struct {
	// TimeoutSec 仅 stop：SIGTERM 后等待秒数，nil 用引擎默认。
	TimeoutSec *int `json:"timeout_sec"`
	// Signal 仅 kill：空用引擎默认 SIGKILL。
	Signal string `json:"signal"`
}

// action 实现 POST /api/v1/containers/{id}/{action}。
// action ∈ start|stop|restart|pause|unpause|kill|resize——白名单在路由层，
// 这里再验一次，双保险不依赖注册表。
func (a *containersAPI) action(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.guardWrite(w, id) {
		return
	}
	var b lifecycleBody
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &b); err != nil {
				httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不是合法 JSON")
				return
			}
		}
	}

	var err error
	switch r.PathValue("action") {
	case "start":
		err = a.dc.Start(r.Context(), id)
	case "stop":
		err = a.dc.Stop(r.Context(), id, b.TimeoutSec)
	case "restart":
		err = a.dc.Restart(r.Context(), id)
	case "pause":
		err = a.dc.Pause(r.Context(), id)
	case "unpause":
		err = a.dc.Unpause(r.Context(), id)
	case "kill":
		err = a.dc.Kill(r.Context(), id, b.Signal)
	case "resize":
		// resize 的行列数走查询串（Engine 原生约定）。
		h, _ := strconv.Atoi(r.URL.Query().Get("h"))
		wq, _ := strconv.Atoi(r.URL.Query().Get("w"))
		err = a.dc.Resize(r.Context(), id, h, wq)
	default:
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "未知操作")
		return
	}
	writeAction(w, r, err)
}

// removeBody 是 DELETE 的可选请求体（force/v/remove_image/remove_deploy_dir
// 也都可以走查询串，二者取并集）。
type removeBody struct {
	Force           bool `json:"force"`
	Volumes         bool `json:"volumes"`
	RemoveImage     bool `json:"remove_image"`
	RemoveDeployDir bool `json:"remove_deploy_dir"`
	// Confirm 是名字确认：须与目标 ID/名字完全一致（guard.Confirm 语义）。
	Confirm string `json:"confirm"`
}

// remove 实现 DELETE /api/v1/containers/{id}?force=&v=&remove_image=&remove_deploy_dir=。
//
// 删容器本身是主操作；remove_image / remove_deploy_dir 是「联动清理」——
// 仅当主操作成功后才执行，结果逐条收集进 ContainerRemoveResult.Messages 回显给用户。
// 引擎不感知的 bind 目录/具名卷清理不在这里——调用方走文件模块与显式卷删除。
func (a *containersAPI) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.guardWrite(w, id) {
		return
	}
	q := r.URL.Query()
	var b removeBody
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		if len(raw) > 0 && json.Unmarshal(raw, &b) != nil {
			httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不是合法 JSON")
			return
		}
	}
	// 破坏性确认：query 与 body 取并集（body 优先），语义同 force/v 的并集口径。
	confirm := b.Confirm
	if confirm == "" {
		confirm = q.Get("confirm")
	}
	if err := guard.Confirm(id, confirm); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	force := b.Force || q.Get("force") == "1"
	volumes := b.Volumes || q.Get("v") == "1"
	removeImage := b.RemoveImage || q.Get("remove_image") == "1"
	removeDeployDir := b.RemoveDeployDir || q.Get("remove_deploy_dir") == "1"

	// 先取容器信息（镜像引用 / compose 标签）——删完就查不到了，必须在主操作之前。
	sum, _ := a.findContainer(r.Context(), id)

	// 主操作优先：容器删不掉就直接报错，不做任何联动，避免「容器还在、目录已删」的半吊子状态。
	if err := a.dc.Remove(r.Context(), id, force, volumes); err != nil {
		code, msg := errToStatus(r, err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}

	res := protocol.ContainerRemoveResult{OK: true}
	var msgs []string
	if removeImage {
		msgs = append(msgs, a.removeImage(r, sum)...)
	}
	if removeDeployDir {
		msgs = append(msgs, a.removeDeployDir(r, sum)...)
	}
	res.Messages = msgs
	httpx.WriteJSON(w, http.StatusOK, res)
}

// findContainer 在容器清单里按 id（全 ID / 前缀 / 名字）定位一条，取它的镜像引用与
// compose 标签——联动清理需要这些信息，而删容器本身只认 id 字符串。
//
// 找不到时返回零值：联动清理本就是「尽力而为」，缺信息就跳过并提示，不阻断主操作。
func (a *containersAPI) findContainer(ctx context.Context, id string) (protocol.ContainerSummary, bool) {
	list, err := a.dc.List(ctx, true)
	if err != nil {
		return protocol.ContainerSummary{}, false
	}
	for _, c := range list {
		if c.ID == id || strings.HasPrefix(c.ID, id) {
			return c, true
		}
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == id {
				return c, true
			}
		}
	}
	return protocol.ContainerSummary{}, false
}

// removeImage 联动删除容器使用的镜像。镜像被其他容器引用（409）或无故不存在（404）
// 都按「跳过并提示」处理——强删是调用方确认链的事，这里不代劳。
func (a *containersAPI) removeImage(r *http.Request, sum protocol.ContainerSummary) []string {
	if sum.ID == "" {
		return []string{"未找到容器信息，已跳过镜像清理"}
	}
	// 优先用 ImageID（精确指向这个容器实际跑的镜像层），空了才退到 Image。
	ref := sum.ImageID
	if ref == "" {
		ref = sum.Image
	}
	if ref == "" {
		return []string{"容器无镜像引用，已跳过镜像清理"}
	}
	if err := a.dc.Delete(r.Context(), ref); err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "删除冲突 (409)"):
			return []string{fmt.Sprintf("镜像 %s 仍被其他容器引用，已跳过", ref)}
		case strings.Contains(msg, "不存在 (404)"):
			return []string{fmt.Sprintf("镜像 %s 不存在，已跳过", ref)}
		default:
			return []string{fmt.Sprintf("镜像 %s 删除失败：%s", ref, msg)}
		}
	}
	return []string{fmt.Sprintf("已删除镜像 %s", ref)}
}

// removeDeployDir 联动递归删除 compose 项目的部署目录（working_dir）。
//
// 安全闸门（任一不通过即跳过并提示）：
//  1. 文件管理已启用（fs != nil），否则没地方安全地删；
//  2. 目录落在文件管理白名单根内（fs.PathToRel 校验，且不是根自身）；
//  3. 目录落在已配置的 compose 扫描目录内，防止误删白名单外的目录；
//  4. 项目无路径冲突（comp.ResolveProject 给出 Conflict）。
func (a *containersAPI) removeDeployDir(r *http.Request, sum protocol.ContainerSummary) []string {
	if a.fs == nil {
		return []string{"文件管理未启用，已跳过部署目录清理"}
	}
	if sum.ID == "" {
		return []string{"未找到容器信息，已跳过部署目录清理"}
	}
	wd := sum.Labels[labelComposeWorkDir]
	if wd == "" {
		return []string{"容器不是 compose 项目，已跳过部署目录清理"}
	}
	rootIndex, rel, ok := a.fs.PathToRel(wd)
	if !ok {
		return []string{fmt.Sprintf("部署目录 %s 不在文件管理白名单内，已跳过", wd)}
	}
	if rel == "" {
		return []string{fmt.Sprintf("部署目录 %s 是白名单根，已跳过", wd)}
	}
	if !withinComposeDir(wd, a.composeDirs) {
		return []string{fmt.Sprintf("部署目录 %s 不在 compose 管理目录内，已跳过", wd)}
	}
	if name := sum.Labels[labelComposeProject]; name != "" && a.comp != nil {
		if p, ok := a.comp.ResolveProject(r.Context(), name); ok && p.Conflict {
			return []string{fmt.Sprintf("compose 项目 %s 存在路径冲突，已跳过部署目录清理", name)}
		}
	}
	if _, err := a.fs.DeleteRecursive(rootIndex, rel); err != nil {
		return []string{fmt.Sprintf("部署目录 %s 清理失败：%s", wd, err.Error())}
	}
	return []string{fmt.Sprintf("已删除部署目录 %s", wd)}
}

// withinComposeDir 判断 path 是否落在某个已配置 compose 扫描目录内或恰为它本身。
// 仅当 path 是某 compose 目录（含其子目录）时才放行，目录外的联动清理一律拒绝。
func withinComposeDir(path string, dirs []string) bool {
	for _, d := range dirs {
		if path == d || strings.HasPrefix(path, d+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// writeAction 统一写操作的成功/失败响应。带 r 是为了让引擎错误日志
// 落上 X-DD-Request-Id 幂等键（errToStatus 内使用）。
func writeAction(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	code, msg := errToStatus(r, err)
	httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
}
