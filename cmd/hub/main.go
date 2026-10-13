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

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/assets"
	"github.com/yy1319hua/dockdeck/internal/auth"
	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/middleware"
	"github.com/yy1319hua/dockdeck/internal/openapi"
	"github.com/yy1319hua/dockdeck/internal/route"
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

	srv := &server{
		st:     st,
		am:     am,
		tokens: apitoken.NewAPI(sqlDB),
		// 登录/建号入口 IP 限流：突发 10，每 30s 补 1。
		rl: middleware.NewRateLimit(10, 30*time.Second),
	}
	handler := middleware.RequestCtx(httpx.SecurityHeaders(srv.routes()))

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
	st     *store.Store
	am     *auth.Manager
	tokens *apitoken.API
	rl     *middleware.RateLimit
	table  *route.Table // 由 routes() 赋值；spec 从这里生成
}

// routes 用 route.Table 声明端点：注册与 OpenAPI 生成共用同一份声明。
// 新增 API 一律走 tbl.Add，不要直接 mux.Handle，否则一致性测试会红。
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	tbl := route.New(mux, func(e route.Entry, h http.Handler) http.Handler {
		var wrapped http.Handler
		switch e.AuthMode {
		case route.AuthNone:
			wrapped = h
		case route.AuthSession:
			wrapped = s.am.RequireAuth(h)
		default:
			wrapped = apitoken.Require(s.am.ServeSession, s.tokens, e.Scope)(h)
		}
		if e.RateLimited {
			wrapped = s.rl.LoginRateLimit(wrapped)
		}
		return wrapped
	})
	s.table = tbl

	// ---- 公开 ----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/health", AuthMode: route.AuthNone,
		Summary: "健康检查", Tags: []string{"Meta"},
	}, func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/openapi.json", AuthMode: route.AuthNone, NoEnvelope: true,
		Summary: "本服务的 OpenAPI 3.0.3 文档", Tags: []string{"Meta"},
		Desc: "由路由登记表实时生成，与实装同源。",
	}, func(w http.ResponseWriter, r *http.Request) {
		b, err := s.spec().JSON()
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "spec 生成失败")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/scopes", AuthMode: route.AuthNone,
		Summary: "API Token 权限项字典", Tags: []string{"Meta"},
		Desc: "列出全部可申请的 scope 及说明。公开：它不含任何本机信息，只是权限名词表。",
	}, apitoken.ScopesHandler)

	// ---- 会话 ----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/auth/me", AuthMode: route.AuthNone,
		Summary: "当前身份", Tags: []string{"Auth"},
		Desc: "未登录也返回 200，并用 setup_required 提示前端引导首启建号，因此挂在 TryAuth 下而非 RequireAuth。",
	}, s.am.TryAuth(http.HandlerFunc(s.am.Me)).ServeHTTP)

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/auth/login", AuthMode: route.AuthNone,
		Summary: "登录", Tags: []string{"Auth"}, Body: &route.Body{}, RateLimited: true,
		Desc: "成功写入会话 cookie。仅限浏览器；API Token 不能调用认证端点。",
	}, s.am.Login)

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/auth/setup", AuthMode: route.AuthNone,
		Summary: "首启建号", Tags: []string{"Auth"}, Body: &route.Body{}, RateLimited: true,
		Desc: "仅当系统尚无任何用户时可用，成功即登录。",
	}, s.am.Setup)

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/auth/logout", AuthMode: route.AuthSession,
		Summary: "登出", Tags: []string{"Auth"},
	}, s.am.Logout)

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/auth/password", AuthMode: route.AuthSession,
		Summary: "改密", Tags: []string{"Auth"}, Body: &route.Body{},
		Desc: "改密成功后撤销该用户全部会话，需重新登录。审计在本端点内完成，故不再套审计中间件。",
	}, s.am.ChangePassword)

	// ---- API Token 管理（仅会话）----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/api-tokens", AuthMode: route.AuthSession,
		Summary: "列出 API Token", Tags: []string{"Tokens"},
		Desc: "只返回元数据：明文与哈希都不出库。",
	}, s.tokens.ListHandler)

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/api-tokens", AuthMode: route.AuthSession, Status: http.StatusCreated,
		Summary: "签发 API Token", Tags: []string{"Tokens"}, Body: &route.Body{},
		Desc: "字段：name（唯一）、scopes（取自 GET /api/v1/scopes）、ttl（7d|30d|90d|1y|never）。" +
			"明文 plaintext 仅本次响应可见。",
	}, s.tokens.CreateHandler)

	tbl.Add(route.Entry{
		Method: "DELETE", Path: "/api/v1/api-tokens/{id}", AuthMode: route.AuthSession,
		Summary: "撤销 API Token", Tags: []string{"Tokens"},
	}, s.tokens.RevokeHandler)

	// ---- S2.7 监控查询 ----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/metrics", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadMetrics,
		Summary: "实时指标快照", Tags: []string{"Servers"},
		Desc:   "返回最近一次采集的主机指标与每容器占用。服务器不存在或从未采集时 404。",
		Params: []route.Param{{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"}},
		Responses: []route.Response{
			{Status: 400, Desc: "非法服务器 ID"},
			{Status: 404, Desc: "服务器不存在或尚无快照"},
		},
		InternalRef: "cmd/hub/metrics.go metricsLatest",
	}, s.metricsLatest)

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/metrics/overview", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadMetrics,
		Summary: "历史指标曲线", Tags: []string{"Servers"},
		Desc: "返回 [from,to] 内主机与容器历史点（升序）。默认最近 1 小时，窗口上限 8 天（保留期 7 天），" +
			"limit 限定点数（默认 720，上限 1440，超出取最新）。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "from", Desc: "起始时间（RFC3339，UTC）", Required: false, Example: "2026-10-14T00:00:00Z", SchemaType: "string", SchemaFmt: "date-time"},
			{Name: "to", Desc: "截止时间（RFC3339，UTC）", Required: false, Example: "2026-10-14T08:00:00Z", SchemaType: "string", SchemaFmt: "date-time"},
			{Name: "limit", Desc: "最多返回的点数", Required: false, Example: "720", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "参数非法（时间格式、to<from、limit 非正整数）"},
			{Status: 404, Desc: "服务器不存在"},
		},
		InternalRef: "cmd/hub/metrics.go metricsOverview",
	}, s.metricsOverview)

	// ---- 占位：S2 起逐步替换为真实实现 ----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadMetrics,
		Summary: "受管主机清单（占位）", Tags: []string{"Servers"},
		Desc: "S2 接入 Agent 前恒返回空数组。",
	}, func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, []any{})
	})

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

// spec 生成 OpenAPI 文档。Info.version 暂为手工常量；S8 接入真实构建版本后由 ldflags 注入。
func (s *server) spec() *openapi.Doc {
	return openapi.Generate(openapi.Info{
		Title:   "DockDeck Hub API",
		Version: "0.1.0",
		Description: "自研多主机监控与 Docker 管理面板的服务端接口。\n\n" +
			"**访问方式**：服务只绑定 Tailscale 网卡地址，不在公网监听；" +
			"浏览器用 cookie 会话，App / 脚本用 `Authorization: Bearer ddk_...`。\n\n" +
			"**两套身份互不信任**：带 Bearer 时只看 Token（即便同时带了 cookie），" +
			"Token 权限由签发时勾选的 scope 决定，scope 不足返回 403；" +
			"登录、改密、Token 管理等端点仅会话可用，Token 调用一律 401。\n\n" +
			"**响应信封**：成功 `{\"data\":…}`，失败 `{\"error\":{\"code\",\"message\"}}`。" +
			"客户端请判 `code` 而非中文文案。",
		License: &openapi.License{
			Name: "PolyForm Noncommercial License 1.0.0",
			URL:  "https://polyformproject.org/licenses/noncommercial/1.0.0",
		},
	}, s.table.Entries())
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
