// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S3.4：容器接口的 Hub 转发层。handler 只做参数解析与状态码映射，
// 与 Agent 的通信全部走 agentcli（S3.4 前半已建并单测过），
// 本文件不碰 SQL——唯一的库访问是 ServerConn 取连接信息。
//
// 与 S2.7 指标端点的分工：那些端点读 Hub 自己的库（轮询采过的快照），
// 这些端点实时转发到 Agent。失败语义因此分开：库故障是 Hub 的 500；
// 转发失败经 protocol.StatusForCode 映射，让调用方分清「Hub 自己挂了」
// 与「Agent 不在线/拒绝了」。

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/agentcli"
	"github.com/yy1319hua/dockdeck/internal/guard"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// agentFor 按 {id} 取服务器连接信息并构造 Agent 客户端。
// 失败时它自己把错误翻译成响应（400/403/404/500）并返回 ok=false，
// 调用方直接 return——每个转发 handler 开头都是同样两行。
//
// 客户端每请求现造、不缓存：Hub 的转发量是「管理员点一下」级别，
// 连接池收益可以忽略；而缓存要处理「服务器改地址/换令牌后失效」，
// 那是额外的正确性负担。正确优先，复用的优化等有实测数据再说。
func (s *server) agentFor(w http.ResponseWriter, r *http.Request) (*agentcli.Client, bool) {
	id, err := parseServerID(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "非法服务器 ID")
		return nil, false
	}
	conn, err := s.st.ServerConn(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "服务器不存在")
		return nil, false
	case errors.Is(err, store.ErrServerDisabled):
		// 禁用服务器的历史指标仍可查（ServerExists 对禁用放行），
		// 但实时转发一律拒绝：向一台已下线的 Agent 发操作没有意义。
		httpx.WriteError(w, http.StatusForbidden, protocol.CodeForbidden, "服务器已禁用，拒绝实时操作")
		return nil, false
	case err != nil:
		// 含令牌解密失败：那会把「认证失败」伪装成「网络不通」，
		// 排障时骗人，宁可 500 让管理员看见密文损坏。
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "读取服务器连接信息失败")
		return nil, false
	}
	cli := agentcli.New(conn.BaseURL, conn.Token, nil)
	// 有活跃反向连接就优先走长连接；没有则保持 nil，全部请求回落 HTTP。
	if s.links != nil {
		if lc := s.links.Conn(id); lc != nil {
			cli.Link = lc
			return cli, true
		}
	}
	if strings.HasPrefix(conn.BaseURL, "reverse://") {
		// 反向模式的 Agent 没有可拨号地址：无连接即离线，别去撞一个不存在的端口。
		httpx.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamUnavailable, "Agent 反向连接不在线")
		return nil, false
	}
	return cli, true
}

// writeAgentError 把 agentcli 的失败翻译成 Hub 的响应。
//
//   - *protocol.Error 是业务结果（Agent 拒绝、引擎 404/409、上游不可达……），
//     经 protocol.StatusForCode 映射状态码后原样透传 code+message——
//     调用方看到的错误码与直连 Agent 时一致，Hub 不做二次造词；
//   - 其余错误全部是出门前的本地校验失败（容器 ID 字符集、tail 形态、
//     resize 行列），请求本身不合法：400 并把原因说清楚。
//     这类文案是用户输入的镜子，不含内部状态。
func writeAgentError(w http.ResponseWriter, err error) {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		httpx.WriteError(w, protocol.StatusForCode(pe.Code), pe.Code, humanizeAgentMessage(pe.Message))
		return
	}
	httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, humanizeAgentMessage(err.Error()))
}

// humanizeAgentMessage 剥掉错误文案里的内部包名前缀与重复段（v0.2.2 #6）：
// Agent 侧各包直接透出 "fsapi: …" / "dockercli: …"，hub 再叠一层动作名，
// toast 上就成了 "compose up 失败: dockercli: compose up 失败: exec: …"。
// 规则：按 ": " 分段，丢弃内部包名段与相邻重复段，只留人话。
func humanizeAgentMessage(msg string) string {
	internal := map[string]bool{
		"fsapi": true, "dockercli": true, "agentcli": true, "guard": true,
		"httpx": true, "exec": true, "crypto": true, "store": true, "metricsync": true,
	}
	segs := strings.Split(msg, ": ")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if internal[s] {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == s {
			continue
		}
		out = append(out, s)
	}
	return strings.Join(out, ": ")
}

// parseAll 解析 ?all=。严格口径：只认 ""/false/0 与 true/1——
// 拼错要响亮失败，而不是静默按 false 处理（清单少给一个容器比报错更难排查）。
func parseAll(r *http.Request) (bool, error) {
	switch v := r.URL.Query().Get("all"); v {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	default:
		return false, fmt.Errorf("all 必须是 true/false（或 1/0），也可省略，got %q", v)
	}
}

// decodeOptionalBody 读可选请求体：空体表示「无选项」（Agent 用引擎默认），
// 非空严格解码——多一个字段也拒，防止把 typo 静默吞成默认行为。
// 4KB 上限与 Agent 侧同款：选项体只有一两个字段，给大了就是异常。
func decodeOptionalBody(r *http.Request, dst any) error {
	return decodeBody(r, dst, 4<<10)
}

// decodeBody 按给定上限读体并严格解码（v0.2.2 #4 拆出的通用形态）。
// compose 端点把整份 YAML 放请求体，用 1MiB 上限（与 httpx.DecodeJSON 同款）。
func decodeBody(r *http.Request, dst any, limit int64) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求体不合法: %w", err)
	}
	return nil
}

// positiveInt 解析必需的正整数查询参数（resize 的 h/w）。
func positiveInt(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s 必须是正整数，got %q", name, v)
	}
	return n, nil
}

// containerList 实现 GET /api/v1/servers/{id}/containers。
// 清单实时取自 Agent（Engine 原生投影）；agentcli 保证空清单编码为 []
// 而非 null，前端据此区分「没有容器」与「没拿到数据」。
func (s *server) containerList(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	all, err := parseAll(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	list, err := cl.ContainerList(r.Context(), all)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// containerInspect 实现 GET /api/v1/servers/{id}/containers/{cid}。
// 响应 {"data": <Engine 原始 JSON>}：Hub 不解读 inspect 的几百个字段
// （裁剪即丢信息），原样透传。cid 非法时 agentcli 本地就拒，
// 经 writeAgentError 落为 400——不校验等于把路径拼接入口敞开。
func (s *server) containerInspect(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	raw, err := cl.ContainerInspect(r.Context(), r.PathValue("cid"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, json.RawMessage(raw))
}

// containerProcesses 实现 GET /api/v1/servers/{id}/containers/{cid}/processes（#91）。
// 透传 Agent 从宿主 /proc 读到的进程明细（需 Agent 以 pid:host 运行）。
// 空清单由 Agent 保证是 [] 非 null，Hub 原样透传不加工。
func (s *server) containerProcesses(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	procs, err := cl.ContainerProcesses(r.Context(), r.PathValue("cid"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, procs)
}

// containerProcessDetail 实现 GET /api/v1/servers/{id}/containers/{cid}/processes/{pid}（#91 v2）。
// 透传 Agent 返回的 exe/cwd；pid 越权/不存在由 Agent 统一 404。
func (s *server) containerProcessDetail(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil || pid <= 0 {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "非法进程 PID")
		return
	}
	d, err := cl.ContainerProcessDetail(r.Context(), r.PathValue("cid"), pid)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

// containerAction 实现 POST /api/v1/servers/{id}/containers/{cid}/{action}。
// action ∈ start|stop|restart|pause|unpause|kill|resize——白名单就是下面
// switch 本身，未知动作 404（与 Agent 侧同款：没有这个子资源）。
// 各动作的选项口径与 Agent 侧一字不差：
//   - stop 请求体 {"timeout_sec":N}（SIGTERM 后等待秒数，nil 用引擎默认）
//   - kill 请求体 {"signal":"SIGTERM"}（空用引擎默认；白名单在 protocol 单点）
//   - resize 查询串 ?h=&w=（行列数，Engine 原生约定）
//
// 其余动作无选项。
func (s *server) containerAction(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	id, action := r.PathValue("cid"), r.PathValue("action")
	ctx := r.Context()

	var err error
	switch action {
	case "start":
		err = cl.ContainerStart(ctx, id)
	case "stop":
		var body struct {
			TimeoutSec *int `json:"timeout_sec"`
		}
		if err = decodeOptionalBody(r, &body); err == nil {
			err = cl.ContainerStop(ctx, id, body.TimeoutSec)
		}
	case "restart":
		err = cl.ContainerRestart(ctx, id)
	case "pause":
		err = cl.ContainerPause(ctx, id)
	case "unpause":
		err = cl.ContainerUnpause(ctx, id)
	case "kill":
		var body struct {
			Signal string `json:"signal"`
		}
		if err = decodeOptionalBody(r, &body); err == nil {
			err = cl.ContainerKill(ctx, id, body.Signal)
		}
	case "resize":
		var h, wd int
		if h, err = positiveInt(r, "h"); err == nil {
			if wd, err = positiveInt(r, "w"); err == nil {
				err = cl.ContainerResize(ctx, id, h, wd)
			}
		}
	default:
		httpx.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "未知操作 "+action)
		return
	}
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// containerRemove 实现 DELETE /api/v1/servers/{id}/containers/{cid}。
//
// S4.4 三重门（本端点守前两道，Agent 自我保护是物理防线）：
//  1. 保护名单命中即拒（403）——名单来自 settings.protected_containers
//     （S7.8 起 JSON 数组文本，存量逗号分隔读取兼容），空缺省用 guard.DefaultPatterns；
//  2. 名字确认（400）——confirm 须与目标 ID/名字完全一致，query 与 body
//     取并集（与 force/v 同款口径），并透传给 Agent（Agent 侧同守一道）。
//
// force/v 查询串与请求体取并集——与 Agent 侧 remove 同款口径
// （Agent 也是 b.Force || query=="1"）。两处都认是为了让 curl 与
// 前端表单都能用，语义并集不猜：任何一处置位即生效。
// 具名卷与 bind 目录不归 Engine 管，清理走文件模块或显式卷删除。
func (s *server) containerRemove(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	cid := r.PathValue("cid")
	var body struct {
		Force           bool   `json:"force"`
		Volumes         bool   `json:"volumes"`
		RemoveImage     bool   `json:"remove_image"`
		RemoveDeployDir bool   `json:"remove_deploy_dir"`
		Confirm         string `json:"confirm"`
	}
	if err := decodeOptionalBody(r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	confirm := body.Confirm
	if confirm == "" {
		confirm = q.Get("confirm")
	}
	cg, err := s.containerGuard(r.Context())
	if err != nil {
		log.Printf("hub: 保护名单配置错误: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "保护名单配置错误")
		return
	}
	if cg.IsProtected(cid) {
		httpx.WriteError(w, http.StatusForbidden, protocol.CodeForbidden, "受保护容器，拒绝删除（保护名单命中）")
		return
	}
	if err := guard.Confirm(cid, confirm); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	force := body.Force || q.Get("force") == "1"
	volumes := body.Volumes || q.Get("v") == "1"
	removeImage := body.RemoveImage || q.Get("remove_image") == "1"
	removeDeployDir := body.RemoveDeployDir || q.Get("remove_deploy_dir") == "1"
	// 删容器是主操作；remove_image / remove_deploy_dir 是联动清理，结果随
	// ContainerRemoveResult 一并回传（OK 主操作是否成功，Messages 逐条联动结果）。
	res, err := cl.ContainerRemove(r.Context(), cid, force, volumes, removeImage, removeDeployDir, confirm)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// containerGuard 取保护名单并编译。名单存 settings.protected_containers
// （S7.8 起 JSON 数组文本，存量逗号分隔读取兼容），空缺省回落 DefaultPatterns；
// 坏正则报错——名单是
// 安全边界，宁可端点 500 也不静默放行。逐请求读取：settings 改动即时
// 生效，SQLite 本地读的开销可忽略。
func (s *server) containerGuard(ctx context.Context) (*guard.Guard, error) {
	raw, err := s.st.GetSetting(ctx, guard.SettingKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	list := guard.ParseList(raw)
	if len(list) == 0 {
		list = guard.DefaultPatterns
	}
	return guard.New("", list...)
}

// containerLogs 实现 GET /api/v1/servers/{id}/containers/{cid}/logs。
// 这是全表唯一 NoEnvelope 端点：日志是裸流，套 {data,error} 信封既别扭
// 又破坏边下边看的流式语义。三个与信封端点不同的处理：
//   - 内容类型与 Content-Disposition 从 Agent 响应透传（export 是下载）；
//   - 不设 Hub 侧读取超时：流的寿命归浏览器 ctx，浏览器断开即取消对
//     Agent 的请求（r.Context() 一路传进 agentcli.Logs）；
//   - 逐块 Flush：follow 模式下前端才能看到滚动。
//
// 查询参数口径与 Agent 侧 logsQuery 一字不差（follow/timestamps/export
// 只认 "1"；tail 白名单在 protocol 单点，agentcli 本地先验）。
func (s *server) containerLogs(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.agentFor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	st, err := cl.Logs(r.Context(), r.PathValue("cid"), protocol.LogsOptions{
		Follow:     q.Get("follow") == "1",
		Tail:       q.Get("tail"),
		Since:      q.Get("since"),
		Until:      q.Get("until"),
		Timestamps: q.Get("timestamps") == "1",
		Export:     q.Get("export") == "1",
	})
	if err != nil {
		writeAgentError(w, err) // 含 tail 非法等本地校验：400
		return
	}
	defer st.Body.Close()

	h := w.Header()
	if st.ContentType != "" {
		h.Set("Content-Type", st.ContentType)
	} else {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	if st.Filename != "" {
		h.Set("Content-Disposition", `attachment; filename="`+st.Filename+`"`)
	}
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := st.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 浏览器断开：写不出去即结束，状态码已发无从补救
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			// 流中途断掉（Agent 重启、网络抖动）：响应头已发出，
			// 无法再用状态码表达，记日志，前端以连接关闭为结束信号。
			if !errors.Is(rerr, io.EOF) {
				log.Printf("hub: 日志流异常中断 %s: %v", r.URL.Path, rerr)
			}
			return
		}
	}
}
