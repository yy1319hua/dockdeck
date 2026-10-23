// Command hub 是 DockDeck 管理面板的服务端。
// 单二进制内嵌前端静态资源，绑定 tailnet / 内网的具体 IP（非 0.0.0.0）提供 HTTPS。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/yy1319hua/dockdeck/internal/agentcli"
	"github.com/yy1319hua/dockdeck/internal/agentlink"
	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/assets"
	"github.com/yy1319hua/dockdeck/internal/auth"
	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/joinauth"
	"github.com/yy1319hua/dockdeck/internal/metricsync"
	"github.com/yy1319hua/dockdeck/internal/middleware"
	"github.com/yy1319hua/dockdeck/internal/openapi"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/route"
	"github.com/yy1319hua/dockdeck/internal/store"
	"github.com/yy1319hua/dockdeck/internal/tlsx"
	"github.com/yy1319hua/dockdeck/internal/version"
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
	// 生成模式：复用同一份路由声明产出 spec，不建库、不监听、不碰线上目录。
	if cfg.DumpOpenAPI != "" {
		return dumpOpenAPI(cfg.DumpOpenAPI)
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	masterKey, err := crypto.LoadOrCreateMasterKey(cfg.MasterKeyPath)
	if err != nil {
		return err
	}

	if cfg.AllowInsecureBind {
		log.Printf("hub: ⚠️ 已启用 --allow-insecure-bind，监听 %s 会暴露到所有网卡（含内网与 docker 网桥），请确认这是有意为之", cfg.ListenAddr)
	}

	// 整库恢复（#93 方案①）：若有暂存库，先在开库前完成文件级替换——
	// 替换过程把当前库挪进 backups/，随后 Open 正常向前迁移。
	if applied, err := db.ApplyStagedRestore(cfg.DBPath); err != nil {
		return fmt.Errorf("应用暂存恢复库失败: %w", err)
	} else if applied {
		log.Printf("hub: 已应用暂存恢复库（原库在 data/backups/pre-restore-boot-*）")
	}

	sqlDB, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	st := &store.Store{DB: sqlDB, Key: masterKey}

	// 本机服务器：Hub 与 Docker 引擎同机，不再单独部署 Agent——Hub 在同进程起
	// agent 服务（P1.4），服务器列表里以「本机」出现。注册幂等（type=local，
	// server_id 固定），令牌稳定复用，留给进程内 agent 与 Hub 调用方共用。
	// 失败不阻断启动：远端管理与监控不受影响，只是本机入口缺失。
	if localID, _, err := st.EnsureLocalServer(context.Background(), "http://"+cfg.LocalAgentAddr); err != nil {
		log.Printf("hub: ⚠️ 本机服务器注册失败（本机管理暂不可用）：%v", err)
	} else {
		log.Printf("hub: 本机服务器 #%d 已注册（agent 地址 http://%s）", localID, cfg.LocalAgentAddr)
	}

	authOpt := auth.DefaultOptions()
	authOpt.Secure = cfg.TLSMode != config.TLSModeNone
	am := auth.NewManager(st, authOpt)

	srv := &server{
		st:     st,
		am:     am,
		tokens: apitoken.NewAPI(sqlDB),
		// 登录/建号入口 IP 限流：突发 10，每 30s 补 1。
		rl: middleware.NewRateLimit(10, 30*time.Second),
		// S7.8：备份下载的临时目录与 TLS 指纹回显都从这里取。
		dataDir: cfg.DataDir,
		// 反向长连接：上限 512，超出握手直接 503。
		links: agentlink.NewRegistry(512),
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
		srv.tlsFingerprint = fingerprint // S7.8：指纹端点从这里读，与启动日志同一来源
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

	// 指标轮询器：没有它 metrics_latest / metrics_snapshots 永远不会有数据（GAP-9）。
	// 与 HTTP 服务同生命周期——收到关闭信号时先停采集，再关监听。
	pollCtx, stopPoll := context.WithCancel(context.Background())
	defer stopPoll()
	poller := metricsync.New(
		st,
		time.Duration(cfg.MetricsIntervalSec)*time.Second,
		time.Duration(cfg.MetricsRetentionDays)*24*time.Hour,
		log.Printf,
		metricsync.WithAuditRetention(time.Duration(cfg.AuditRetentionDays)*24*time.Hour),
		// 采集也走同一条链路：有反向连接就用，没有回落 HTTP。
		metricsync.WithLink(func(id int64) agentcli.Doer {
			if c := srv.links.Conn(id); c != nil {
				return c
			}
			return nil
		}),
	)
	go poller.Run(pollCtx)

	// 等待关闭信号或监听错误。
	stop := make(chan struct{})
	go func() {
		waitForShutdown()
		stopPoll() // 先停采集，避免关闭期间还在写库
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
	srv.links.CloseAll()
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

	// S7.8 设置页依赖：dataDir 供整库备份落临时文件（<dataDir>/backups/）；
	// tlsFingerprint 供指纹端点回显（仅自签模式有值，其余模式空串）。
	dataDir        string
	tlsFingerprint string

	// links 是反向长连接注册表：有活跃连接时 agentFor 优先走它，没有则回落 HTTP。
	links *agentlink.Registry
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
		case route.AuthJoin:
			wrapped = joinauth.Verify(s.st)(h)
		default: // route.AuthScope
			wrapped = apitoken.Require(s.am.ServeSession, s.tokens, e.Scope)(h)
		}
		if e.RateLimited {
			wrapped = s.rl.LoginRateLimit(wrapped)
		}
		return wrapped
	})
	s.table = tbl

	// 反向长连接握手（101 升级）：不进 route.Table——
	// OpenAPI 无法正确描述 WebSocket，硬塞进去只会让生成器与一致性测试都变复杂。
	// 鉴权在 handler 内自做（Bearer + HMAC + 时间窗）。
	mux.Handle("GET "+agentlink.LinkPath, &agentlink.Handler{Store: s.st, Reg: s.links})

	// ---- 公开 ----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/health", AuthMode: route.AuthNone,
		Summary: "健康检查", Tags: []string{"Meta"},
		Desc: "进程存活探针：恒返回 200，不校验身份、不查库。部署脚本与负载均衡用它判断 Hub 是否起来。",
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
		Method: "GET", Path: "/api/v1/scopes", OKSchema: "ScopeList", AuthMode: route.AuthNone,
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
		Desc: "删除当前会话并清 cookie；重复调用幂等（已登出再登出仍 200）。",
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
		Desc:   "按 ID 撤销（不是按名字）。撤销后该 Token 立即失效；目标不存在返回 404。",
		Params: []route.Param{{Name: "id", Desc: "Token ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"}},
	}, middleware.Audit(s.st, "token.revoke", s.tokens.RevokeHandler))

	// ---- S2.7 监控查询 ----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/metrics", OKSchema: "MetricsEnvelope", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadMetrics,
		Summary: "实时指标快照", Tags: []string{"Servers"},
		Desc:   "返回最近一次采集的服务器指标与每容器占用。服务器不存在或从未采集时 404。",
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
		Desc: "返回 [from,to] 内服务器与容器历史点（升序）。默认最近 1 小时，窗口上限 8 天（保留期 7 天），" +
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
		Method: "GET", Path: "/api/v1/servers/{id}/containers", OKSchema: "ContainerList", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
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
		Method: "GET", Path: "/api/v1/servers/{id}/containers/{cid}/processes", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		OKSchema: "ContainerProcessList",
		Summary:  "容器进程", Tags: []string{"Containers"},
		Desc: "容器内进程明细（#91）：Agent 从宿主 /proc 按 cgroup 归属容器后返回，需 Agent 以 pid:host 运行。" +
			"空清单编码为 [] 而非 null。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "cid", Desc: "容器 ID 或名字", Required: true, Example: "9f3a2b1c4d5e"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "容器不存在（Agent 报 404）"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/containers.go containerProcesses",
	}, middleware.Audit(s.st, "container.processes", s.containerProcesses))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/containers/{cid}/processes/{pid}", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		OKSchema: "ContainerProcessDetail",
		Summary:  "进程详情", Tags: []string{"Containers"},
		Desc: "单个进程的 exe/cwd（#91 v2）。pid 须归属该容器，越权/不存在统一 404。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "cid", Desc: "容器 ID 或名字", Required: true, Example: "9f3a2b1c4d5e"},
			{Name: "pid", Desc: "进程 PID（十进制正整数）", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "进程不存在或不属于该容器"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/containers.go containerProcessDetail",
	}, middleware.Audit(s.st, "container.processDetail", s.containerProcessDetail))

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
			{Name: "action", Desc: "容器动作", Required: true, Example: "restart",
				Enum: []string{"start", "stop", "restart", "pause", "unpause", "kill", "resize"}},
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
			{Name: "confirm", Desc: "破坏性确认：回传目标容器 ID 或名字以确认，与 cid 完全一致才放行（query 与 body 取并集，空值 400）", Required: true, Example: "9f3a2b1c4d5e", SchemaType: "string"},
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
		Method: "GET", Path: "/api/v1/servers/{id}/images", OKSchema: "ImageList", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
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
			{Name: "ref", Desc: "镜像引用：name:tag 或镜像 ID，可含 / 与 :（如 library/nginx:latest）。尾通配符容许斜杠，无需转义", Required: true, Example: "alpine:3.20"},
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

	// ---- S7.9 卷与网络转发（只读两条，scope 复用容器 scope）----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/volumes", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "数据卷清单（只读）", Tags: []string{"Volumes"},
		Desc: "实时取自 Agent 的 Engine 原生卷清单。" +
			"UsageData 可能为 null（驱动未统计），null 表示「Engine 没给」而非「0 个容器在用」——" +
			"把这两者混同会让用户误判卷可删。卷不提供创建/删除/prune：删除卷即删数据，" +
			"且 prune 的 dangling 判定由 Engine 做出、误判不可逆。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/resources.go volumeList",
	}, middleware.Audit(s.st, "volume.list", s.volumeList))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/networks", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "容器网络清单（只读）", Tags: []string{"Networks"},
		Desc: "实时取自 Agent 的 Engine 原生网络清单。" +
			"每项自带 Containers 映射（接入容器名与 IPv4/Mac），故「哪些容器接入了」不需要额外请求——" +
			"与卷只给 RefCount 计数不同，差异来自 Engine 的数据形态。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/resources.go networkList",
	}, middleware.Audit(s.st, "network.list", s.networkList))

	// ---- S4.7 Compose 转发（Agent 侧 S4.2 已落地，Hub 侧补齐转发）----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/compose", OKSchema: "ComposeList", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
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
			{Name: "action", Desc: "Compose 动作", Required: true, Example: "up",
				Enum: []string{"up", "down", "restart", "pull"}},
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

	// ---- S4.7(2/2) Compose YAML 文件读写转发（Agent 侧 S4.3 已落地）----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/compose/{project}/file", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "读取 Compose YAML", Tags: []string{"Compose"},
		Desc: "返回项目 compose 文件的绝对路径与原文。只读——要改走下面的 diff 预览 + PUT 两段式。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "project", Desc: "项目名（白名单内目录名）", Required: true, Example: "watchtower"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "项目名为空（本地拦截，不出门）"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "项目不存在或不在白名单"},
			{Status: 502, Desc: "Agent 不可达或读文件失败"},
		},
		InternalRef: "cmd/hub/compose.go composeFileGet",
	}, middleware.Audit(s.st, "compose.file.read", s.composeFileGet))

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/{id}/compose/{project}/diff", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "Compose YAML 修改预览（diff）", Tags: []string{"Compose"},
		Desc: "请求体 {\"content\":\"...\"}。返回新内容相对当前文件的 unified diff——**只算不写**，" +
			"不落盘、不产生备份，是「先看 diff → 再确认写」两段式的第一段。" +
			"scope 归 write：它是写入流程的一段（带请求体的 POST），与 PUT 同一把钥匙。",
		Body: &route.Body{Type: "json"},
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "project", Desc: "项目名（白名单内目录名）", Required: true, Example: "watchtower"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "请求体不合法，或项目名为空（本地拦截，不出门）"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "项目不存在或不在白名单"},
			{Status: 502, Desc: "Agent 不可达或计算 diff 失败"},
		},
		InternalRef: "cmd/hub/compose.go composeFileDiff",
	}, middleware.Audit(s.st, "compose.diff", s.composeFileDiff))

	tbl.Add(route.Entry{
		Method: "PUT", Path: "/api/v1/servers/{id}/compose/{project}/file", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "写入 Compose YAML", Tags: []string{"Compose"},
		Desc: "请求体 {\"content\":\"...\"}。Agent 侧**写前先把当前内容备份为 <file>.bak.<RFC3339>**，" +
			"备份落盘成功才覆盖写（红线「备份优先于变更」）；响应回传备份路径供回滚。" +
			"建议先用上面的 diff 端点预览再调本端点。",
		Body: &route.Body{Type: "json"},
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "project", Desc: "项目名（白名单内目录名）", Required: true, Example: "watchtower"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "请求体不合法，或项目名为空（本地拦截，不出门）"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "项目不存在或不在白名单"},
			{Status: 502, Desc: "Agent 不可达，或备份/写入失败"},
		},
		InternalRef: "cmd/hub/compose.go composeFilePut",
	}, middleware.Audit(s.st, "compose.file.write", s.composeFilePut))

	// ---- S7.11 Compose 写前备份转发：列举/读取只读（read:containers）、
	// 恢复是覆盖写（write:containers，与写 YAML 同款）。三条都落审计。----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/compose/{project}/backups", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "Compose 写前备份清单", Tags: []string{"Compose"},
		Desc: "列出该 compose 文件的写前备份（<文件>.bak.<定宽奈秒时间戳>），按时间倒序（最新在前）。" +
			"只回文件名/大小/时间，**不回宿主机绝对路径**（与文件管理模块同口径）。" +
			"备份由每次 PUT 写 YAML 自动生成；系统自动轮转——每个 compose 文件只保留最近 20 份，更旧的自动删除（用户侧无删除入口）。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "project", Desc: "compose 项目名（白名单内目录名）", Required: true, Example: "web"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/compose.go composeBackupsList",
	}, middleware.Audit(s.st, "compose.backups.list", s.composeBackupsList))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/compose/{project}/backups/content", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadContainers,
		Summary: "读取 Compose 备份正文", Tags: []string{"Compose"},
		Desc: "读指定备份的正文，供前端预览与 diff。备份名走查询串 file（含 RFC3339 的 + 与 :，" +
			"放进路径段要处理编码差异，故不进路径）。预览不另开端点：拿到正文后调既有的 " +
			"compose diff 端点算 unified diff，与现有两段式写入同款。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "project", Desc: "compose 项目名", Required: true, Example: "web"},
			{Name: "file", Desc: "备份文件名（须以 <compose 文件名>.bak. 开头）", Required: true, Example: "docker-compose.yml.bak.2026-10-17T10:00:00.000000001+08:00"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "缺少 file 参数，或备份名不合法/越界"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/compose.go composeBackupsContent",
	}, middleware.Audit(s.st, "compose.backups.read", s.composeBackupsContent))

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/{id}/compose/{project}/backups/restore", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteContainers,
		Summary: "用备份覆盖当前 Compose 文件", Tags: []string{"Compose"},
		Desc: "请求体的 file 字段填备份文件名。恢复**本身就是一次覆盖写**，故 Agent 侧" +
			"会先把「恢复前的当前内容」备份成新的 .bak.<定宽奈秒时间戳> 再覆盖——否则回滚是单向门：" +
			"恢复到旧版本后就回不去了。响应回传那份新备份的文件名（仅文件名，不含目录）。" +
			"注意：改 YAML 不会直接改动运行中的容器，需再执行 compose up 才生效。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "project", Desc: "compose 项目名", Required: true, Example: "web"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "缺少 file 字段，或备份名不合法/越界"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/compose.go composeBackupsRestore",
	}, middleware.Audit(s.st, "compose.backups.restore", s.composeBackupsRestore))

	// ---- S5.5 文件管理转发（Agent 侧 S5.1–S5.4/S5.6 已落地，Hub 侧补齐转发+审计）----
	// read:files 覆盖 list/stat/content（读）；write:files 覆盖 content 写与动作（写）。
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/files", OKSchema: "FilesList", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadFiles,
		Summary: "文件清单", Tags: []string{"Files"},
		Desc: "实时取自 Agent：在配置的根白名单内递归列举目录内容；空清单编码为 [] 而非 null。" +
			"root 缺省 0（首个根），path 缺省根自身，depth 缺省 1（只列直接子项）。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "root", Desc: "根白名单序号（缺省 0）", Required: false, Example: "0", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "path", Desc: "相对根的路径（可空，表示根自身）", Required: false, Example: "app/config"},
			{Name: "depth", Desc: "递归深度（缺省 1）", Required: false, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/files.go fileList",
	}, middleware.Audit(s.st, "file.list", s.fileList))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/files/roots", OKSchema: "FilesRoots", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadFiles,
		Summary: "文件根列表", Tags: []string{"Files"},
		Desc: "返回该服务器 --fs-root 白名单的序号、宿主机绝对路径与当前是否存在。" +
			"面板据此把「根 0/1/2」显示为可读路径；路径不可用的根应置灰并给移除入口。" +
			"老 Agent 无此端点（404），面板回退为按序号探测。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/files.go fileRoots",
	}, middleware.Audit(s.st, "file.roots", s.fileRoots))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/files/stat", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadFiles,
		Summary: "文件元信息", Tags: []string{"Files"},
		Desc: "取文件或目录的元信息（大小、类型、修改时间、是否被截断）。不返回正文——正文走 content 流式下载。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "root", Desc: "根白名单序号（缺省 0）", Required: false, Example: "0", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "path", Desc: "相对根的路径", Required: false, Example: "app/config.yaml"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "路径不存在"},
			{Status: 502, Desc: "Agent 不可达或返回异常"},
		},
		InternalRef: "cmd/hub/files.go fileStat",
	}, middleware.Audit(s.st, "file.stat", s.fileStat))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/files/content", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadFiles,
		Summary: "文件正文下载", Tags: []string{"Files"}, NoEnvelope: true, ContentType: "application/octet-stream",
		Desc: "裸流透传，无信封：正文可能是任意二进制。元信息走响应头（X-DD-File-Path / X-DD-File-Size /" +
			" X-DD-Truncated，截断标记用于提示客户端「文件比这更长」）。可按单文件上限截断。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "root", Desc: "根白名单序号（缺省 0）", Required: false, Example: "0", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "path", Desc: "相对根的路径", Required: false, Example: "app/config.yaml"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "路径不存在"},
			{Status: 502, Desc: "Agent 不可达或打开流失败"},
		},
		InternalRef: "cmd/hub/files.go fileContent",
	}, middleware.Audit(s.st, "file.content", s.fileContent))

	tbl.Add(route.Entry{
		Method: "PUT", Path: "/api/v1/servers/{id}/files/content", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteFiles,
		Summary: "写入文件内容", Tags: []string{"Files"},
		Desc: "请求体即文件内容（不经信封）。Agent 侧写前先把当前内容备份为 <file>.bak.<RFC3339>，" +
			"备份落盘成功才覆盖写，响应回传备份路径供回滚。上传上限由共享常量 MaxFileUploadBytes（16 MiB）" +
			"约束：Hub 与 Agent 同源，避免「Hub 放行、Agent 413」。",
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "root", Desc: "根白名单序号（缺省 0）", Required: false, Example: "0", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "path", Desc: "相对根的路径", Required: false, Example: "app/config.yaml"},
		},
		Responses: []route.Response{
			{Status: 403, Desc: "服务器已禁用，或命中敏感黑名单/越界路径"},
			{Status: 404, Desc: "路径不存在"},
			{Status: 413, Desc: "上传内容超过单文件上限"},
			{Status: 502, Desc: "Agent 不可达，或备份/写入失败"},
		},
		InternalRef: "cmd/hub/files.go fileContentPut",
	}, middleware.Audit(s.st, "file.write", s.fileContentPut))

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/{id}/files/{action}", AuthMode: route.AuthScope, Scope: apitoken.ScopeWriteFiles,
		Summary: "文件写动作", Tags: []string{"Files"},
		Desc: "action ∈ mkdir | delete | rename；body {\"action\",\"path\",\"dest\"}（rename 需 dest）。" +
			"动作名取 URL 段，与请求体必须一致。非法动作由 Hub 侧 agentcli 拦下（400，不出门）。" +
			"写前备份、路径白名单与敏感黑名单均在 Agent 侧。",
		Body: &route.Body{Type: "json"},
		Params: []route.Param{
			{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "action", Desc: "文件动作", Required: true, Example: "mkdir",
				Enum: []string{"mkdir", "delete", "rename"}},
			{Name: "root", Desc: "根白名单序号（缺省 0）", Required: false, Example: "0", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "动作不在白名单，或动作名与请求体不一致（本地拦截，不出门）"},
			{Status: 403, Desc: "服务器已禁用，或命中敏感黑名单/越界路径"},
			{Status: 404, Desc: "路径不存在"},
			{Status: 409, Desc: "目标已存在或目录非空"},
			{Status: 502, Desc: "Agent 不可达或执行失败"},
		},
		InternalRef: "cmd/hub/files.go fileAction",
	}, middleware.Audit(s.st, "file.action", s.fileAction))

	// ---- S7.7 审计查询（audit_logs 自 S1 只写不读，本期补查询端点；
	// read:audit scope 早在 S1.10 预留而无端点消费，本期起有）----
	// 鉴权用 AuthScope：会话用户不受 scope 限（ServeSession 不查 scope，
	// 见 internal/auth），API Token 必须勾 read:audit，否则 403 scope_denied。
	// 刻意不包 middleware.Audit：高频读留痕会淹掉真操作记录（方案 Q4）。
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/audit_logs", OKSchema: "AuditLogsPageEnvelope", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadAudit,
		Summary: "审计日志查询", Tags: []string{"Audit"},
		Desc: "倒序（最新在前）+ before_id 游标分页：limit 缺省 100、上限 1000；" +
			"响应 next_before=0 表示没有更多，否则作为下一次请求的 before_id。" +
			"过滤维度：from/to（RFC3339，缺省最近 7 天）、server_id、action 前缀" +
			"（如 container 命中 container.* 全部）、result（ok|error）。" +
			"本端点自身不写审计行。",
		Params: []route.Param{
			{Name: "from", Desc: "起始时间（RFC3339，含）", Required: false, Example: "2026-10-15T00:00:00Z", SchemaType: "string"},
			{Name: "to", Desc: "截止时间（RFC3339，含）", Required: false, Example: "2026-10-16T00:00:00Z", SchemaType: "string"},
			{Name: "server_id", Desc: "按服务器过滤（缺省不限）", Required: false, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "action", Desc: "动作前缀（如 container、token、auth）", Required: false, Example: "container", SchemaType: "string"},
			{Name: "result", Desc: "结果（缺省不限）", Required: false, Example: "error", SchemaType: "string"},
			{Name: "before_id", Desc: "游标：只返回 id 更小的行", Required: false, Example: "0", SchemaType: "integer", SchemaFmt: "int64"},
			{Name: "limit", Desc: "每页条数（缺省 100，上限 1000）", Required: false, Example: "100", SchemaType: "integer", SchemaFmt: "int64"},
		},
		Responses: []route.Response{
			{Status: 400, Desc: "参数非法（时间格式 / limit 非正整数 / action 字符集 / result 取值）"},
			{Status: 403, Desc: "Token 缺少 read:audit scope（会话用户不受此限）"},
		},
		InternalRef: "cmd/hub/audit.go auditLogs",
	}, s.auditLogs)

	// ---- S7.8 设置页（方案 Q1–Q5 全 A）----
	// 四枚一律 AuthSession：泄漏的 API Token 不得改安全配置、不得拖库
	// （与 Token 管理同口径）。PUT 与 backup 包 Audit——改安全配置与
	// 拖库都必须是审计表里的一等公民；两枚 GET 不包：读配置与读指纹
	// 没有副作用，留痕只会淹掉真操作记录（Q4 口径）。
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/settings/protected-containers", AuthMode: route.AuthSession,
		Summary: "读取保护名单", Tags: []string{"Settings"},
		Desc: "返回正则数组与 using_default：名单为空时容器守卫回落默认名单（^dockdeck-），" +
			"UI 据此提示「当前为默认名单」。S7.8 起存 JSON 数组文本（正则含逗号不被切碎）；" +
			"S7.8 前的逗号分隔存量读取时兼容，不迁移。",
		Responses: []route.Response{
			{Status: 401, Desc: "未登录，或携带 API Token（本端点仅会话可调）"},
		},
		InternalRef: "cmd/hub/settings.go getProtectedContainers",
	}, s.getProtectedContainers)

	tbl.Add(route.Entry{
		Method: "PUT", Path: "/api/v1/settings/protected-containers", AuthMode: route.AuthSession,
		Summary: "覆盖写保护名单", Tags: []string{"Settings"}, Body: &route.Body{},
		Desc: "整名单覆盖：逐条编译校验，坏正则 400 并指出第几条，库无写入；" +
			"空数组即清除（回落默认名单）。保存即时生效——容器守卫逐请求读 " +
			"settings，无需重启。",
		Responses: []route.Response{
			{Status: 400, Desc: "请求体不合法，或第 N 条正则编译失败"},
			{Status: 401, Desc: "未登录，或携带 API Token"},
		},
		InternalRef: "cmd/hub/settings.go putProtectedContainers",
	}, middleware.Audit(s.st, "settings.protected.update", s.putProtectedContainers))

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/settings/tls-fingerprint", AuthMode: route.AuthSession,
		Summary: "TLS 证书指纹", Tags: []string{"Settings"},
		Desc: "返回内置自签证书的 SHA-256 指纹（hex，冒号分隔），供浏览器证书固定时人工比对。" +
			"非自签模式（none / 外部终止 TLS）下 fingerprint 为空串、self_signed=false。" +
			"不展示过期时间：指纹是「当下这一枚」的标识，过期归运维在宿主机侧处理。",
		Responses: []route.Response{
			{Status: 401, Desc: "未登录，或携带 API Token"},
		},
		InternalRef: "cmd/hub/settings.go getTLSFingerprint",
	}, s.getTLSFingerprint)

	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/admin/backup", AuthMode: route.AuthSession,
		NoEnvelope: true, ContentType: "application/vnd.sqlite3",
		Summary: "下载整库快照", Tags: []string{"Settings"},
		Desc: "VACUUM INTO 生成一致快照后流式下载（临时文件中转，用完即删）。" +
			"文件含全部表——用户、会话、审计、受管服务器与各服务器加密令牌；" +
			"一份快照等于一份需要同等保护的数据库副本，仅会话可调正是为此。",
		Responses: []route.Response{
			{Status: 401, Desc: "未登录，或携带 API Token"},
			{Status: 500, Desc: "快照生成或读取失败"},
		},
		InternalRef: "cmd/hub/settings.go backupDB",
	}, middleware.Audit(s.st, "admin.backup", s.backupDB))

	// ---- 整库恢复（仅会话；暂存+重启生效，方案①）----
	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/admin/restore", AuthMode: route.AuthSession,
		NoEnvelope: true, ContentType: "application/vnd.sqlite3",
		Summary: "上传整库备份并暂存", Tags: []string{"Settings"}, Body: &route.Body{Type: "octet-stream"},
		Desc: "上传此前下载的整库备份（SQLite 原始字节）。服务端校验 integrity_check 与 " +
			"schema_migrations 版本后，自动备份当前库（pre-restore-*.db 永久保留），再把合格备份" +
			"暂存为 restore-staged.db——重启 Hub 容器生效。登录密码走 X-DD-Admin-Password 头。" +
			"任一步失败零副作用，当前库不受影响。",
		Responses: []route.Response{
			{Status: 400, Desc: "缺少密码头 / 上传超限 / 备份校验失败"},
			{Status: 401, Desc: "未登录，或携带 API Token"},
			{Status: 403, Desc: "登录密码不正确"},
			{Status: 500, Desc: "暂存失败"},
		},
		InternalRef: "cmd/hub/restore.go serverRestore",
	}, middleware.Audit(s.st, "admin.restore", s.serverRestore))

	// ---- 审计日志手动清理（仅会话；#89，自动清理在 metricsync 轮里）----
	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/admin/audit/prune", AuthMode: route.AuthSession,
		Summary: "清理审计日志", Tags: []string{"Settings"}, Body: &route.Body{},
		Desc: "删除早于 before_days 的审计行（下限 7 天防手滑），返回删除行数。" +
			"动作本身记一条审计（admin.audit_prune）。自动清理见 --audit-retention-days。",
		Responses: []route.Response{
			{Status: 400, Desc: "before_days 缺失或小于 7"},
			{Status: 401, Desc: "未登录，或携带 API Token"},
			{Status: 500, Desc: "清理失败"},
		},
		InternalRef: "cmd/hub/audit_admin.go serverAuditPrune",
	}, middleware.Audit(s.st, "admin.audit_prune", s.serverAuditPrune))

	// 受管服务器清单：直接读 servers 表，不做实时探测（在线状态以 last_seen 为准）。
	// 列表只投影管理所需字段，绝不回带 Agent 令牌等秘密；纳管（写入 servers）机制见`docs/ARCHITECTURE.md` 的纳管设计裁定。
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers", OKSchema: "ServerList", AuthMode: route.AuthScope, Scope: apitoken.ScopeReadMetrics,
		Summary: "受管服务器清单", Tags: []string{"Servers"},
		Desc: "返回全部受管服务器（含已禁用）。列表不暴露 Agent 令牌等秘密字段；在线状态以 last_seen 为准。" +
			"纳管（写入 servers 表）机制见`docs/ARCHITECTURE.md` 的纳管设计裁定。",
		Responses: []route.Response{
			{Status: 403, Desc: "缺少 read:metrics 权限"},
			{Status: 500, Desc: "读取受管服务器清单失败"},
		},
	}, middleware.Audit(s.st, "server.list", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.st.ListServers(r.Context())
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "读取受管服务器清单失败")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, list)
	}))

	// ---- GAP-1 纳管：签发一次性邀请（仅会话）+ Agent 启动自注册（join_secret）----
	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/invite", AuthMode: route.AuthSession, Status: http.StatusCreated,
		Summary: "签发纳管邀请", Tags: []string{"Servers"}, Body: &route.Body{},
		Desc: "生成一枚一次性 join_secret，供 Agent 容器启动时自注册。明文仅本次响应可见。" +
			"仅浏览器会话可调（与 Token 管理同口径：API Token 不得自签邀请，否则权限收不回来）。",
		Responses: []route.Response{
			{Status: 400, Desc: "请求体不合法"},
			{Status: 401, Desc: "未登录"},
		},
	}, middleware.Audit(s.st, "server.invite", s.serverInvite))

	tbl.Add(route.Entry{
		Method: "POST", Path: "/api/v1/servers/enroll", AuthMode: route.AuthJoin, Status: http.StatusCreated,
		RateLimited: true,
		Summary:     "Agent 自注册纳管", Tags: []string{"Servers"}, Body: &route.Body{},
		Desc: "Agent 启动时以 Authorization: Bearer <join_secret> 调用：认领邀请、写入 servers 行，" +
			"并以同一 secret 作为本机长期共享令牌（Hub 侧密文存储）。请求体可带 arch / docker_ver " +
			"（Agent 上报的 GOARCH 与 Engine 版本，写入服务器列表展示；缺失留空不阻断纳管）。" +
			"**幂等**：同一 join_secret 重复调用返回既有服务器 id 并刷新 base_url（重启/改址自愈），" +
			"非空的 arch / docker_ver 一并刷新。",
		Responses: []route.Response{
			{Status: 400, Desc: "请求体不合法"},
			{Status: 401, Desc: "join_secret 无法识别"},
			{Status: 409, Desc: "同名服务器已存在"},
			{Status: 410, Desc: "邀请已过期或已不可用"},
		},
	}, s.serverEnroll)

	// ---- 删除受管服务器（仅会话；物理删除，级联清 metrics，释放名字可重新纳管）----
	tbl.Add(route.Entry{
		Method: "DELETE", Path: "/api/v1/servers/{id}", AuthMode: route.AuthSession,
		Summary: "删除受管服务器", Tags: []string{"Servers"},
		Desc: "物理删除一台受管服务器：metrics 历史级联清空、审计留存（server_id 置空）；" +
			"删除后同名可立即重新纳管。仅浏览器会话可调（与纳管邀请同口径：管理操作不开放 API Token）。",
		Responses: []route.Response{
			{Status: 401, Desc: "未登录，或携带 API Token"},
			{Status: 404, Desc: "服务器不存在"},
			{Status: 500, Desc: "删除失败"},
		},
		InternalRef: "cmd/hub/servers.go serverDelete",
	}, middleware.Audit(s.st, "server.delete", s.serverDelete))

	// ---- 服务器改名（仅会话；Hub 侧显示名，与 Agent 连接无关）----
	tbl.Add(route.Entry{
		Method: "PATCH", Path: "/api/v1/servers/{id}", AuthMode: route.AuthSession,
		Summary: "服务器改名", Tags: []string{"Servers"}, Body: &route.Body{},
		Desc: "修改 Hub 侧显示名（servers.name，全局唯一，去首尾空白，上限 64 字节）。" +
			"不影响 Agent 连接与 reverse:// 标识。仅浏览器会话可调（管理操作不开放 API Token）。",
		Responses: []route.Response{
			{Status: 400, Desc: "名字为空或超 64 字节"},
			{Status: 401, Desc: "未登录，或携带 API Token"},
			{Status: 404, Desc: "服务器不存在"},
			{Status: 409, Desc: "同名服务器已存在"},
			{Status: 500, Desc: "改名失败"},
		},
		InternalRef: "cmd/hub/servers.go serverRename",
	}, middleware.Audit(s.st, "server.rename", s.serverRename))

	// ---- 获取服务器部署配置（仅会话；返回原 token，重部署不新增服务器）----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/servers/{id}/deploy", AuthMode: route.AuthSession,
		Summary: "获取服务器部署配置", Tags: []string{"Servers"},
		Desc: "返回该服务器的原 Agent 令牌与连接信息，供面板「重新生成部署配置」。" +
			"用返回 token 生成的 compose / docker run 重新部署后，Agent 启动 enroll 会命中既有 " +
			"claimed_at + server_id 幂等分支——仍是同一台服务器，不会新增。" +
			"仅浏览器会话可调（与纳管邀请/删除/改名同口径：管理操作不开放 API Token）。" +
			"返回的 token 是长期共享令牌，等同该机控制权，请妥善保管、勿外泄。",
		Responses: []route.Response{
			{Status: 401, Desc: "未登录，或携带 API Token"},
			{Status: 403, Desc: "服务器已禁用"},
			{Status: 404, Desc: "服务器不存在"},
			{Status: 500, Desc: "读取服务器连接信息失败"},
		},
		InternalRef: "cmd/hub/servers.go serverDeploy",
	}, middleware.Audit(s.st, "server.deploy_view", s.serverDeploy))

	// ---- 构建元数据：设置页「版本信息」分区消费（仅会话；不写审计，理由见 handler）----
	tbl.Add(route.Entry{
		Method: "GET", Path: "/api/v1/system/version", AuthMode: route.AuthSession,
		Summary: "构建版本信息", Tags: []string{"System"},
		Desc: "返回 Hub 的构建元数据（version / commit / date），由构建时 -ldflags 注入 internal/version；" +
			"未注入的本地构建回退占位值（dev / unknown）。",
		Responses: []route.Response{
			{Status: 401, Desc: "未登录"},
		},
		InternalRef: "cmd/hub/version.go systemVersion",
	}, s.systemVersion)

	// ---- PLAN-NODE-DOCS B：文档三件套（公开只读，从同一份 spec 动态生成）----
	// 见 cmd/hub/docs.go 顶部注释：为什么挂 /docs 而不走 route.Table。
	mux.HandleFunc("GET /docs/openapi.json", s.docsSpec)
	mux.HandleFunc("GET /docs/llms.txt", s.docsLLMS)
	mux.HandleFunc("GET /docs/api.md", s.docsAPIMD)

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

// spec 生成 OpenAPI 文档。Info.version 由 internal/version 承载、经 Makefile -ldflags 注入；
// 本地未注入构建时回退到 "dev"（见 internal/version）。S9 起制品不再写死 "0.1.0"。
func (s *server) spec() *openapi.Doc {
	return openapi.Generate(openapi.Info{
		Title:   "DockDeck Hub API",
		Version: version.Version,
		Description: "自研多服务器监控与 Docker 管理面板的服务端接口。\n\n" +
			"**访问方式**：默认绑定 tailnet / 内网的具体 IP；确需监听所有网卡必须显式开启 " +
			"`--allow-insecure-bind`（默认拒绝 0.0.0.0）。公网入口由 Cloudflare Tunnel 提供——" +
			"cloudflared 主动出站，不开放任何入站端口。" +
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
