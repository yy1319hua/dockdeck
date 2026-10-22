// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Command agent 运行在每台被纳管的宿主机上，只做一件事：对 Hub 提供服务器指标与
// 受限操作接口。它不保存历史数据，重装即可恢复。
//
// 2026-10-20（v0.2.6）起 Agent 只有反向长连接一种形态：不监听任何端口，
// 启动后主动连 Hub 的 WebSocket——NAT 后 / 无公网 IP 的机器同样可纳管。
// 原先的正向模式（--listen + 自签证书 + 网卡白名单）整套删除。
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/collector"
	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/fsapi"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("agent: %v", err)
	}
}

func run(args []string) error {
	cfg, err := config.LoadAgent(args)
	if err != nil {
		return err
	}

	// 文件管理可能未启用（未配 --fs-root 时返回 nil）；配了而根目录不可用则启动失败。
	fapi, err := newFilesAPI(cfg)
	if err != nil {
		return err
	}

	// compose 目录配置里的可疑项（相对路径、指向根目录等）在启动时说清楚——
	// 配置错了要能被看见，而不是安静地扫了个寂寞。
	for _, w := range cfg.ComposeDirWarnings {
		log.Printf("agent: compose 目录配置提醒: %s", w)
	}

	// 容器删除联动需要文件管理守卫与 compose 操作器；二者未启用时传 nil，
	// cmd/agent 的 remove 内部按 nil 安全处理（只做主删除、跳过联动）。
	var fsGuard *fsapi.Guard
	if fapi != nil {
		fsGuard = fapi.g
	}
	compapi := newComposeAPI(cfg.ComposeDirs, cfg.SelfContainer, cfg.DockerSocket)

	handler := buildHandler(cfg.Token, collector.New(cfg.DiskMounts), newEngineStatsSource(cfg.DockerSocket),
		newContainersAPI(cfg.DockerSocket, cfg.SelfContainer, fsGuard, compapi.op, cfg.ComposeDirs), newImagesAPI(cfg.DockerSocket),
		compapi, fapi, newResourcesAPI(cfg.DockerSocket))

	// 反向模式下「能连上 Hub 并完成纳管」就是 Agent 的命脉：Hub 未就绪时无限重试
	// （退避上限 30s），只有凭据/邀请类永久失败才退出——
	// 那种情况重试一万次也不会好，早退出能让运维直接看到原因。
	// 握手前 servers 表里必须已有本机记录，所以这一步必须同步完成。
	if err := enrollWithRetry(context.Background(), cfg, 0); err != nil {
		return err
	}
	log.Printf("agent: 已纳管，主动连接 Hub %s", cfg.HubURL)

	// 唯一形态：不监听端口，本地 mux 是唯一入口——反向连接收到的帧会被合成
	// http.Request 后回到这个 handler，路由/鉴权与曾经的 HTTP 模式完全一致。
	return runReverse(cfg, handler)
}

// buildHandler 装配 Agent 的路由表。
// 抽成函数是为了让冒烟测试能复用与生产完全相同的中间件链。
// src 是容器指标源：S3.1 接入 Engine API 前用 NilSource，端点始终可用但返回空列表。
// 传 nil 时同样落到 NilSource，调用方不必判空。
// iapi 是镜像端点（S4.1）：与容器共享同一把 docker socket，无自身守卫。
// compapi 是 Compose 端点（S4.2）：白名单目录来自配置，exec 走宿主 docker compose。
// fapi 是文件管理端点（S5.1）：**传 nil 时一条路由都不注册**——红线第 3 条
// 「未配 --fs-root 时该模块不注册路由」的落点就在这个 if 上。
// rapi 是卷与网络端点（S7.9）：只读两条，与容器共享同一把 docker socket。
func buildHandler(token string, col *collector.Collector, src collector.ContainerStatsSource, capi *containersAPI, iapi *imagesAPI, compapi *composeAPI, fapi *filesAPI, rapi *resourcesAPI) http.Handler {
	mux := http.NewServeMux()
	// Agent 侧同样使用 /api/v1 前缀，与 Hub 侧路由一一对应。
	mux.Handle("/api/v1/info", agenthttp.Auth(token, nil)(agenthttp.Info()))
	mux.Handle("/api/v1/metrics", agenthttp.Auth(token, nil)(metricsHandler(col)))
	mux.Handle("GET /api/v1/containers/stats", agenthttp.Auth(token, nil)(containerStatsHandler(src)))
	// ---- 容器接口（S3.2）：读写都在 Auth 后面；写操作另有自我保护守卫 ----
	// 全部方法限定注册：Go mux 要求同一子树的方法限定一致，混用会 panic。
	mux.Handle("GET /api/v1/containers", agenthttp.Auth(token, nil)(http.HandlerFunc(capi.list)))
	mux.Handle("GET /api/v1/containers/{id}", agenthttp.Auth(token, nil)(http.HandlerFunc(capi.inspect)))
	mux.Handle("POST /api/v1/containers/{id}/{action}", agenthttp.Auth(token, nil)(http.HandlerFunc(capi.action)))
	mux.Handle("DELETE /api/v1/containers/{id}", agenthttp.Auth(token, nil)(http.HandlerFunc(capi.remove)))
	mux.Handle("GET /api/v1/containers/{id}/logs", agenthttp.Auth(token, nil)(http.HandlerFunc(capi.logs)))
	// 容器进程明细（#91）：从宿主 /proc 读取归属该容器的进程，需 pid:host。
	mux.Handle("GET /api/v1/containers/{id}/processes", agenthttp.Auth(token, nil)(http.HandlerFunc(capi.processes)))
	// ---- 镜像接口（S4.1）：list 读；pull/delete/prune 写（无自身守卫，
	// 见 images.go 头注）。{ref...} 尾通配符容纳含 "/" 的镜像引用。----
	mux.Handle("GET /api/v1/images", agenthttp.Auth(token, nil)(http.HandlerFunc(iapi.list)))
	mux.Handle("POST /api/v1/images/pull", agenthttp.Auth(token, nil)(http.HandlerFunc(iapi.pull)))
	mux.Handle("DELETE /api/v1/images/{ref...}", agenthttp.Auth(token, nil)(http.HandlerFunc(iapi.delete)))
	mux.Handle("POST /api/v1/images/prune", agenthttp.Auth(token, nil)(http.HandlerFunc(iapi.prune)))
	// ---- 卷与网络（S7.9）：只读两条，无写端点。----
	mux.Handle("GET /api/v1/volumes", agenthttp.Auth(token, nil)(http.HandlerFunc(rapi.volumes)))
	mux.Handle("GET /api/v1/networks", agenthttp.Auth(token, nil)(http.HandlerFunc(rapi.networks)))
	// ---- Compose 接口（S4.2）：list/logs 读；action 写（无自身守卫，白名单即保护）。
	// {project} 是白名单内项目名（目录名），Agent 侧 resolveProject 映射到真实路径，
	// 防 ../ 穿越——白名单外的名字（含任意路径）一律 404。----
	mux.Handle("GET /api/v1/compose", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.list)))
	mux.Handle("POST /api/v1/compose/{project}/{action}", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.action)))
	mux.Handle("GET /api/v1/compose/{project}/logs", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.logs)))
	// ---- Compose YAML 文件读写（S4.3）：GET 读、POST diff 预览、PUT 写（写前备份）。
	// file/diff 是字面量段，比 POST 的 {action} 通配更具体，Go mux 按最具体优先匹配，
	// 不会与 action 路由冲突（"diff" 本身也不是合法动作）。----
	mux.Handle("GET /api/v1/compose/{project}/file", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.fileGet)))
	mux.Handle("POST /api/v1/compose/{project}/diff", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.fileDiff)))
	mux.Handle("PUT /api/v1/compose/{project}/file", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.filePut)))
	// ---- Compose 写前备份（S7.11）：列举/读取是只读，恢复是覆盖写（恢复前先备份当前内容）。
	// backups/content 与 file 都是字面量段，比 POST 的 {action} 通配更具体，不冲突。----
	mux.Handle("GET /api/v1/compose/{project}/backups", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.backupsList)))
	mux.Handle("GET /api/v1/compose/{project}/backups/content", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.backupsContent)))
	mux.Handle("POST /api/v1/compose/{project}/backups/restore", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.backupsRestore)))
	// ---- 文件管理（S5.1）：未配 --fs-root 时 fapi 为 nil，这里一条都不注册。
	// 上传路由用 AuthN 抬高签名体上限（验签要读完整 body），其余端点仍用默认上限。
	// stat/content 是字面量段，与 POST 的 {action} 通配不冲突（方法也不同）。----
	if fapi != nil {
		fsAuth := agenthttp.AuthN(token, nil, fapi.maxBody)
		mux.Handle("GET /api/v1/files", fsAuth(http.HandlerFunc(fapi.list)))
		mux.Handle("GET /api/v1/files/roots", fsAuth(http.HandlerFunc(fapi.roots)))
		mux.Handle("GET /api/v1/files/stat", fsAuth(http.HandlerFunc(fapi.stat)))
		mux.Handle("GET /api/v1/files/content", fsAuth(http.HandlerFunc(fapi.content)))
		mux.Handle("PUT /api/v1/files/content", fsAuth(http.HandlerFunc(fapi.contentPut)))
		mux.Handle("POST /api/v1/files/{action}", fsAuth(http.HandlerFunc(fapi.action)))
	}
	return mux
}

// containerStatsHandler 返回每容器资源占用快照。
//
// 降级语义与服务器指标一致：源出错时返回 200 + 空列表并记日志，而不是 5xx——
// 容器曲线缺失不该让 Hub 误判机器离线；NilSource 天然走这条路径。
func containerStatsHandler(src collector.ContainerStatsSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if src == nil {
			src = collector.NilStatsSource{}
		}
		stats, err := src.ContainerStats()
		if err != nil {
			log.Printf("agent: 容器指标采集失败，本轮返回空列表：%v", err)
			stats = []protocol.ContainerStat{}
		}
		httpx.WriteJSON(w, http.StatusOK, stats)
	}
}

// metricsHandler 返回一次服务器指标快照。
//
// 部分指标缺失时仍返回 200：少一项只是少一条曲线，返回 5xx 会让 Hub
// 把这台机器判成离线——那是比缺指标严重得多的误判。
func metricsHandler(col *collector.Collector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := col.Collect()
		if err != nil {
			log.Printf("agent: 指标采集存在缺失：%v", err)
		}
		httpx.WriteJSON(w, http.StatusOK, m)
	}
}
