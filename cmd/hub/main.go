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
	}, middleware.Audit(s.st, "token.create", s.tokens.CreateHandler))

	tbl.Add(route.Entry{
		Method: "DELETE", Path: "/api/v1/api-tokens/{id}", AuthMode: route.AuthSession,
		Summary: "撤销 API Token", Tags: []string{"Tokens"},
	}, middleware.Audit(s.st, "token.revoke", s.tokens.RevokeHandler))

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
	}, middleware.Audit(s.st, "metrics.latest", s.metricsLatest))

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
	}, middleware.Audit(s.st, "metrics.overview", s.metricsOverview))

	// ---- S3.4 容器转发（实时到 Agent）----
	// 与上面指标端点的分工：那些读 Hub 自己的库，这些实时转发。
	// 失败的 502 一律来自 Agent 侧（不可达/异常），Hub 自身故障是 500。
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/containers", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "容器清单", Tags: []string{"Containers"},
		Desc: "实时取自 Agent 的 Engine 原生清单（未停止的默认不含，all=true 含已停止）。" +
			"空清单编码为 [] 而非 null。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "all", Desc: "含已停止容器（true/false，或 1/0；省略为 false）", Required: false, Example: "false", SchemaType: "boolean"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/containers.go containerList",
	}, middleware.Audit(s.st, "container.list", s.containerList))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/containers/{cid}", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "容器详情", Tags: []string{"Containers"},
		Desc: "Engine inspect JSON 原样透传（信封的 data 层），Hub 不做字段裁剪。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "cid", Desc: "容器 ID 或名字", Required: true, Example: "9f3a2b1c4d5e"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "容器不存在（Agent/Engine 报 404）"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/containers.go containerInspect",
	}, middleware.Audit(s.st, "container.inspect", s.containerInspect))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/containers/{cid}/logs", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "容器日志", Tags: []string{"Containers"}, NoEnvelope: true, ContentType: "text/plain",
		Desc: "裸流透传，无信封：默认 stdout/stderr 合流文本（text/plain）；export=1 时 Engine 原始字节流下载" +
			"（application/octet-stream + Content-Disposition，docker CLI 可直接消费）。" +
			"follow=1 为长连接，逐块 Flush，浏览器断开即结束。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "cid", Desc: "容器 ID 或名字", Required: true, Example: "9f3a2b1c4d5e"},
			{Name: "follow", Desc: "持续跟随（仅认 1）", Required: false, Example: "1", SchemaType: "boolean"},
			{Name: "tail", Desc: "尾部行数：all 或纯数字", Required: false, Example: "200"},
			{Name: "since", Desc: "起始时间（RFC3339 或 Unix 秒）", Required: false, Example: "2026-10-14T00:00:00Z", SchemaType: "string", SchemaFmt: "date-time"},
			{Name: "until", Desc: "截止时间（RFC3339 或 Unix 秒）", Required: false, Example: "2026-10-14T08:00:00Z", SchemaType: "string", SchemaFmt: "date-time"},
			{Name: "timestamps", Desc: "每行带时间戳（仅认 1）", Required: false, Example: "1", SchemaType: "boolean"},
			{Name: "export", Desc: "原始字节流导出下载（仅认 1）", Required: false, Example: "1", SchemaType: "boolean"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/containers.go containerLogs",
	}, middleware.Audit(s.st, "container.logs", s.containerLogs))

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/{id}/containers/{cid}/{action}", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "容器操作", Tags: []string{"Containers"},
		Desc: "action ∈ start|stop|restart|pause|unpause|kill|resize，未知动作 404。" +
			"stop 请求体 {\"timeout_sec\":N}（SIGTERM 后等待秒数，省略用引擎默认）；" +
			"kill 请求体 {\"signal\":\"SIGTERM\"}（白名单内信号，空用引擎默认）；" +
			"resize 查询串 ?h=&w=（行列正整数，Engine 原生约定）；其余动作无选项。" +
			"写操作不重试（Agent 侧尚无幂等去重），一次失败即如实回报。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "cid", Desc: "容器 ID 或名字", Required: true, Example: "9f3a2b1c4d5e"},
			{Name: "action", Desc: "start|stop|restart|pause|unpause|kill|resize", Required: true, Example: "restart"},
			{Name: "h", Desc: "resize 行数（正整数）", Required: false, Example: "40", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "w", Desc: "resize 列数（正整数）", Required: false, Example: "120", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用，或命中 Agent 自身容器（不碰自身红线）"},
			{Status: 404, Desc: "容器不存在或未知操作"},
			{Status: 409, Desc: "容器状态不允许该操作（Engine 报 409）"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/containers.go containerAction",
	}, middleware.AuditFunc(s.st, func(r *http.Request) string {
		return "container." + r.PathValue("action")
	}, s.containerAction))

	tbl.Add(route.Entry{
		Method: "DELETE", Path: "/api/v1/servers/{id}/containers/{cid}", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "删除容器", Tags: []string{"Containers"},
		Desc: "force=1 强删运行中的容器；v=1 连同匿名卷删除（查询串与请求体取并集，与 Agent 侧同口径）。" +
			"具名卷与 bind 目录不归 Engine 管，不在此清理。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "cid", Desc: "容器 ID 或名字", Required: true, Example: "9f3a2b1c4d5e"},
			{Name: "force", Desc: "强删运行中的容器（仅认 1）", Required: false, Example: "1", SchemaType: "boolean"},
			{Name: "v", Desc: "连同匿名卷删除（仅认 1）", Required: false, Example: "1", SchemaType: "boolean"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用，或命中 Agent 自身容器（不碰自身红线）"},
			{Status: 404, Desc: "容器不存在"},
			{Status: 409, Desc: "Engine 拒绝删除（如运行中未 force）"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/containers.go containerRemove",
	}, middleware.Audit(s.st, "container.remove", s.containerRemove))

	// ---- S4.1 镜像转发（实时到 Agent，scope 复用容器 scope）----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/images", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "镜像清单", Tags: []string{"Images"},
		Desc: "实时取自 Agent 的 Engine 原生清单（all=true 含中间层）。" +
			"dangling=true 只列悬空镜像（无 tag），是 prune dry-run 两段式的第一段。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "all", Desc: "含中间层镜像（true/false，或 1/0；省略为 false）", Required: false, Example: "false", SchemaType: "boolean"},
			{Name: "dangling", Desc: "只列悬空镜像（true/false，或 1/0；省略为 false）", Required: false, Example: "false", SchemaType: "boolean"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/images.go imageList",
	}, middleware.Audit(s.st, "image.list", s.imageList))

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/{id}/images/pull", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "拉取镜像", Tags: []string{"Images"},
		Desc: "请求体 {\"ref\":\"alpine:3.20\"}。长操作（大镜像以分钟计），Hub 侧超时 10 分钟。" +
			"响应是汇总结果（最终状态/层清单），Engine 流内业务失败映射 502。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "ref 为空或字符集非法（本地校验，不出门）"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达，或 Engine 流内报错（如仓库不存在）"},
		},
		InternalRef: "cmd/hub/images.go imagePull",
	}, middleware.Audit(s.st, "image.pull", s.imagePull))

	tbl.Add(route.Entry{
		Method: "DELETE", Path: "/api/v1/servers/{id}/images/{ref...}", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "删除镜像", Tags: []string{"Images"},
		Desc: "ref 形如 alpine:3.20 或镜像 ID（可含 /，如 library/nginx:latest）。" +
			"被容器占用的镜像 Engine 回 409；强删（先处理容器）不在本端点，是调用方决策链。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "ref", Desc: "镜像引用（name:tag 或 ID）", Required: true, Example: "alpine:3.20"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "镜像不存在"},
			{Status: 409, Desc: "镜像被容器占用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/images.go imageDelete",
	}, middleware.Audit(s.st, "image.delete", s.imageDelete))

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/{id}/images/prune", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "清理悬空镜像", Tags: []string{"Images"},
		Desc: "真删（dry-run 两段式的第二段：前端先 GET images?dangling=true 展示清单，" +
			"用户确认后才调本端点）。返回 Engine 的清理清单与回收字节数。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/images.go imagePrune",
	}, middleware.Audit(s.st, "image.prune", s.imagePrune))

	// ---- S4.7 Compose 转发（Agent 侧 S4.2 已落地，Hub 侧补齐转发）----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/compose", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "Compose 项目清单", Tags: []string{"Compose"},
		Desc: "实时取自 Agent：在 Agent 配置的白名单目录（DD_AGENT_COMPOSE_DIRS）内递归发现 compose 文件，" +
			"并逐个查运行状态。白名单外的项目一律不可见（项目名只经白名单解析，不进 shell）。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/compose.go composeList",
	}, middleware.Audit(s.st, "compose.list", s.composeList))

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/{id}/compose/{project}/{action}", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "Compose 写动作", Tags: []string{"Compose"},
		Desc: "action ∈ up|down|restart|pull；非法动词由 Hub 侧 agentcli 拦下（400，不出门），未知项目 404。" +
			"响应透传 compose 原生输出。**compose 没有 prune 子命令**——镜像清理走镜像端点，不在此暴露。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "project", Desc: "项目名（白名单内目录名）", Required: true, Example: "watchtower"},
			{Name: "action", Desc: "up | down | restart | pull", Required: true, Example: "up"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "动作不在白名单（本地拦截，不出门）"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "项目不存在或不在白名单"},
			{Status: 502, Desc: "Agent 不可达，或 compose 执行失败（如端口冲突）"},
		},
		InternalRef: "cmd/hub/compose.go composeAction",
	}, middleware.Audit(s.st, "compose.action", s.composeAction))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/compose/{project}/logs", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "Compose 项目日志", Tags: []string{"Compose"}, NoEnvelope: true, ContentType: "text/plain",
		Desc: "裸文本流（--no-color）。follow=1 为长连接，逐块 Flush，调用方断开即结束；" +
			"不设 Hub 侧读取超时——流的寿命归调用方 ctx。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "project", Desc: "项目名（白名单内目录名）", Required: true, Example: "watchtower"},
			{Name: "follow", Desc: "持续跟随（仅认 1）", Required: false, Example: "1", SchemaType: "boolean"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "项目名为空（本地拦截，不出门）"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "项目不存在或不在白名单"},
			{Status: 502, Desc: "Agent 不可达或打开流失败"},
		},
		InternalRef: "cmd/hub/compose.go composeLogs",
	}, middleware.Audit(s.st, "compose.logs", s.composeLogs))

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
			"客户端请判 `code` 而非中文文案。\n\n" +
			"**幂等键 `X-DD-Request-Id`**：所有端点接受可选请求头 `X-DD-Request-Id`" +
			"（1-64 字符，限字母数字与 `._-`，非法值 400）。缺省时 Hub 现场生成，" +
			"响应头原样回显。写操作将该键落入审计并转发给 Agent——它用于审计关联与" +
			"排查对账，不做执行级去重（Agent 无状态、写操作不重试）。",
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
