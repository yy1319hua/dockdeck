// Command hub 是 DockDeck 管理面板的服务端。
// 单二进制内嵌前端静态资源，绑定 tailscale 网卡 IP 提供 HTTPS。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/yy1319hua/dockdeck/internal/assets"
	"github.com/yy1319hua/dockdeck/internal/auth"
	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/middleware"
	"github.com/yy1319hua/dockdeck/internal/store"
	"github.com/yy1319hua/dockdeck/internal/tlsx"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("hub: %v", err)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	masterKey, err := crypto.LoadOrCreateMasterKey(cfg.MasterKeyPath)
	if err != nil {
		return err
	}

	sqlDB, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	st := &store.Store{DB: sqlDB, Key: masterKey}

	authOpt := auth.DefaultOptions()
	authOpt.Secure = cfg.TLSMode != config.TLSModeNone
	am := auth.NewManager(st, authOpt)

	srv := &server{st: st, am: am}
	handler := httpx.SecurityHeaders(middleware.RequestCtx(srv.routes()))

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	host, port := splitHostPort(cfg.ListenAddr)
	var fingerprint string
	if cfg.TLSMode == config.TLSModeSelf {
		cert, fp, err := tlsx.EnsureSelfSigned(cfg.DataDir, host, port)
		if err != nil {
			return err
		}
		fingerprint = fp
		httpSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}

	scheme := "https"
	if cfg.TLSMode == config.TLSModeNone {
		scheme = "http"
	}
	log.Printf("DockDeck Hub 监听 %s://%s", scheme, cfg.ListenAddr)
	if fingerprint != "" {
		log.Printf("TLS 证书指纹(SHA-256)：%s", fingerprint)
	}
	log.Printf("数据库：%s  主密钥：%s", cfg.DBPath, cfg.MasterKeyPath)

	errCh := make(chan error, 1)
	go func() {
		if cfg.TLSMode == config.TLSModeSelf {
			errCh <- httpSrv.ListenAndServeTLS("", "") // 证书已在 TLSConfig
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()

	// 等待关闭信号或监听错误。
	stop := make(chan struct{})
	go func() {
		waitForShutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		close(stop)
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-stop:
	}
	log.Printf("hub 已优雅关闭")
	return nil
}

// server 聚合依赖，避免全局变量。
type server struct {
	st *store.Store
	am *auth.Manager
}

// routes 用 Go 1.22 的方法+路径通配注册路由。
func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// 登录/建号入口加 IP 限流（突发 10，每 30s 补 1）。
	rl := middleware.NewRateLimit(10, 30*time.Second)

	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// 可选认证：未登录也返回 setup_required，供前端首启建号判断。
	mux.Handle("GET /api/v1/auth/me", s.am.TryAuth(http.HandlerFunc(s.am.Me)))

	mux.Handle("POST /api/v1/auth/login", rl.LoginRateLimit(http.HandlerFunc(s.am.Login)))
	mux.Handle("POST /api/v1/auth/setup", rl.LoginRateLimit(http.HandlerFunc(s.am.Setup)))
	mux.Handle("POST /api/v1/auth/logout", s.am.RequireAuth(http.HandlerFunc(s.am.Logout)))
	// 改密自身的审计在 auth.ChangePassword 内完成，不再套中间件以免重复记录。
	mux.Handle("POST /api/v1/auth/password", s.am.RequireAuth(http.HandlerFunc(s.am.ChangePassword)))

	// 后续阶段填充的受保护占位，保证前端骨架可联调。
	mux.Handle("GET /api/v1/servers", s.am.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, []any{})
	})))

	// 前端 SPA 兜底：非 /api 路径交静态资源，未命中回 index.html。
	static := assets.Handler()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "接口不存在")
			return
		}
		static.ServeHTTP(w, r)
	})
	return mux
}

func waitForShutdown() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func splitHostPort(addr string) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 443
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return h, 443
	}
	return h, n
}
