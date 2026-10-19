// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package fakeagent 提供一个 httptest 可用的假 Agent：真实的鉴权中间件 +
// 可配置的指标响应。用途是端到端测试 Hub→Agent 链路（agentcli ↔ 鉴权 ↔ 端点），
// 不 mock 鉴权本身——链路测试的价值就在于连签名头一起验。
package fakeagent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// logsFollowTicks 是 follow 模式下的心跳帧数：真 Agent 的 follow 是长连接，
// 假 Agent 用有限帧数把「流活着」这件事演出来（客户端断开即结束）。
const logsFollowTicks = 5

// logsFollowTickDelay 是心跳间隔。e2e 用它对抗「客户端擅自施加读超时」：
// 客户端若把 ReadTimeout 用在流上，第一帧之后就被掐断。
const logsFollowTickDelay = 30 * time.Millisecond

// Fake 是假 Agent。零值不可用，用 New 构造。
type Fake struct {
	Token string

	mu         sync.Mutex
	info       protocol.AgentInfo
	metrics    *protocol.HostMetrics
	containers []protocol.ContainerStat
	// containerList / containerInspect 是容器读端点（S3.4）的配置响应。
	// inspect 按 ID 取，取不到按真 Agent 的口径回 404——引擎没有这个容器。
	containerList    []protocol.ContainerSummary
	containerInspect map[string]json.RawMessage
	// selfID 模拟真 Agent 的「不碰自身」守卫：写操作命中即 403。
	// 留空表示不启用守卫（多数读链路测试用）。
	selfID string
	// logsBody 是日志端点（S3.4）的响应体：裸流，不走信封（与真 Agent 一致）。
	logsBody string
	// imageList / pullResult / pruneResult 是镜像端点（S4.1）的配置响应。
	imageList   []protocol.ImageSummary
	pullResult  protocol.PullResult
	pruneResult protocol.PruneResult
	// volumeList / networkList 是卷与网络两个只读端点（S7.9）的配置响应。
	// 两者形态按真实引擎给：卷是对象包数组（这里只存解开后的清单）、网络是裸数组。
	volumeList  []protocol.VolumeSummary
	networkList []protocol.NetworkSummary
	// composeList / composeAction / composeLogs 是 Compose 端点（S4.2）的配置响应。
	composeList   []protocol.ComposeProject
	composeAction protocol.ComposeActionResult
	composeLogs   string
	// composeFile / composeFileDiff / composeFileWritten 是 Compose 文件端点
	// （S4.3）的配置响应：GET 读、POST diff 预览、PUT 写（返回备份路径）。
	composeFile        protocol.ComposeFile
	composeFileDiff    protocol.ComposeFileDiff
	composeFileWritten protocol.ComposeFileWritten
	// composeBackups / backupContents / backupRestored 是写前备份端点（S7.11）
	// 的配置响应：清单、按名字取正文、恢复结果（恢复前新备份的文件名）。
	// backupName 记录最近一次读/恢复请求带的备份名，供断言「名字真的透传到了 Agent」。
	composeBackups []protocol.ComposeBackup
	backupContents map[string]string
	backupRestored protocol.ComposeBackupRestored
	backupName     string
	// fileList / fileMeta / fileContent* / fileWritten / fileActionDone 是文件端点
	// （S5.5）的配置响应。content 走裸流（与真实 Agent 一致）：GET 返回正文与
	// X-DD-File-* 头，PUT 接收正文并记录，POST {action} 接收 FileAction 体。
	fileList        protocol.FilesList
	fileMeta        protocol.FileMeta
	fileContentBody string
	fileContentCT   string
	fileWritten     protocol.FileWritten
	fileActionDone  protocol.FileActionDone
	// calls 记录每次进入业务端点的请求路径（已过鉴权），供断言「实际打到了哪」。
	calls []string
	// bodies 记录写端点的请求体（成功路径），供 e2e 断言 stop/kill/remove
	// 的选项（timeout_sec/signal/force/v）真的透传到了 Agent——
	// 只记路径的话，「Hub 把请求体解错了字段名」这种静默丢选项无从发现。
	bodies []string
	// reqIDs 记录每次请求携带的 X-DD-Request-Id（与 Calls 同序，未带记空串），
	// 供 e2e 断言 Hub 把客户端的幂等键原样转发给了 Agent。
	reqIDs []string
	// failNext 让下 N 次请求先返回给定状态码（测重试），随后恢复正常。
	failNext   int
	failStatus int
}

// New 构造假 Agent，各端点响应为合理的最小样本。
func New(token string) *Fake {
	return &Fake{
		Token:            token,
		containerList:    []protocol.ContainerSummary{},
		containerInspect: map[string]json.RawMessage{},
	}
}

// SetInfo / SetMetrics / SetContainers 配置响应体。
func (f *Fake) SetInfo(i protocol.AgentInfo) { f.mu.Lock(); f.info = i; f.mu.Unlock() }
func (f *Fake) SetMetrics(m *protocol.HostMetrics) {
	f.mu.Lock()
	f.metrics = m
	f.mu.Unlock()
}
func (f *Fake) SetContainers(cs []protocol.ContainerStat) {
	f.mu.Lock()
	f.containers = cs
	f.mu.Unlock()
}

// SetContainerList / SetContainerInspect 配置容器读端点（S3.4）。
func (f *Fake) SetContainerList(cs []protocol.ContainerSummary) {
	f.mu.Lock()
	f.containerList = cs
	f.mu.Unlock()
}
func (f *Fake) SetContainerInspect(m map[string]json.RawMessage) {
	f.mu.Lock()
	f.containerInspect = m
	f.mu.Unlock()
}

// SetSelfID 配置自身容器标识，模拟真 Agent 的自我保护守卫（S3.4）。
func (f *Fake) SetSelfID(id string) {
	f.mu.Lock()
	f.selfID = id
	f.mu.Unlock()
}

// SetLogs 配置日志端点响应体（S3.4，裸流）。
func (f *Fake) SetLogs(body string) {
	f.mu.Lock()
	f.logsBody = body
	f.mu.Unlock()
}

// SetComposeList / SetComposeAction / SetComposeLogs 配置 Compose 端点（S4.2）。
func (f *Fake) SetComposeList(ps []protocol.ComposeProject) {
	f.mu.Lock()
	f.composeList = ps
	f.mu.Unlock()
}
func (f *Fake) SetComposeAction(r protocol.ComposeActionResult) {
	f.mu.Lock()
	f.composeAction = r
	f.mu.Unlock()
}
func (f *Fake) SetComposeLogs(body string) {
	f.mu.Lock()
	f.composeLogs = body
	f.mu.Unlock()
}

// SetComposeFile / SetComposeFileDiff / SetComposeFileWritten 配置 Compose
// 文件端点（S4.3）响应。
func (f *Fake) SetComposeFile(v protocol.ComposeFile) {
	f.mu.Lock()
	f.composeFile = v
	f.mu.Unlock()
}
func (f *Fake) SetComposeFileDiff(v protocol.ComposeFileDiff) {
	f.mu.Lock()
	f.composeFileDiff = v
	f.mu.Unlock()
}
func (f *Fake) SetComposeFileWritten(v protocol.ComposeFileWritten) {
	f.mu.Lock()
	f.composeFileWritten = v
	f.mu.Unlock()
}

// SetComposeBackups 配置 GET /api/v1/compose/{project}/backups 的响应。
func (f *Fake) SetComposeBackups(list []protocol.ComposeBackup) {
	f.mu.Lock()
	f.composeBackups = list
	f.mu.Unlock()
}

// SetBackupContents 配置 GET .../backups/content 按名字返回的正文。
func (f *Fake) SetBackupContents(m map[string]string) {
	f.mu.Lock()
	f.backupContents = m
	f.mu.Unlock()
}

// SetBackupRestored 配置 POST .../backups/restore 的响应。
func (f *Fake) SetBackupRestored(v protocol.ComposeBackupRestored) {
	f.mu.Lock()
	f.backupRestored = v
	f.mu.Unlock()
}

// LastBackupName 返回最近一次读/恢复请求带的备份名（供断言透传）。
func (f *Fake) LastBackupName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backupName
}

// SetFileList / SetFileMeta / SetFileContent / SetFileWritten / SetFileActionDone
// 配置文件端点（S5.5）响应。
func (f *Fake) SetFileList(v protocol.FilesList) {
	f.mu.Lock()
	f.fileList = v
	f.mu.Unlock()
}
func (f *Fake) SetFileMeta(v protocol.FileMeta) {
	f.mu.Lock()
	f.fileMeta = v
	f.mu.Unlock()
}
func (f *Fake) SetFileContent(body, ct string) {
	f.mu.Lock()
	f.fileContentBody = body
	f.fileContentCT = ct
	f.mu.Unlock()
}
func (f *Fake) SetFileWritten(v protocol.FileWritten) {
	f.mu.Lock()
	f.fileWritten = v
	f.mu.Unlock()
}
func (f *Fake) SetFileActionDone(v protocol.FileActionDone) {
	f.mu.Lock()
	f.fileActionDone = v
	f.mu.Unlock()
}

// isSelf 与真 Agent 的 containersAPI.isSelf 同口径：大小写不敏感的前缀匹配
// （容器 ID 的 12 位短形式与 64 位全形式、名字都覆盖到）。
func (f *Fake) isSelf(target string) bool {
	f.mu.Lock()
	self := f.selfID
	f.mu.Unlock()
	if self == "" || target == "" {
		return false
	}
	t := strings.ToLower(target)
	self = strings.ToLower(self)
	return strings.HasPrefix(t, self) || strings.HasPrefix(self, t)
}

// FailNext 让接下来 n 次请求返回 status（测读重试链路）。
func (f *Fake) FailNext(n, status int) {
	f.mu.Lock()
	f.failNext, f.failStatus = n, status
	f.mu.Unlock()
}

// Calls 返回已过鉴权的请求路径副本。
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// WriteBodies 返回写端点已记录的请求体副本（顺序与 Calls 中写请求一致）。
func (f *Fake) WriteBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.bodies))
	copy(out, f.bodies)
	return out
}

// recordBody 记录写端点的请求体。只在成功路径调用：
// 被守卫/白名单拦下的请求没有「透传选项」可言。
func (f *Fake) recordBody(raw []byte) {
	f.mu.Lock()
	f.bodies = append(f.bodies, string(raw))
	f.mu.Unlock()
}

// Handler 装配鉴权中间件 + 端点。
// 读端点：info / metrics / containers-stats（S2）+ containers / containers/{id}（S3.4）。
// 写端点（S3.4）：POST containers/{id}/{action} 与 DELETE containers/{id}，
// 与真 Agent 路由同款方法限定 + 同款自我保护守卫 + 同款动作白名单。
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.Handler {
		return agenthttp.Auth(f.Token, nil)(h)
	}
	mux.Handle("/api/v1/info", authed(f.serveInfo))
	mux.Handle("/api/v1/metrics", authed(f.serveMetrics))
	// 容器子树全部方法限定注册：Go mux 要求同一子树的方法限定一致，
	// 混用（如方法不限定的 stats 与方法限定的 DELETE {id}）直接 panic——
	// 与真 Agent main.go 同款约束（S3.2 踩过并修过）。
	mux.Handle("GET /api/v1/containers/stats", authed(f.serveContainers))
	mux.Handle("GET /api/v1/containers", authed(f.serveContainerList))
	mux.Handle("GET /api/v1/containers/{id}", authed(f.serveContainerInspect))
	mux.Handle("POST /api/v1/containers/{id}/{action}", authed(f.serveContainerAction))
	mux.Handle("DELETE /api/v1/containers/{id}", authed(f.serveContainerRemove))
	mux.Handle("GET /api/v1/containers/{id}/logs", authed(f.serveContainerLogs))
	// ---- 镜像端点（S4.1）：与真 Agent 路由同款方法限定；{ref...} 尾通配符
	// 容纳含 "/" 的引用（library/nginx）。无自身守卫（真 Agent 同款）。----
	mux.Handle("GET /api/v1/images", authed(f.serveImageList))
	mux.Handle("POST /api/v1/images/pull", authed(f.serveImagePull))
	mux.Handle("DELETE /api/v1/images/{ref...}", authed(f.serveImageDelete))
	mux.Handle("POST /api/v1/images/prune", authed(f.serveImagePrune))
	// ---- 卷与网络（S7.9）：只读两条 ----
	mux.Handle("GET /api/v1/volumes", authed(f.serveVolumeList))
	mux.Handle("GET /api/v1/networks", authed(f.serveNetworkList))
	// ---- Compose 端点（S4.2）：list/logs 读；action 写。{project} 是白名单内
	// 项目名（目录名），与真 Agent 路由同款方法限定。----
	mux.Handle("GET /api/v1/compose", authed(f.serveComposeList))
	mux.Handle("POST /api/v1/compose/{project}/{action}", authed(f.serveComposeAction))
	mux.Handle("GET /api/v1/compose/{project}/logs", authed(f.serveComposeLogs))
	// ---- Compose 文件端点（S4.3）：GET 读、POST diff、PUT 写。file/diff 是字面量段，
	// 比 POST 的 {action} 通配更具体，与真 Agent 一样按最具体优先匹配。----
	mux.Handle("GET /api/v1/compose/{project}/file", authed(f.serveComposeFileGet))
	mux.Handle("POST /api/v1/compose/{project}/diff", authed(f.serveComposeFileDiff))
	mux.Handle("PUT /api/v1/compose/{project}/file", authed(f.serveComposeFilePut))
	// ---- 写前备份（S7.11）：清单/正文只读，恢复是覆盖写 ----
	mux.Handle("GET /api/v1/compose/{project}/backups", authed(f.serveComposeBackups))
	mux.Handle("GET /api/v1/compose/{project}/backups/content", authed(f.serveComposeBackupsContent))
	mux.Handle("POST /api/v1/compose/{project}/backups/restore", authed(f.serveComposeBackupsRestore))
	// ---- 文件端点（S5.5）：list/stat/content（读）与 content 写、{action} 写动作。
	// 与真实 Agent 路由同款方法限定；content 走裸流（不经信封）。----
	mux.Handle("GET /api/v1/files", authed(f.serveFileList))
	mux.Handle("GET /api/v1/files/stat", authed(f.serveFileStat))
	mux.Handle("GET /api/v1/files/content", authed(f.serveFileContent))
	mux.Handle("PUT /api/v1/files/content", authed(f.serveFileContentPut))
	mux.Handle("POST /api/v1/files/{action}", authed(f.serveFileAction))
	return mux
}

// Server 起一个 httptest.Server（TLS），返回服务器与关闭函数。
// TLS 模拟真实部署：Agent 全程自签证书，Hub 侧 InsecureSkipVerify + 上层指纹信任。
func (f *Fake) Server() *httptest.Server {
	srv := httptest.NewTLSServer(f.Handler())
	return srv
}

// maybeFail 返回 true 表示本轮要失败。记录调用发生在失败之前：重试链路要数总请求数。
func (f *Fake) maybeFail(r *http.Request) (bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.RequestURI())
	f.reqIDs = append(f.reqIDs, r.Header.Get(protocol.HeaderRequestID))
	if f.failNext > 0 {
		f.failNext--
		return true, f.failStatus
	}
	return false, 0
}

// RequestIDs 返回每次请求的 X-DD-Request-Id 记录副本（与 Calls 同序；
// 未带该头的请求记空串）。
func (f *Fake) RequestIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.reqIDs))
	copy(out, f.reqIDs)
	return out
}

func (f *Fake) serveInfo(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	info := f.info
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, info)
}

func (f *Fake) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	m := f.metrics
	f.mu.Unlock()
	if m == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (f *Fake) serveContainers(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	cs := f.containers
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, cs)
}

func (f *Fake) serveContainerList(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	cs := f.containerList
	f.mu.Unlock()
	// 与真 Agent 一致：清单永不为 null（Engine 空清单是 []）。
	if cs == nil {
		cs = []protocol.ContainerSummary{}
	}
	httpx.WriteJSON(w, http.StatusOK, cs)
}

func (f *Fake) serveContainerInspect(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	id := r.PathValue("id")
	f.mu.Lock()
	raw, ok := f.containerInspect[id]
	f.mu.Unlock()
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "容器不存在")
		return
	}
	// 与真 Agent 一致：Engine JSON 原样进信封的 data 层，一个字段不增不减。
	httpx.WriteJSON(w, http.StatusOK, json.RawMessage(raw))
}

// containerWriteGuard 是写端点的统一守卫：自身容器命中即 403
// （与真 Agent 的 guardWrite 同口径，拦截在任何「成功」之前）。
func (f *Fake) containerWriteGuard(w http.ResponseWriter, id string) bool {
	if f.isSelf(id) {
		httpx.WriteError(w, http.StatusForbidden, protocol.CodeForbidden, "拒绝操作 Agent 自身容器（不碰自身红线）")
		return true
	}
	return false
}

func (f *Fake) serveContainerAction(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	id, action := r.PathValue("id"), r.PathValue("action")
	if f.containerWriteGuard(w, id) {
		return
	}
	// 动作白名单与真 Agent 路由层一致；未知动作 404（真 Agent 同款）。
	switch action {
	case "start", "stop", "restart", "pause", "unpause", "kill", "resize":
	default:
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "未知操作")
		return
	}
	// 请求体只校验「是合法 JSON」，语义由真 Agent/Engine 把关。
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if len(raw) > 0 && !json.Valid(raw) {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	f.recordBody(raw)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (f *Fake) serveContainerRemove(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	if f.containerWriteGuard(w, r.PathValue("id")) {
		return
	}
	// 真 Agent 的 remove 会读请求体（force/v 可走 body）；假 Agent 记录之，
	// 让 e2e 能断言 Hub 的 body/查询串并集口径真的透传了过来。
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	f.recordBody(raw)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// serveContainerLogs 实现 GET /api/v1/containers/{id}/logs。
// 裸流不走信封（与真 Agent 一致）：export=1 给 octet-stream + 下载文件名，
// 否则 text/plain；follow=1 分帧吐并逐帧 Flush——客户端若擅自施加读超时，
// 第一帧之后就会被掐断，e2e 用这一点验证「流免超时」。
func (f *Fake) serveContainerLogs(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "查询串不合法")
		return
	}
	if !protocol.ValidTail(q.Get("tail")) {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "tail 必须是 all 或数字")
		return
	}
	f.mu.Lock()
	body := f.logsBody
	f.mu.Unlock()

	if q.Get("export") == "1" {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeLogName(r.PathValue("id"))+`.log"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush() // 首帧尽快推给客户端
	if q.Get("follow") != "1" {
		_, _ = io.WriteString(w, body)
		return
	}
	_, _ = io.WriteString(w, body)
	flush()
	for i := 1; i <= logsFollowTicks; i++ {
		time.Sleep(logsFollowTickDelay)
		if _, err := io.WriteString(w, "\n--follow-tick-"+strconv.Itoa(i)+"--\n"); err != nil {
			return // 客户端断开即结束
		}
		flush()
	}
}

// sanitizeLogName 让下载文件名不带奇怪字符（真 Agent 同款 sanitizeFilename）。
func sanitizeLogName(id string) string {
	out := make([]rune, 0, len(id))
	for _, ch := range id {
		if (ch >= '0' && ch <= '9') || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '-' || ch == '_' {
			out = append(out, ch)
		}
	}
	if len(out) == 0 {
		return "container"
	}
	return string(out)
}

// ---- 镜像端点（S4.1）----

func (f *Fake) serveImageList(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	list := f.imageList
	f.mu.Unlock()
	if list == nil {
		list = []protocol.ImageSummary{}
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// serveVolumeList 是卷清单（S7.9 只读）。与真 Agent 同口径：nil 回空数组而非 null，
// 让 Hub 侧 agentcli 解码稳定（null 也能解，但空数组与「无卷」的语义更直白）。
func (f *Fake) serveVolumeList(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	list := f.volumeList
	f.mu.Unlock()
	if list == nil {
		list = []protocol.VolumeSummary{}
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// serveNetworkList 是网络清单（S7.9 只读）。
func (f *Fake) serveNetworkList(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	list := f.networkList
	f.mu.Unlock()
	if list == nil {
		list = []protocol.NetworkSummary{}
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

func (f *Fake) serveImagePull(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	var b struct {
		Ref string `json:"ref"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &b) != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if !protocol.ValidImageRef(b.Ref) {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "非法镜像引用")
		return
	}
	f.recordBody(raw)
	f.mu.Lock()
	res := f.pullResult
	f.mu.Unlock()
	// 流内错误按真 Agent 口径映射 502（HTTP 200 但业务失败）。
	if res.Err != "" {
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamError, "拉取失败: "+res.Err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (f *Fake) serveImageDelete(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	ref := r.PathValue("ref")
	if !protocol.ValidImageRef(ref) {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "非法镜像引用")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (f *Fake) serveImagePrune(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	res := f.pruneResult
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, res)
}

// ---- Compose 端点（S4.2）----

func (f *Fake) serveComposeList(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	list := f.composeList
	f.mu.Unlock()
	if list == nil {
		list = []protocol.ComposeProject{}
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

func (f *Fake) serveComposeAction(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	project := r.PathValue("project")
	action := r.PathValue("action")
	if !protocol.ValidComposeAction(action) {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "不支持的 compose 动作: "+action)
		return
	}
	if project == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "项目名不能为空")
		return
	}
	f.mu.Lock()
	res := f.composeAction
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (f *Fake) serveComposeLogs(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	body := f.composeLogs
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

// ---- Compose 文件端点（S4.3）——与真 Agent 同款：走信封 JSON，body 非法即 400 ----

func (f *Fake) serveComposeFileGet(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	res := f.composeFile
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (f *Fake) serveComposeFileDiff(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	// 与真 Agent 同款 1MiB：整份 YAML 进请求体，4KiB 是选项体的口径（v0.2.2 #4）。
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req protocol.ComposeFileUpdate
	if len(raw) > 0 && json.Unmarshal(raw, &req) != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	f.recordBody(raw)
	f.mu.Lock()
	res := f.composeFileDiff
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (f *Fake) serveComposeFilePut(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	// 与真 Agent 同款 1MiB：整份 YAML 进请求体，4KiB 是选项体的口径（v0.2.2 #4）。
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req protocol.ComposeFileUpdate
	if len(raw) > 0 && json.Unmarshal(raw, &req) != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	f.recordBody(raw)
	f.mu.Lock()
	res := f.composeFileWritten
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, res)
}

// serveComposeBackups 是备份清单（S7.11 只读）。
func (f *Fake) serveComposeBackups(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	list := f.composeBackups
	f.mu.Unlock()
	if list == nil {
		list = []protocol.ComposeBackup{}
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeBackupsList{Backups: list})
}

// serveComposeBackupsContent 是备份正文。备份名走查询串（含 + 与 :）。
func (f *Fake) serveComposeBackupsContent(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "缺少 file 参数（备份文件名）")
		return
	}
	f.mu.Lock()
	f.backupName = name
	content := f.backupContents[name]
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, protocol.ComposeBackupContent{Name: name, Content: content})
}

// serveComposeBackupsRestore 是恢复（覆盖写）。记下名字供断言。
func (f *Fake) serveComposeBackupsRestore(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	var body protocol.ComposeBackupUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不合法")
		return
	}
	if body.File == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "缺少 file 字段（备份文件名）")
		return
	}
	f.mu.Lock()
	f.backupName = body.File
	res := f.backupRestored
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, res)
}

// SetImageList / SetPullResult / SetPruneResult 配置镜像端点（S4.1）响应。
func (f *Fake) SetImageList(list []protocol.ImageSummary) {
	f.mu.Lock()
	f.imageList = list
	f.mu.Unlock()
}

// SetVolumeList 配置 GET /api/v1/volumes 的响应。
func (f *Fake) SetVolumeList(list []protocol.VolumeSummary) {
	f.mu.Lock()
	f.volumeList = list
	f.mu.Unlock()
}

// SetNetworkList 配置 GET /api/v1/networks 的响应。
func (f *Fake) SetNetworkList(list []protocol.NetworkSummary) {
	f.mu.Lock()
	f.networkList = list
	f.mu.Unlock()
}

func (f *Fake) SetPullResult(res protocol.PullResult) {
	f.mu.Lock()
	f.pullResult = res
	f.mu.Unlock()
}

func (f *Fake) SetPruneResult(res protocol.PruneResult) {
	f.mu.Lock()
	f.pruneResult = res
	f.mu.Unlock()
}

// ---- 文件端点（S5.5）----

// serveFileList 实现 GET /api/v1/files?root=&path=&depth=。
func (f *Fake) serveFileList(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	v := f.fileList
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, v)
}

// serveFileStat 实现 GET /api/v1/files/stat?root=&path=。
func (f *Fake) serveFileStat(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	v := f.fileMeta
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, v)
}

// serveFileContent 实现 GET /api/v1/files/content?root=&path=。
// 裸流不走信封（与真实 Agent 一致）：正文任意二进制，元信息走 X-DD-File-* 头。
func (f *Fake) serveFileContent(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	body, ct := f.fileContentBody, f.fileContentCT
	f.mu.Unlock()
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-DD-File-Path", "app/config.yaml")
	h.Set("X-DD-File-Size", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

// serveFileContentPut 实现 PUT /api/v1/files/content?root=&path=。
// 请求体即文件内容本身：记录后回传备份信息（与真实 Agent 同款）。
func (f *Fake) serveFileContentPut(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, protocol.MaxFileUploadBytes+1))
	f.recordBody(raw)
	f.mu.Lock()
	v := f.fileWritten
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, v)
}

// serveFileAction 实现 POST /api/v1/files/{action}?root=。
// 记录 FileAction 请求体后回传动作结果（与真实 Agent 同款）。
func (f *Fake) serveFileAction(w http.ResponseWriter, r *http.Request) {
	if done, status := f.maybeFail(r); done {
		w.WriteHeader(status)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	f.recordBody(raw)
	f.mu.Lock()
	v := f.fileActionDone
	f.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, v)
}
