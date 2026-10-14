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
