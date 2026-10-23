// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/yy1319hua/dockdeck/internal/agenthttp"
	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// localAgent 是 Hub 进程内本机 Agent 的运行态。
type localAgent struct {
	id   int64
	addr string
	srv  *http.Server
}

// startLocalAgent 在 Hub 进程内启动本机 Agent（监听回环）。
//
// 顺序刻意是「先监听、再注册」：配置成 127.0.0.1:0 时端口由内核分配，
// 只有拿到实际地址后写进 servers.base_url，Hub 的 HTTP 调用方才能拨通。
// 注册幂等（type=local，server_id 固定），令牌与远端 Agent 同款，
// 鉴权、路由、指标口径完全一致——本机不是特殊分支，只是少了一层网络。
//
// 返回错误时调用方只记日志、不阻断 Hub 启动：远端管理与监控不依赖本机 Agent。
func startLocalAgent(ctx context.Context, st *store.Store, cfg config.Config) (*localAgent, error) {
	ln, err := net.Listen("tcp", cfg.LocalAgentAddr)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", cfg.LocalAgentAddr, err)
	}
	addr := ln.Addr().String()

	id, token, err := st.EnsureLocalServer(ctx, "http://"+addr)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("注册本机服务器: %w", err)
	}

	handler, err := agenthttp.NewHandler(config.LocalAgentConfig(token, cfg.LocalDockerSocket, cfg.LocalComposeDirs))
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("装配本机 Agent 路由: %w", err)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			log.Printf("hub: 本机 Agent 服务退出：%v", serr)
		}
	}()
	return &localAgent{id: id, addr: addr, srv: srv}, nil
}
