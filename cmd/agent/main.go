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

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           buildHandler(cfg.Token, collector.New(cfg.DiskMounts), collector.NilStatsSource{}, newContainersAPI(cfg.DockerSocket, cfg.SelfContainer), newImagesAPI(cfg.DockerSocket), newComposeAPI(cfg.ComposeDirs)),
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
func buildHandler(token string, col *collector.Collector, src collector.ContainerStatsSource, capi *containersAPI, iapi *imagesAPI, compapi *composeAPI) http.Handler {
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
	// ---- Compose 接口（S4.2）：list/logs 读；action 写（无自身守卫，白名单即保护）。
	// {project} 是白名单内项目名（目录名），Agent 侧 resolveProject 映射到真实路径，
	// 防 ../ 穿越——白名单外的名字（含任意路径）一律 404。----
	mux.Handle("GET /api/v1/compose", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.list)))
	mux.Handle("POST /api/v1/compose/{project}/{action}", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.action)))
	mux.Handle("GET /api/v1/compose/{project}/logs", agenthttp.Auth(token, nil)(http.HandlerFunc(compapi.logs)))
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
