// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S3.2：Agent 容器接口。读转发给 dockercli；写转发前过「自我保护」守卫。
//
// 职责边界：保护名单与名字确认是 Hub/UI 层的三重门；Agent 只守最后一道
// 物理防线——绝不响应针对自身容器的写操作（红线「不碰自身」），
// 且每个操作只针对单个容器（本文件没有任何批量入口）。

package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/dockercli"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// containersAPI 聚合容器端点的依赖。
type containersAPI struct {
	dc *dockercli.Client
	// selfID 是自身容器标识（ID/名字/短 ID 都可能），写入命中即拒。
	selfID string
}

// newContainersAPI 构造。self 为空时取 hostname（容器内 HOSTNAME 即短 ID，
// Docker 默认行为），拿不到就留空——没有 docker socket 的环境写操作自然全失败。
func newContainersAPI(socket, self string) *containersAPI {
	if socket == "" {
		socket = dockercli.DefaultSocketPath
	}
	if self == "" {
		h, err := os.Hostname()
		if err == nil {
			self = h
		}
	}
	return &containersAPI{dc: dockercli.New(socket), selfID: strings.ToLower(strings.TrimSpace(self))}
}

// isSelf 判断目标是否命中自身。比较是大小写不敏感的前缀匹配：
// 容器 ID 的 12 位短形式与 64 位全形式、名字都覆盖到。
func (a *containersAPI) isSelf(target string) bool {
	if a.selfID == "" || target == "" {
		return false
	}
	t := strings.ToLower(target)
	return strings.HasPrefix(t, a.selfID) || strings.HasPrefix(a.selfID, t)
}

// guardWrite 是写操作的统一守卫。返回 true 表示已写响应，调用方直接 return。
func (a *containersAPI) guardWrite(w http.ResponseWriter, id string) bool {
	if a.isSelf(id) {
		httpx.WriteError(w, http.StatusForbidden, protocol.CodeForbidden, "拒绝操作 Agent 自身容器（不碰自身红线）")
		return true
	}
	return false
}

// errToStatus 把 dockercli 的错误映射到 HTTP 状态码。
// 文案里带 Engine 的原始片段（截断过），排查时能看到 Docker 说了什么。
func errToStatus(err error) (int, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "不存在 (404)"):
		return http.StatusNotFound, msg
	case strings.Contains(msg, "(409)"):
		return http.StatusConflict, msg
	case strings.Contains(msg, "非法容器标识") || strings.Contains(msg, "不允许的信号"):
		return http.StatusBadRequest, msg
	default:
		log.Printf("agent: 容器操作引擎错误: %v", err)
		return http.StatusBadGateway, "Docker 引擎请求失败"
	}
}

// list 实现 GET /api/v1/containers?all=。
func (a *containersAPI) list(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "true"
	out, err := a.dc.List(r.Context(), all)
	if err != nil {
		code, msg := errToStatus(err)
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
		code, msg := errToStatus(err)
		httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, json.RawMessage(raw))
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
	writeAction(w, err)
}

// removeBody 是 DELETE 的可选请求体（force/v 也可以走查询串，二者取并集）。
type removeBody struct {
	Force   bool `json:"force"`
	Volumes bool `json:"volumes"`
}

// remove 实现 DELETE /api/v1/containers/{id}?force=&v=。
// 引擎不感知的 bind 目录/具名卷清理不在这里——调用方走文件模块与显式卷删除。
func (a *containersAPI) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.guardWrite(w, id) {
		return
	}
	var b removeBody
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		if len(raw) > 0 && json.Unmarshal(raw, &b) != nil {
			httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体不是合法 JSON")
			return
		}
	}
	force := b.Force || r.URL.Query().Get("force") == "1"
	volumes := b.Volumes || r.URL.Query().Get("v") == "1"
	writeAction(w, a.dc.Remove(r.Context(), id, force, volumes))
}

// writeAction 统一写操作的成功/失败响应。
func writeAction(w http.ResponseWriter, err error) {
	if err == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	code, msg := errToStatus(err)
	httpx.WriteError(w, code, protocol.CodeUpstreamError, msg)
}
