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
		Handler:           buildHandler(cfg.Token, collector.New(cfg.DiskMounts)),
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
func buildHandler(token string, col *collector.Collector) http.Handler {
	mux := http.NewServeMux()
	// Agent 侧同样使用 /api/v1 前缀，与 Hub 侧路由一一对应。
	mux.Handle("/api/v1/info", agenthttp.Auth(token, nil)(agenthttp.Info()))
	mux.Handle("/api/v1/metrics", agenthttp.Auth(token, nil)(metricsHandler(col)))
	return mux
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
