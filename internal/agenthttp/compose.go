// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.2：Compose 端点（Agent 侧）。只做三件事：list 列举白名单内项目、
// action 执行白名单写动作、logs 流日志。所有宿主侧的 exec 都收在
// dockercli.ComposeOperator 接口后面（真实是 ComposeClient，测试换假实现）。
//
// 白名单由两个关卡守住：① ListProjects 只在配置的目录内递归发现；②
// ResolveProject 把项目名映射到白名单内真实路径，白名单外的名字（含 ../ 穿越）
// 一律 404。Agent 持有 ComposeDirs 配置，所以白名单判定发生在 Agent 这一层，
// 而非 Hub——Hub 只转发项目名，拿不到白名单外的路径去 exec。

package agenthttp

import (
	"context"
	"io"
	"log"
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

type composeAPI struct {
	op dockercli.ComposeOperator
}

// NewComposeAPI 用配置好的白名单目录装配真实客户端。
// self 是 Agent 自身容器标识（ID 或名字）：装配后 compose 侧即开启
// 「不 down/restart 承载自己的 compose 项目」自保护——Agent 停了就没有面板
// 可以用来救它，故这一层必须自己守住。
//
// socket 传入时额外接入「label 来源发现」：从宿主容器上的 compose label
// 反推项目目录，让运维不必手工把每个目录写进白名单。socket 为空则只
// 保留白名单来源（行为与既有版本一致）。
func NewComposeAPI(dirs []string, self string, socket string) *composeAPI {
	if socket == "" {
		return &composeAPI{op: dockercli.NewComposeClientWithSelf(dirs, self)}
	}
	dc := dockercli.New(socket)
	labelDirs := func(ctx context.Context) ([]string, error) {
		// 只列运行中的容器：已停止容器的 label 仍指向旧目录，
		// 纳入后会把已废弃的部署目录重新拉回清单。
		containers, err := dc.List(ctx, false)
		if err != nil {
			return nil, err
		}
		return dockercli.ComposeDirsFromLabels(containers), nil
	}
	return &composeAPI{op: dockercli.NewComposeClientWithLabelDirs(dirs, self, labelDirs)}
}

// list 实现 GET /api/v1/compose。
func (a *composeAPI) list(w http.ResponseWriter, r *http.Request) {
	projects, err := a.op.ListProjects(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeUpstreamError, "列举 compose 项目失败: "+err.Error())
		return
	}
	if projects == nil {
		projects = []protocol.ComposeProject{}
	}
	httpx.WriteJSON(w, http.StatusOK, projects)
}

// action 实现 POST /api/v1/compose/{project}/{action}。
func (a *composeAPI) action(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	action := r.PathValue("action")
	if !protocol.ValidComposeAction(action) {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "不支持的 compose 动作: "+action)
		return
	}
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	var (
		out string
		err error
	)
	switch protocol.ComposeAction(action) {
	case protocol.ComposeUp:
		out, err = a.op.Up(r.Context(), p.ConfigFile)
	case protocol.ComposeDown:
		out, err = a.op.Down(r.Context(), p.ConfigFile)
	case protocol.ComposeRestart:
		out, err = a.op.Restart(r.Context(), p.ConfigFile)
	case protocol.ComposePull:
		out, err = a.op.Pull(r.Context(), p.ConfigFile)
	}
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "compose "+action+" 失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeActionResult{Output: out})
}

// logs 实现 GET /api/v1/compose/{project}/logs?follow=1。
// 沿用容器日志的流通道：逐帧 Flush，客户端断开即结束。
func (a *composeAPI) logs(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	follow := r.URL.Query().Get("follow") == "1"
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	rc, err := a.op.Logs(r.Context(), p.ConfigFile, follow)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "打开 compose 日志流失败: "+err.Error())
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	if _, err := io.Copy(flushWriter{w, flusher}, rc); err != nil {
		// 流中途断掉：状态码已发出，只能记日志。前端以连接关闭为结束信号。
		log.Printf("agent: compose 日志流中断 (%s=%q): %v", protocol.HeaderRequestID, r.Header.Get(protocol.HeaderRequestID), err)
	}
}

// fileGet 实现 GET /api/v1/compose/{project}/file：读取项目当前 compose 文件内容。
// 先 ResolveProject（白名单外/穿越一律 404），再由 op 读真实文件。
func (a *composeAPI) fileGet(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	content, err := a.op.ReadFile(r.Context(), p.ConfigFile)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "读取 compose 文件失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeFile{ConfigFile: p.ConfigFile, Content: content})
}

// fileDiff 实现 POST /api/v1/compose/{project}/diff：计算请求体内容相对当前文件的
// unified diff 供预览——不落盘、不产生备份。请求体 {"content": "..."}，走信封 JSON。
func (a *composeAPI) fileDiff(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	var req protocol.ComposeFileUpdate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不合法: "+err.Error())
		return
	}
	d, err := a.op.Diff(r.Context(), p.ConfigFile, req.Content)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "计算 compose diff 失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeFileDiff{Diff: d})
}

// filePut 实现 PUT /api/v1/compose/{project}/file：写前先备份 <file>.bak.<ts>，再覆盖写。
// 响应回传备份路径，供用户回滚。请求体 {"content": "..."}，走信封 JSON。
func (a *composeAPI) filePut(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	var req protocol.ComposeFileUpdate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不合法: "+err.Error())
		return
	}
	backup, err := a.op.WriteFile(r.Context(), p.ConfigFile, req.Content)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "写入 compose 文件失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeFileWritten{BackupFile: backup})
}

// backupsList 实现 GET /api/v1/compose/{project}/backups：列举该项目的写前备份。
// 只读——只回文件名/大小/时间，不回宿主机绝对路径（Q4-A）。
func (a *composeAPI) backupsList(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	list, err := a.op.ListBackups(r.Context(), p.ConfigFile)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "列举 compose 备份失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeBackupsList{Backups: list})
}

// backupsContent 实现 GET /api/v1/compose/{project}/backups/content?file=<备份名>：
// 读备份正文，供前端预览与 diff。备份名走查询串而非路径段——它含 RFC3339 的
// "+" 与 ":"（如 +08:00），放进路径段要处理编码差异（S7.5 已踩过同类坑）。
func (a *composeAPI) backupsContent(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "缺少 file 参数（备份文件名）")
		return
	}
	content, err := a.op.ReadBackup(r.Context(), p.ConfigFile, name)
	if err != nil {
		// 名字不合法（不以 <compose 文件>.bak. 开头 / 越界）与读失败都归 400：
		// 这两种都是调用方给错了名字，不是上游故障。
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeBackupContent{Name: name, Content: content})
}

// backupsRestore 实现 POST /api/v1/compose/{project}/backups/restore：用备份覆盖当前文件。
// 请求体 {"file": "<备份名>"}。恢复由 op 侧先备份「恢复前的当前内容」再覆盖（Q2-A），
// 响应回传那份新备份的文件名——有了它，恢复之后还能再回到恢复前的状态。
func (a *composeAPI) backupsRestore(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	p, ok := a.op.ResolveProject(r.Context(), project)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "compose 项目不存在或不在白名单: "+project)
		return
	}
	var req protocol.ComposeBackupUpdate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不合法: "+err.Error())
		return
	}
	if req.File == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "缺少 file 字段（备份文件名）")
		return
	}
	backup, err := a.op.RestoreBackup(r.Context(), p.ConfigFile, req.File)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeBackupRestored{Backup: backup})
}
