// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Command agent 运行在每台被纳管的宿主机上，只做一件事：对 Hub 提供主机指标与
// 受限操作接口。它不保存历史数据，重装即可恢复。
package main

import (
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/collector"
	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/tlsx"
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
	// 破例放行必须留下痕迹：这条日志是事后追责的唯一依据。
	if cfg.AllowInsecureBind {
		log.Printf("agent: ⚠️ 已启用 --allow-insecure-bind，%q 未经网卡白名单校验，请确认该地址不对外暴露", cfg.ListenAddr)
	}

	// GAP-1 纳管：配了 Hub 与 join_secret 时，启动即自注册。幂等、best-effort，
	// 失败只告警不阻断服务（登记可在此后重启时重试）。放后台，避免拖慢监听就绪。
	// 反向模式下 enroll 必须同步（握手前 servers 表里必须已有本机记录），
	// 所以后台自注册只服务正常模式。
	if cfg.HubURL != "" && cfg.JoinSecret != "" && !cfg.Reverse {
		go func() {
			if err := enrollToHub(cfg); err != nil {
				log.Printf("agent: ⚠️ 纳管自注册失败（将在下次重启重试）：%v", err)
			}
		}()
	}

	// 文件管理可能未启用（未配 --fs-root 时返回 nil）；配了而根目录不可用则启动失败。
	fapi, err := newFilesAPI(cfg)
	if err != nil {
		return err
	}

	handler := buildHandler(cfg.Token, collector.New(cfg.DiskMounts), newEngineStatsSource(cfg.DockerSocket),
		newContainersAPI(cfg.DockerSocket, cfg.SelfContainer), newImagesAPI(cfg.DockerSocket),
		newComposeAPI(cfg.ComposeDirs), fapi, newResourcesAPI(cfg.DockerSocket))

	// 反向模式：不监听、不自签，主动连 Hub；本地 mux 仍是唯一入口，
	// 帧会被合成为 http.Request 后回到这个 handler，路由/鉴权与 HTTP 模式完全一致。
	if cfg.Reverse {
		if err := enrollToHub(cfg); err != nil {
			log.Printf("agent: ⚠️ 反向模式纳管失败（Hub 里没有本机记录就无法握手）：%v", err)
			return err
		}
		log.Printf("agent: 反向模式，主动连接 Hub %s", cfg.HubURL)
		return runReverse(cfg, handler)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	if cfg.TLSMode == config.TLSModeNone {
		log.Printf("agent: 明文监听 %s（TLS 模式为 none，仅限回环/反代场景）", cfg.ListenAddr)
		return srv.ListenAndServe()
	}

	host, port, err := config.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.CertDir, 0o700); err != nil {
		return err
	}
	cert, fingerprint, err := tlsx.EnsureSelfSigned(cfg.CertDir, host, port)
	if err != nil {
		return err
	}
	// 指纹是 Hub 首次纳管时信任这台 Agent 的依据，必须打印出来供人核对。
	log.Printf("agent: 自签证书指纹 %s（Hub 纳管时需比对）", fingerprint)
	srv.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	log.Printf("agent: 监听 https://%s", cfg.ListenAddr)
	return srv.ListenAndServeTLS("", "")
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
		mux.Handle("GET /api/v1/files/stat", fsAuth(http.HandlerFunc(fapi.stat)))
		mux.Handle("GET /api/v1/files/content", fsAuth(http.HandlerFunc(fapi.content)))
		mux.Handle("PUT /api/v1/files/content", fsAuth(http.HandlerFunc(fapi.contentPut)))
		mux.Handle("POST /api/v1/files/{action}", fsAuth(http.HandlerFunc(fapi.action)))
	}
	return mux
}

// containerStatsHandler 返回每容器资源占用快照。
//
// 降级语义与主机指标一致：源出错时返回 200 + 空列表并记日志，而不是 5xx——
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

// metricsHandler 返回一次主机指标快照。
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
