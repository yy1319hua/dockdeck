# DockDeck 架构

多服务器监控与 Docker 管理面板，监控主线参照哪吒监控（Nezha）。一个 Hub 管 N 台宿主机，
每台宿主机跑一个 Agent 采集主机指标、按需读写 docker.sock 与受限文件目录。
Tailscale 私网或内网反代访问，不开任何公网端口。对外提供 OpenAPI，App 与浏览器 UI 共用同一组端点。

## 功能边界（2026-10-13 用户确认）

| 含 | 不含 |
| --- | --- |
| 主机监控：CPU / 内存 / 磁盘 / 网络流量 / 负载 / 在线率 / 历史曲线 | 服务拨测（HTTP/TCP/Ping/SSL 证书） |
| Docker 容器管理、日志、镜像、Compose | 告警通知（Telegram/邮件/Webhook） |
| 文件管理（限定根白名单内） | Web 终端 / WebSSH |
| 审计日志、OpenAPI + API Token | 免登录公开状态页（状态页须登录才显示） |

「不含」是用户明确排除项，不得以「参照哪吒」为名自行加回。

## 组件

```
浏览器 / App ──HTTPS──► Hub(:8443, 绑 tailnet / 内网的具体 IP（非 0.0.0.0）)
                          ├─ Go 单二进制，前端 go:embed
                          ├─ SQLite（用户/服务器/会话/API令牌/审计/指标快照）
                          ├─ 鉴权：cookie 会话 + Bearer API Token（scope 制）
                          └─ Agent Client ──HTTP+Bearer+HMAC──► Agent(:9527, 每台宿主机)
                                                                 ├─ /proc 主机指标
                                                                 ├─ Docker Engine API (unix socket)
                                                                 └─ 受限文件系统（根白名单）
```

Hub 与 Agent 是两个独立二进制，同一 module、共享 `internal/protocol`。

## 网络与暴露（核心约束）

宿主机有公网/局域网接口，**不能绑 0.0.0.0，不能改防火墙**。容器以 `network_mode: host` 部署（模板见 `deploy/docker-compose.yml`），由**应用自己**
把监听绑到具体 IP（tailscale 网卡或内网网卡）——`config.Validate` 对 0.0.0.0 一律
拒绝；桥接网络里容器 IP 运行时分配、写不进启动参数，故不走 `-p IP:port` 发布
（docker-proxy）而用 host 网络直绑：

```
--listen <BIND_IP>:8443     # Hub（也可用 DD_LISTEN 环境变量）
--listen <BIND_IP>:9527     # Agent（--bind-ifaces tailscale0 校验归属）
```

`BIND_IP` 由部署脚本现取（`tailscale ip -4` 或指定内网 IP），写进 `.env`，不硬编码进仓库。
Agent 启动时校验绑定地址属于 tailscale0，否则拒绝启动（`--allow-insecure-bind` 例外）。

### 反向长连接（Agent 主动连 Hub，2026-10-19 起）

上面的「Hub 拨号到 Agent」在 NAT 后或纯内网（无公网入口）的机器上不成立。
为此新增反向模式（`--reverse` / `DD_AGENT_REVERSE=1`）：

```
Agent ──出站 wss──> Hub /api/v1/agent/link ──复用同一条 WS──> 下发原 HTTP 请求
```

- 连接由 **Agent 主动拨出**，Hub 从不反连；握手复用现有 Bearer + HMAC（±120s 时间窗），
  连接绑定 `servers.id`；同 ID 新连接顶替旧连接（旧连接收 `goaway` 后关闭）。
- Hub 侧 `internal/agentlink` 维护 `server_id → 连接`；`agentcli` 通过 `Doer` 抽象
  在有活跃连接时走 WS、没有则**回落 HTTP**——两种模式共用同一套请求构造/重试/解信封逻辑。
- Agent 侧把帧合成为 `http.Request` 后 `ServeHTTP` 到本地 mux，
  **路由、鉴权、错误映射与 HTTP 模式完全一致**（不复制 handler）。
- 心跳 25s（`--link-ping`），必须明显小于反代空闲超时（Cloudflare 约 100s）。
- M1 只承载非流式请求：请求 ≤1 MiB、响应 ≤8 MiB；流式日志与大文件在 M2。

## 认证

两套并存、互不越权的鉴权，均通过同一 `/api/*` 端点组：

- **cookie 会话（浏览器 UI）**：用户名 + 密码，**Argon2id**（密码永不入库，只存 hash+参数）。
  会话为服务端签发的随机 token，库里存 SHA-256，HttpOnly + SameSite=Strict cookie，
  空闲 8h / 绝对 72h 过期；改密、登出、失败超阈值即撤销。
- **Bearer API Token（App / 自动化）**：仅接受 `Authorization: Bearer <token>`。
  token 为服务端生成的随机串，**库里只存 SHA-256，明文仅在创建时显示一次**；
  带 scope 与过期时间，撤销后立即失效。scope：
  `read:metrics`（主机与容器指标只读）、`read:containers`（容器/镜像/Compose 清单只读）、
  `write:containers`（容器操作）、`read:files`（文件列表/元信息/读取）、`write:files`（文件写）、`read:audit`（审计只读）。
  Token 不能访问 UI 专属端点，cookie 会话不识别 Bearer 头——避免会话固定与 CSRF 面扩散。
- Hub 侧：自签证书 + 指纹写入设置页；`DD_TLS_MODE=self|none`。
  `none` 只允许监听 127.0.0.1，用于反代场景。
- Hub→Agent：每台服务器一个 256bit 随机 token，`Authorization: Bearer`；
  库中以机器主密钥（`/srv/dockdeck/secrets/master.key`，0600，首次启动生成）做 AES-256-GCM 加密存储。
  写操作附带 `X-DD-Timestamp` + `X-DD-Signature`（HMAC-SHA256 over method+path+body），Agent 校验，防重放 ±120s。

## 数据模型（SQLite，WAL）

| 表 | 作用 |
| --- | --- |
| `users` | username、argon2 hash+参数、禁用标记、last_login |
| `sessions` | token hash、user、过期、IP、UA |
| `api_tokens` | token hash、name、scopes、created_by、expires_at、last_used_at、revoked_at |
| `servers` | name、base_url、token(密)、arch、docker 版本、启用、last_seen、最近一次健康结果 |
| `audit_logs` | ts、user/token、server、action、target、detail(JSON)、result、error、IP |
| `metrics_snapshots` | server、ts、cpu%/mem/disk/net、load、进程数、每容器 cpu/mem（滚动 7 天，30s 采样） |
| `settings` | key/value：看板阈值、备份策略、保护名单、文件根白名单等 |

驱动固定 `modernc.org/sqlite`（纯 Go，无 CGO）——这是 `CGO_ENABLED=0` 交叉编译 ARM64/x86_64 的前提。
迁移为 `internal/db/migrations/*.sql` 顺序执行，带 `schema_migrations` 版本表。

## OpenAPI

- **同源生成（S8.1）**：`cmd/hub --dump-openapi <dir>` 复用运行时同一份 `route.Table` 写出
  `docs/openapi.json` 与 `docs/openapi.yaml` 后退出——不建库、不开监听、不碰线上数据目录，
  故 spec 与运行时路由不可能漂移。两份产物都入库，勿手改（改注解 → 重新生成）。
- 运行时经 `GET /api/openapi.json` 暴露同一份 spec（公开端点，依赖 tailnet / 内网隔离，见 GAP-4）。
- **YAML 发射器零依赖**（`internal/openapi/yaml.go`）：先 `Doc.JSON()` 再逐 token 走成保序值树按缩进输出，
  键序与 JSON 逐节一致；多行串用 `|-` 字面块，会被 YAML 类型推断的裸键（数字开头、`true/false/null`、
  含 `/` `#`）一律加引号，保证 YAML 解析回来与 JSON 深等值。
- **参数刻画**：`route.Param.Enum` 声明取值白名单（三个 `{action}` 端点：容器动作 / Compose 动作 / 文件动作），
  **只进文档、不参与运行时校验**——动词白名单仍由各 handler 把关；`{ref...}` 的取值说明含「可带 `/` 与 `:`」。
- **一致性红测（S8.2）**：登记表 ↔ spec 的路径键双向比对（两侧先过 `openapi.SpecPath`）、每条端点的
  summary / 分组 / 说明与每条参数、响应的说明必须非空、spec 里每个 tag 必须有说明，
  `docs/openapi.json` 与 `docs/openapi.yaml` 必须与现生成逐字节一致（手改产物即红）；
  另有「不得绕过登记表直接注册 mux」的源码审查。
- **路径模板口径**：Go mux 的尾通配符 `{ref...}` 在文档里写作 `{ref}`（`openapi.SpecPath`），
  模板变量名与声明的 path 参数名必须一致——路由侧仍是 `{ref...}`（要容纳含 `/` 的引用）。
- 版本：`openapi: 3.0.3`（App 侧工具链兼容性最稳）。
- 鉴权声明：`securitySchemes` 提供 `cookieAuth` 与 `bearerAuth` 两种。
- **使用指南 `docs/API.md`（S8.3）**：面向 App / 脚本 / 自动化的「怎么用」——两套身份与鉴权矩阵、信封与错误码全表、`X-DD-Request-Id` 语义（审计关联，非执行级去重）、Token 创建/撤销全流程、六个 scope 的端点归属、跨端点约定与踩坑清单、curl 与 TypeScript 客户端示例。**分工**：契约本体是 spec（端点 / 参数 / 响应码以 spec 为准），API.md 与 spec 冲突时以 spec 为准并视为 bug。文档与 spec / 登记表的一致性由 `<工作区>/tools/verify-api-doc.py`（**不进 git**，工作区工具）机械核对：登记表↔spec 逐条、六个 scope 端点归属、47 条示例的路径/方法/查询参数。
- **防误删确认参数**：`DELETE /servers/{id}/containers/{cid}` 运行时 `guard.Confirm` 强制回传目标容器 id 或名字，与 URL 的 `cid` 完全一致才放行（query 与 body 取并集，空值 400 `bad_request`；兼作防误删护栏并与保护名单构成双闸门）。参数已在路由登记表与 `docs/openapi.{json,yaml}` 登记（`cmd/hub/main.go:399`，`confirm` Required）。
- UI 与 App 共用端点集，不为 App 另开一套路径。

## HTTP 契约

Hub API 前缀 `/api/v1`（`/api/v1/servers/{id}/…` 转发到对应 Agent，`/api/v1/{auth,api-tokens,settings,admin,audit_logs,scopes,health,openapi.json}` 由 Hub 直处），
响应统一 `{ "data": ..., "error": { "code", "message" } }`。下表以实装路由登记表（`cmd/hub/main.go` → `route.Table`）为准；与 `docs/openapi.{json,yaml}` 同源。

> 说明：`p` = 实装方法/路径；`s会话` = 仅 cookie 会话（Token 调一律 401）；`b Token` = 受 scope 约束（见 §6）；`—公开` = 无需凭据。
> 纳管（`/servers`、`/servers/invite`）和自注册（`/servers/enroll`）接入真实路由；纳管**无 `PATCH/DELETE /servers*`、无 `/servers/{id}/test`**。

### 公开端点（Token 与 session 都跳过）

| p | 说明 |
| --- | --- |
| `GET /health` | 健康检查（ladder：db→disk→enrolled；503 表降级） |
| `GET /scopes` | API Token 权限项字典（scope→说明） |
| `GET /auth/me` | 当前身份（未登录→401；未建号→setup_required） |
| `POST /auth/login` | 登录（账号锁 5 次/15min → 429；IP 限流 10 桶 / 30s 补 1） |
| `POST /auth/setup` | 首启建号（仅当无账号；IP 限流同 login） |
| `GET /api/openapi.json` | spec 本体（同源生成；版本字段由构建时注入，仓库默认 `dev`） |

### 仅会话端点（Token 一律 `401 unauthenticated`）

| p | 说明 |
| --- | --- |
| `POST /auth/logout` | 登出 |
| `POST /auth/password` | 改密 |
| `GET/POST/DELETE /api-tokens{+/{id}}` | Token 签发、列表、按 id 撤销（撤销后再调同 id → 404） |
| `POST /servers/invite` | 签发纳管邀请（TTL 默认 30min/最大 24h；返回 join_secret） |
| `GET/PUT /settings/protected-containers` | 保护名单：取 → `{patterns, using_default}`；写 → JSON 数组，非法正则 400 `bad_pattern` |
| `GET /settings/tls-fingerprint` | 自签证书 SHA-256 指纹（`{fingerprint, self_signed}`） |
| `GET /admin/backup` | 整库快照下载（DB 这时不 WAL checkpoint，GET 不落审计；PUT 类写入才包） |

### 鉴权端点（Token 受 scope 约束，会话用户全权限）

> scope 只约束 Token；会话用户（含调这些端点的浏览器）不受 scope 限制。
> 读 `read:*`、写 `write:*`；**写不蕴含读**。每行末尾括号里是**缺 scope 时的错码**。

| p | scope | 说明 |
| --- | --- | --- |
| `GET /servers` | Token `read:metrics`（403） | 受管主机清单；`ServerSummary` 线上字段 PascalCase（无 JSON tag） |
| `POST /servers/enroll` | Token `read:metrics`（403） | Agent 自注册纳管（201）；与 login/setup 同受 IP 限流 |
| `GET /servers/{id}/metrics` | `read:metrics` | 实时指标快照（单时刻，无 since/until/step） |
| `GET /servers/{id}/metrics/overview` | `read:metrics` | 历史曲线；窗口上限 8 天、`limit` 默认 720/上限 1440 |
| `GET /servers/{id}/containers` `…/containers/{cid}` | `read:containers` | 容器清单与详情（PascalCase 线上字段） |
| `GET …/containers/{cid}/logs` | `read:containers` | 日志流（**无信封、无读超时、不重试**；`since/until` 接受 Unix 秒或 RFC3339） |
| `POST …/containers/{cid}/{action}` | `write:containers` | start/stop/restart/pause/unpause/kill/resize（枚举白名单，文档只进不校） |
| `DELETE …/containers/{cid}` | `write:containers` | 需 `confirm=<id 或名字>` 精确一致 + 非保护名单；query 与 body 取并集 |
| `GET …/images` `POST …/images/pull` `DELETE …/images/{ref}` `POST …/images/prune` | 读 `read:containers`/写 `write:containers` | 镜像清单、拉取、删除、`prune` 清悬空（**无 dry-run**） |
| `GET …/compose` `GET/PUT …/compose/{project}/file` `POST …/compose/{project}/diff` | `read/write:containers` | Compose 项目清单、YAML 读写与 diff 预览 |
| `POST …/compose/{project}/{action}` `GET …/compose/{project}/logs` | 写/读 `containers` | Compose 写动作（diff/up 等；枚举白名单）与项目日志 |
| `GET …/compose/{project}/backups` `…/backups/content` `POST …/backups/restore` | 读/写 `containers` | 清单与读正文只读；**恢复是覆盖写故挂 write**；只回文件名/大小/时间，不含绝对路径，不提供删除 |
| `GET …/files` `…/files/stat` `…/files/content` | `read:files` | 列表、元信息、正文下载（**scope 是 `read:files`，不是 `read:metrics`**） |
| `PUT …/files/content` `POST …/files/{action}` | `write:files` | 写/删/建目录/改名；限根白名单；写前自动 `*.bak.<RFC3339>` 备份 |
| `GET /servers/{id}/volumes` `…/networks` | `read:containers` | 卷与网络只读清单；**无写端点**；读落审计（`volume.list`/`network.list`） |
| `GET /audit_logs` | `read:audit`；会话全权限 | 审计查询（游标 `before_id`，不含导出端点） |

### 跨端点口径（易踩点，详见 `docs/API.md` §7）
- **PascalCase 线上字段**：容器/镜像/servers 的 DTO 无 JSON tag（如 `ServerSummary.ID/Name/…`），这是有意选择，不是漂移。
- **写不蕴含读**：发 `write:files` 的 Token 调 `read:files` 仍 403；需要读就同时勾 `read:*`。
- **`X-DD-Request-Id`**：审计关联键，**不是执行级去重**（Agent 无状态、写操作不重试）。
- **`docs/API.md` 与本文冲突时以 spec 为准**——那本身就是 bug，应反馈。

Agent API 前缀 `/api/v1`：`/info` `/metrics` `/containers*` `/images*` `/compose*` `/logs` `/files*`，
语义与 Hub 一一对应；Hub 只做鉴权、审计、聚合，不重新实现 Docker 逻辑。Agent API 前缀 `/api/v1`：`/info` `/metrics` `/containers*` `/images*` `/compose*` `/logs` `/files*`，
语义与 Hub 一一对应；Hub 只做鉴权、审计、聚合，不重新实现 Docker 逻辑。

### Engine 契约实测基线（2026-10-14，node1 生产机 + ARM 宿主机，Docker Engine 29.1.3 / API 1.52）

> node1 连接信息（IP 等）属生产资产，禁止抄进入库文件（硬规矩 6）。本节只保留代号与引擎版本，不写可定位信息。

以下行为在 node1 生产机与 ARM 宿主机（均为 Docker 29.1.3 / API 1.52）上**只读实测**确认，
Hub/Agent 的口径与之对齐；升级引擎大版本时需复测：

- `/_ping` 返回 200 并带 `Api-Version` 头；`/version` 报 `ApiVersion 1.52`、`MinAPIVersion 1.44`。
- `containers/json` 默认只含 running；**空清单是 `[]` 不是 `null`**（Hub 侧同口径透传）。
- inspect 透传 Engine 原生 JSON；容器不存在时 404 `{"message":"No such container: …"}`；
  12 位短 ID 前缀可用（cid 校验按此放行）。
- logs **必须显式选流**：不带 `stdout`/`stderr` 时 Engine 29.1.3 直接 400
  （"you must choose at least one stream"）——dockercli 固定带 `stdout=1&stderr=1` 合流。
- logs 的 `tail`：非法值（`abc`/`-1`）Engine 不报错、按全量返回（node1 上即 23MB 级），
  Agent 侧 ValidTail 提前 400——比 Engine 严是刻意的，防怪字符串打满带宽。
- logs 的 `since`/`until`：**只认 Unix 秒**。RFC3339（含正确编码的 `+08:00`）会 500
  （`strconv.ParseInt` 失败，连 400 都不是）；`0` 与缺省等价（不过滤）；
  未来时间戳返回空流而非报错。**归一化已落地**（S3.6）：Agent 侧 logsQuery 经
  `protocol.NormalizeTimeParam` 把 RFC3339 转 Unix 秒再打 Engine，非法值 400；
  Hub 对外保持 RFC3339 友好（OpenAPI 按 date-time 宣传，Unix 秒兼容），
  agentcli 用同一函数做出门前预检。Hub→Agent 的 URL 编码由 url.Values 保证，
  Agent→Engine 归一后是纯数字，dockercli 仍保留 QueryEscape 防御。
- logs `timestamps=1`：每行前缀 RFC3339 UTC 时间戳；日志正文可能含 ANSI 转色码
  （容器自身 logger 产出），前端按纯文本渲染需容忍或清洗。
- `export=1` 的原始字节流与 `Content-Disposition` 由 Agent 拼装，docker CLI 可直接消费。

#### 镜像 API 契约（S4.1，2026-10-14 node1 写实测，Docker Engine 29.1.3 / API 1.52）

- `/images/json`：dangling 过滤以 `filters={"dangling":["true"]}` 形态下发（Hub→Agent 一跳翻译，构造点固定、无注入面），空清单为 `[]` 非 `null`（与 containers 同口径）；`ImageSummary` 用 Engine 原生字段名 `RepoTags`/`Size`，不在本层重命名。
- 镜像引用校验（`ValidImageRef`）：字符集 `[a-zA-Z0-9_.:/@-]`、首字符 alnum、≤255 字节、拒 `..`/`//`/残缺结尾；含 `@` 强制 `sha256:64hex`。node1 实测确认 `sha256:64hex` 形态的 digest 引用进 URL path 被 Engine 接受（delete 返回 404 而非 400，证明 path 合法）。
- pull（`POST /images/create?fromImage=&tag=`）：进度为 NDJSON 流，首条事件 `{"status":"Pulling from library/alpine","id":"3.20"}` 的 **id 是 tag 名（含点，非层）**，层事件 id 为纯 hex（真实 Engine 为 12 位、≥8 位判定通过）；按 hex 形态过滤区分层与首条 tag 事件。末条 `{"status":"Status: Downloaded newer image for <ref>"}`。
- **pull 失败的常见形态**：tag 不存在时 Engine **直接 404** + `{"message":"failed to resolve reference …: not found"}`（**不是** 200 + 流内 `error` 事件）。dockercli 在 HTTP 层拦截非 2xx，错误体读 `message` 字段（Docker 标准）；流内 `error` 字段检测仅作罕见场景（HTTP 200 但流内失败）防御。
- delete（`DELETE /images/{ref}`，无 force）：未被容器占用时 HTTP 200；不存在时 404 `{"message":"No such image: …"}`（dockercli 映射 `CodeNotFound`，文案与容器侧逐字对齐）。
- prune（`POST /images/prune`，**无 dry-run 参数**）：响应 `{"ImagesDeleted": [...], "SpaceReclaimed": N}`，其中 `ImagesDeleted` 为对象数组 `[{"Deleted":...},{"Untagged":...}]`，**无删除时为 `null` 而非 `[]`**。因 Engine 无 dry-run，DockDeck 用两段式：先 `GET /images?dangling=true` 展示将被清理清单，UI 确认后再 `POST /images/prune` 真删（对应红线第 2 条「prune 必先展示清单」的工程落地）。
- 超时分级：pull / prune 双跳各 10 分钟（`dockercli.PullTimeout` 与 `agentcli.ImagePullTimeout` 同量级）；其余镜像写操作走 5s 写超时；`agentcli.ImagePull` 直调 `once` 绕过默认 5s 写超时（长操作单独计时）。

#### Compose API 契约（S4.2 + S4.3 + S4.7，2026-10-14 node1 写实测，Docker Compose v5.1.4 / v2 系）

Compose **没有 daemon API**：与容器/镜像走 Engine socket HTTP 不同，Compose 全部走宿主侧 `docker compose` v2 CLI（`exec.Command("docker","compose",...)`）。dockercli 用 `composeRunner` 抽象执行——`Captured` 抓 stdout+stderr 合流（ps/up/down 等要拿输出）、`Streamed` 返回 stdout 管道（logs --follow）；默认 `execComposeRunner`，测试注入假实现。

- **白名单双关卡**（路径穿越唯一关卡）：
  1. `discover` 只在 `DD_AGENT_COMPOSE_DIRS`（逗号分隔，与 `--compose-dirs` / `DiskMounts` 同款）配置目录内递归找 `docker-compose.y*ml` / `compose.y*ml`（大小写敏感），忽略无关 yml，单目录不可读不拖垮列举；
  2. `ResolveProject(project)` 把项目名映射到白名单内真实路径，白名单外名字（含 `../` 穿越）一律 404。
  Agent 绝不把任意字符串透传 shell：写动词经 `ValidComposeAction`（up/down/restart/pull）在 agentcli 与 Agent 双侧校验，非法动词 Agent 侧 400、未知项目 404；项目名只经白名单解析。
- **实测 v2 CLI 行为**（node1，Docker Compose v5.1.4，即 v2 插件系）：
  - `docker compose -f <file> ps --format json`：单服务时返回**单个 JSON 对象**（非数组），字段含 `State`（"running"/"exited"）、`Service`、`Image`、`Name`、`Status`、`Project`；**空输出（down 之后）= stopped**。dockercli 先按 `[]composeRow` 解、失败回退逐行，两种形态都吃。
  - `up -d` / `down`(`-v`) / `restart` / `pull`：输出是纯文本（非 JSON），合并 stdout+stderr、`--no-color` 后透传为 `ComposeActionResult.Output` 给前端展示；成败写在 Output 里，不另造文案。
  - `logs --no-color [service] [--follow]`：纯文本流，复用容器日志通道（裸流、不经信封、不复用读超时）；无输出服务返回空流（合法，非错误）。
  - **compose 无 prune 子命令**：`down` 移除容器/网络（`-v` 也清卷），镜像不删；镜像清理是独立的 `docker image prune`（全局，不在 Compose 暴露范围）。故 DockDeck Compose 面故意不含 prune，与红线第 2 条「prune 必先展示清单」一致。
- **API 形态（S4.2 + S4.3 实际落地；S4.3 补回了第 104 行草图里的文件端点。下列为 Agent 侧路径）**：
  - `GET /api/v1/compose`：白名单扫描列项目，信封内层 `[]protocol.ComposeProject`，空清单 `[]` 非 `null`；
  - `POST /api/v1/compose/{project}/{action}`：action ∈ {up,down,restart,pull}（非法动词本地拦不出门），返回 `ComposeActionResult.Output`；未知项目 404；
  - `GET /api/v1/compose/{project}/logs?follow=`：裸 `text/plain` 流，复用容器日志的 `flushWriter` 逐帧下发；
  - `GET /api/v1/compose/{project}/file`（S4.3）：返回 `ComposeFile{config_file, content}`；
  - `POST /api/v1/compose/{project}/diff`（S4.3）：请求体 `{"content":...}`，返回 `ComposeFileDiff{diff}`——**只算不写**，不落盘、不产生备份；
  - `PUT /api/v1/compose/{project}/file`（S4.3）：请求体 `{"content":...}`，返回 `ComposeFileWritten{backup_file}`。
- **Hub 侧转发（S4.7）**：Hub 在 `/api/v1/servers/{id}/compose...` 上镜像上面六条 Agent 路径，
  只做鉴权、审计与转发（handler 不碰 SQL、不重新实现 Docker 逻辑，与镜像转发同款模板）：
  - scope：list / logs / file 读归 `read:containers`；action / diff / PUT 归 `write:containers`
    （diff 虽不落盘，但它是写入流程的一段、带请求体的 POST，与 PUT 同一把钥匙）；
  - logs 是 `NoEnvelope` 裸流——与容器日志同款：内容类型从 Agent 响应透传、
    不设 Hub 侧读超时（流寿命归调用方 ctx）、逐块 Flush；
  - **白名单判定留在 Agent 侧**：Hub 只转发项目名，不复制一份 `ComposeDirs`——
    复制就等于给「两侧口径漂移」开口子；
  - 审计动作：compose.list / compose.action / compose.logs / compose.file.read /
    compose.diff / compose.file.write（读也审，与容器、镜像同口径）。
- **写前备份与 diff 预览（S4.3，红线第 2 条「备份优先于变更」的工程落地）**：
  - `WriteFile` 先把当前内容备份为 `<file>.bak.<RFC3339>` 再覆盖写，**备份落盘成功才动原文件**——任一失败即中止，不给「原文件已改但无备份」留口子；只编辑已发现的项目，源文件缺失即报错且不新建。
  - diff 由 `internal/diff.Unified` 自实现（LCS + 上下文窗口分块，`@@` 头与 `diff -u` 同语义：纯新增旧区间 `-0,0`、纯删除新区间 `+0,0`），**零新增依赖**；相同文本（含尾部换行归一）返回空串。预览与写入分两个端点，前端可「先看 diff → 再确认写」。
  - 文件操作直接吃 `ResolveProject` 给出的白名单路径（穿越拦截仍是白名单这一关，不另行校验路径）；三个端点均**先解析项目再解 body**：未知项目一律 404（不读、不写、也不解 body），已知项目 body 非法（`httpx.DecodeJSON` 的 `DisallowUnknownFields`）400。
  - 路由：`file` / `diff` 是字面量段，比 `POST .../{project}/{action}` 的通配更具体，Go mux 按最具体优先匹配，不与 action 路由冲突（"diff" 本身也不是合法动作）。

#### 文件管理契约（S5.1–S5.4/S5.6 Agent 侧 + S5.5 Hub 侧转发/审计/scope，均已落地）

文件管理是这个项目里唯一的通用写能力，边界因此收得比别处紧：**未配 `--fs-root` 时 Agent 一条路由都不注册**，
功能在网络层就不存在；配了根目录而根不可用时**启动直接失败**，不静默降级成「模块关闭」
（用户以为开着、实际是关的，这种误解比启动失败危险）。

Agent 路由（Hub 侧同构路径 `/api/v1/servers/{id}/files*` 已随 S5.5 落地，审计动作 `file.list`/`file.stat`/`file.content`/`file.write`/`file.action`）：

| Agent 路由 | 语义 | scope（S5.5 启用） |
| --- | --- | --- |
| `GET /api/v1/files?root=&path=&depth=` | 目录列表 `FilesList{path,entries,truncated}`；空清单 `[]` 非 `null` | `read:files` |
| `GET /api/v1/files/stat?path=` | `FileMeta`（大小/类型/修改时间/是否超限） | `read:files` |
| `GET /api/v1/files/content?path=` | 流式正文；元信息走 `X-DD-File-*` 响应头，被截断时 `X-DD-Truncated: 1` | `read:files` |
| `PUT /api/v1/files/content?path=` | 流式上传（请求体即内容）；返回 `FileWritten{path,backup_file,size,created}` | `write:files` |
| `POST /api/v1/files/{action}` | `mkdir` / `delete`（仅空目录）/ `rename`（`dest` 同守白名单）；返回 `FileActionDone` | `write:files` |

- **路径一律相对白名单根**，对外不回宿主机绝对路径（`protocol.FileEntry` 的既有口径）；多根用 `root=<索引>` 寻址，缺省 0。
- **校验顺序固定，且全在任何 IO 之前**：词法检查（绝对路径 / `..` 段 / 控制字符 / 超长 / 过深）→ 白名单词法前缀 →
  `EvalSymlinks` realpath → 再前缀（挡软链逃逸）→ 黑名单（请求路径与真实路径各查一遍）。前缀比对带分隔符边界，
  防 `/data-evil` 冒充 `/data`；`..` 是**显式拒绝**而不是交给 `Clean` 静默吃掉（`Clean("/"+"../x")` 会把越界输入洗成合法请求）。
- **动作名有两个来源**（路径段与请求体）**必须一致**，不一致 400——不猜调用方想干什么。
- **列表只列可操作项**：每一项都过一遍关卡，逃逸软链、命中黑名单、悬空软链不出现（看得见就点得动，点了不会 403）；
  软链按**真实落点**展示（`Name` 是链接名、`Path` 是解引用后的相对路径），同一目标经不同链接出现两次是预期的。
- **写前备份优先于变更**：覆盖写先落 `<file>.bak.<RFC3339>`，**备份落盘成功才 rename 覆盖**；同秒撞名追加 `.1`/`.2`；
  写入固定 `0644`（**不带可执行位**，红线「上传物不可被执行」）；新建文件无备份、`created=true`。
- **目标末段是软链一律拒**（写 / 删 / 改名）：否则 `delete inlink` 删掉的是真目录 `sub`，写 `cert.txt`（白名单内软链）
  会把别处的文件内容搬进白名单里——两者都不是用户按下去时以为会发生的事。读操作仍可穿软链（`Resolve` 已保证落点在白名单内）。
- **上限**：单文件 `--fs-max-file`（默认 16 MiB）、目录深度 `--fs-max-depth`（默认 8）、单次列表 `--fs-max-entries`（默认 2000）。
  读超限→截断并标记；写超限→413 且**原文件一个字节都没动**（先写同目录临时文件、上限检查通过后才动原文件）。
- **上传上限与鉴权签名体是同一个数**：Agent 验签必须先读完整个 body，故新增 `agenthttp.AuthN`，
  文件路由的签名体上限 = `max(1 MiB, --fs-max-file)`；两侧共享硬顶为 `protocol.MaxFileUploadBytes = 16 MiB`，
  `--fs-max-file` 超过它 Agent 启动即报错。**因此上传不是端到端流式**（Hub 与 Agent 都要把 body 读进内存验签，
  且 Hub 侧读 body 上限必须用同一常量，否则出现「Hub 放行、Agent 413」的错位）；落盘侧仍是流式 + 原子改名。
  要支持更大的文件，需改该常量并两侧同步发版——这是刻意的取舍：无上限等于给 Agent 装个内存放大器。
- **黑名单**：内置默认列表（`id_rsa`/`*.pem`/`*.key`/`.env`/`shadow`/`master.key`/`.ssh` 等）+ 配置 `--fs-deny`
  **只能追加，删不掉默认**；pattern 用 `path.Match`，逐段与整条相对路径各匹配一遍，宁严不宽；
  pattern 在构造期即校验合法性——留到请求期再「忽略 Match 错误」等于把保护静默关掉。

## 主机指标采集

Agent 侧只读 `/proc`，不引入 gopsutil 这类额外依赖（交叉编译体积与可控性）：

| 指标 | 来源 |
| --- | --- |
| CPU % | `/proc/stat` 两次采样差值（含 user/system/iowait/steal） |
| 内存 | `/proc/meminfo`：used = MemTotal − MemAvailable |
| 磁盘用量 | `statfs` 系统调用，逐挂载点，路径由配置指定 |
| 网络流量 | `/proc/net/dev` 累计 rx/tx 字节，速率由 Hub 侧按时间差算 |
| 负载 | `/proc/loadavg` |
| 进程数 | `/proc` 下数字目录计数 |
| 在线时长 | `/proc/uptime` |
| 每容器 cpu/mem | Docker `/containers/{id}/stats?stream=false` |

采样在 Agent 侧不落地历史（除最近一次快照），历史序列由 Hub 轮询写 `metrics_snapshots`，
滚动 7 天。这样 Agent 无状态、可随意重装。

## 文件管理（新增模块，边界最严）

文件写能力与「只读默认」原则冲突，因此单独收紧：

1. **根白名单**：Agent 启动时用 `--fs-root` 指定可访问目录，可多个；不在白名单内的路径一律拒绝，且校验发生在任何 IO 之前。
2. **realpath 后前缀校验**：解析符号链接得真实路径，再校验前缀属于白名单，杜绝 `..` 与软链逃逸。
3. **黑名单**：私钥、`credentials`、`.env`、`/etc/shadow` 类 pattern 不可读不可写。
4. **不做执行**：没有 exec、没有终端。上传的文件不会被运行。
5. **默认关闭**：未显式配 `--fs-root` 时文件模块不注册路由。
6. **写操作全审计**，且需 `write:files` scope。

## 现有容器与数据保护（不可协商）

1. **默认不写宿主机**：Hub 不主动写任何文件。写入路径只有两类，且都需显式请求——
   用户提交的 Compose YAML，以及文件管理模块在白名单目录内的操作。除此之外全部只读。
2. **备份优先于变更**：写 Compose 文件与白名单内文件前，先落 `*.bak.<RFC3339>`；SQLite 用 `VACUUM INTO` 出快照，不复制运行中的 db 文件。
3. **破坏性动作三重门**（S4.4 落地为 `internal/guard`，Hub 与 Agent 共享比较语义）：保护名单命中即拒
   （`settings.protected_containers` 逗号分隔正则，缺省仅 `^dockdeck-`；watchtower/komari 由运维自行删除、
   不入名单（2026-10-15 用户定），部署时录入的现有容器名追加进 settings）；`prune` 类必须先 **在 UI 展示将被删清单**再执行——Engine 的 `/images/prune` 无 dry-run 参数，DockDeck 以「先 `GET /images?dangling=true` 取清单、UI 确认后 `POST /images/prune` 真删」两段式等价实现；
   删除容器要求输入名字确认（`confirm` 与目标 ID/名字完全一致，Hub 校验后透传，Agent 同守一道——
   直连 Agent 的调用方也绕不过）。所有此类操作进审计。
4. **不碰自身**：Hub/Agent 容器自身、其 volume、其网络在 API 层标记 `protected`，UI 灰置。
5. **不猜语义**：Agent 只转发 Docker API，不批量重启、不自动更新、不改 restart policy、不动宿主机 systemd 单元。
6. **失败即停**：Agent 对任何 5xx 不重试写操作；写操作全部幂等键 `X-DD-Request-Id` 落审计。

### 幂等键 X-DD-Request-Id（写操作纪律的组成）

- **形态**：请求头 `X-DD-Request-Id`，1-64 字符，限 `[A-Za-z0-9._-]`。非法值 Hub 直接 400——这个值要进审计库、进响应头、转发给 Agent，放行 CR/LF 等于给响应拆分开门。旧头 `X-Request-Id` 仅入站兼容。
- **缺省生成**：客户端不带时 Hub 现场生成（8 字符随机），响应头 `X-DD-Request-Id` 原样回显，客户端重试时带上它即可对账。
- **落审计**：写操作的审计行记录该键（`audit.request_id`），与响应头回显值一致。
- **转发 Agent**：Hub → Agent 的写请求原样转发该头；Agent 的引擎错误日志带它落行，Hub 审计与 Agent 日志可对同一次操作。
- **边界（明确不做的）**：不做执行级去重——Agent 无状态、可随意重装，记忆键等于制造漂移源；写操作不重试（见上第 6 条），重复提交由调用方负责。该键的保证是**审计关联与排查对账**，不是「同一键只执行一次」。

### 审计覆盖面（S3.4 起读+写全量，S3.6 补齐）

- **已纳入**：容器 5 端点（list/inspect/logs/action/remove，S3.4）+ 指标 2 端点（`metrics.latest`/`metrics.overview`，读也审——「谁看过哪台机器」同样是安全审计对象，S3.6 补）+ Token 2 写端点（`token.create`/`token.revoke`，S3.6 补）+ 文件 5 端点（`file.list`/`file.stat`/`file.content`/`file.write`/`file.action`，读+写全量，S5.5）。auth 的 login/logout/setup/password 由 handler 内自审计（含失败路径），不套中间件。
- **机制**：`middleware.Audit` 包在 handler 外、鉴权链内——匿名 401 与 scope 403 属鉴权层事件不留行（否则探针机器人刷满审计表）；成功与失败（≥400）都落行，`server_id` 从 `{id}` 路径参数自动带（只认 `/api/v1/servers/` 前缀，Token 端点的 `{id}` 是 Token ID 不是服务器，记 0，ID 在 `target` 列不丢）。
- **审计表无外键**（迁移 0004）：`server_id`/`user_id` 不引用活表——「打向不存在服务器的探测」必须留痕（旧 schema 下这种行被外键静默吞掉，恰是最该留的行）；审计行记录「请求打向哪、谁打的」，实体可以从未存在或已被删。对照：`metrics_latest`/`container_snapshots` 是活数据，外键保留。
- **流式端点**（logs follow）的审计行落在流结束时——连接多久，记录多久后落库。

## 前端

Vue 3 + Vite + TS，`vue-router` + `pinia`，图表 uPlot，无重型 UI 框架，自带深色设计系统。
构建产物 `web/dist` 经 `go:embed` 进二进制，交付物仍是单个可执行文件。
页面：**服务器监控总览（主视图）** / 单主机详情（指标·容器·日志·镜像·Compose·文件）/
审计 / API Token 与设置。状态与数据须登录才显示，无免登录公开页。
危险操作二次确认，输入目标名才可提交。

### 前端工程约定（S6.1 起）

| 项 | 约定 |
| --- | --- |
| 目录 | `web/`（`.gitignore` 早已预置 `web/node_modules/`、`web/dist/`、`web/.vite/`、`web/coverage/`） |
| 包管理器 | **npm**。理由：本文与 S6.5 措辞即为 `npm run build`；不引 pnpm/yarn 以免多一套锁文件与工具链 |
| Node | **`>=22.12.0`**。Vite 8 系 engines 为 `^20.19.0 \|\| >=22.12.0`；Node 20.20.2 虽恰好擦线可用，但余量太小，故 sanbox 已升 22.23.3 LTS |
| 脚本 | `npm run dev`（开发期 dev server）/ `npm run build`（产出 `web/dist`）/ `npm run preview` / `npm run typecheck`（`vue-tsc --noEmit`） |
| 类型检查 | **TypeScript 钉 `^5.9.3`，勿升 7.x**：TS 7.0 移除了 `./lib/tsc` 子路径导出，而 `vue-tsc` 至 3.3.13（当前 latest）仍以此路径挂载 tsc，升级后 `npm run typecheck` 直接以 `ERR_PACKAGE_PATH_NOT_EXPORTED` 失败——已实测，非推测。待 vue-tsc 发支持 TS 7 的版本再动 |
| 唯一色源 | `web/src/styles/tokens.css` 定义全部深色令牌；**组件样式只准引用变量，不得写字面色值**；语义色按用途命名（`--danger` 而非 `--red`）；`--chart-1..6` 预留 S7.1 的 uPlot，S6 阶段不引用 |
| 自检页 | `/dev/tokens` 把全套令牌可视化（类别：背景层级/前景/边框/强调/语义实色/语义浅底/图表色板/圆角/间距/字号） |
| 开发期联调 | dev server 把 `/api` 反代到本机 Hub（`secure:false` 接受自签 TLS）；**该开关只存在于开发配置**，生产由 Hub 同源提供 SPA 与 `/api`，不经代理 |
| 产物入库 | `web/dist` **不入库**（构建产物），`package-lock.json` **必须入库**（锁版本，防依赖漂移）。fresh clone 无 `web/dist` 目录，见 `docs/ARCHITECTURE.md`「构建与交付」——S6.5 接线前须先处置 |
| Hub 侧接入 | `cmd/hub/main.go` 的 `/` catch-all 已是「非 `/api/` 前缀交静态资源、未命中回落 `index.html`」的 SPA 兜底，前端接入时 Go 侧**零改动** |
| 请求层 | **零依赖 `fetch`**（`src/api/client.ts`），不引 axios：需求只到「同源 JSON + 会话 cookie」。**不得带 Bearer 头、不得改 credentials**——ARCHITECTURE 既定：会话不识别 Bearer，且 API Token 不能访问 UI 专属端点；同源由部署保证（生产 Hub 同源，开发期 vite 反代） |
| 错误语义 | 失败一律 `{"error":{code,message}}`，前端归一为 `ApiError{status,code,message}`；**按 code（`protocol.Code*`）而非 HTTP 状态码判定处置**——`unauthorized`(401) 才引导重新登录，`forbidden`(403) 是权限问题不该踢人，`rate_limited`(429) 只需重试 |
| 鉴权闭环 | `GET /auth/me` 是唯一事实来源（未登录也 200，靠 `setup_required` 区分该建号还是该登录）；会话由 Hub cookie 持有，前端**不落 localStorage**。守卫顺序：先探测 → 建号优先 → 登录兜底；**探测失败只报错不重定向**；`/login`、`/setup` 必须公开，否则自重定向成环 |
| 布局 | 主界面由 `AppShell` / `AppSidebar` / `AppTopbar` 三件套承载。**套壳与否取决于路由层级，不是 CSS 开关**：`/` 及其子路由的父组件是 AppShell，`/login`·`/setup` 是顶层裸路由（居中单卡）——改布局时改路由结构，不要在 `App.vue` 里堆 if。响应式**先定手机**：`<1024px` 侧栏为 fixed 抽屉 + 遮罩（点遮罩 / Esc / 路由切换即关），`≥1024px` 才常驻成网格列并可折叠成图标条。`sidebarOpen`（抽屉）与 `sidebarCollapsed`（折叠）是两个独立状态：前者随路由关闭，后者是长期偏好，**路由切换不得重置** |
| 导航与图标 | 侧栏菜单单一数据源 `src/config/nav.ts`：无 `to` 的项渲染为置灰不可点并带「待实现」，让布局先于功能页定型，S7 只需往里填页面。图标一律走 `AppIcon`（自制 24×24 描边 path，**不引图标库**），与「无重型 UI 框架」同一原则 |
| 表单控件 | `src/styles/forms.css` 提供 `.field` / `.field__input` / `.btn` / `.btn--primary` / `.card` 等基础块，**任何页面写表单都用它们**，不得各写各的输入框——S7 设置页（改密、保护名单、API Token）全是表单，不统一迟早长出几种不一样的输入框 |
| 认证错误语义 | 登录失败后端**统一回 `invalid_credentials`**（「用户不存在」与「密码错」刻意不区分，避免泄漏用户名是否存在），前端**不得**自作主张提示「用户名不存在」；`account_locked`(429)、`account_disabled`(403)、`already_initialized`(409) 的语义由后端 message 直接呈现。建号唯一硬规则是密码 ≥ 8 位（后端 `weak_password`），前端先拦一次省往返，后端仍复查 |
| 跳转安全 | 登录后 `redirect` 参数**只接受站内路径**：必须以单个 `/` 开头且不以 `//` 开头——后者是协议相对地址，会把用户送到外站（开放重定向） |
| 总览页的三条硬约束 | ①**在线以后端 `servers.last_seen` 为准**：轮询器成败都更新它（只在成功时写会让掉线机器永远显示在线）。②**网络速率不能从实时快照算**：后端 `NetStat` 是开机累计值，前端只有最新一次快照，故用历史曲线最后两点求差；点数不足时显示累计值并标注，不许编造速率。③**uPlot 是 canvas，不解析 CSS 变量**：配色在运行时从 `:root` 读 `--chart-*`（值仍出自 tokens.css）。刷新周期 30s 与采集对齐，页面卸载必须停表 |
| servers 列表的字段口径 | 后端 `ServerSummary` 无 json tag，线上字段是 Go 导出名（**`ID`/`Name`/`BaseURL`/`Enabled`/`LastSeen`/`LastError`**）。前端在 `api/servers.ts` 用 `toServer` 单点映射成 camelCase——后端若补 json tag，改这一处即可 |
| 详情页的口径（S7.2） | ①**一图一指标，不塞多序列双 Y 轴**：四项指标量纲不同（百分比 vs 字节），双轴图像两个指标相关实则误导；网络上下行同量纲是唯一画两条线的例外，且画的是**速率**（相邻点差分，首点无前值留空断线，不画成 0）。②**进程只有总数**：后端无进程列表端点，详情页显示 `HostMetrics.processes` 总数并注明「明细待后端立项」，不造进程表。③**`loading` 与 `refreshing` 分开**：首载/切区间走 `loading`（区间按钮禁用防并发），后台轮询走 `refreshing`（只显示「刷新中…」，不动按钮）；轮询与首载共用同一条 `pull`（实时快照 + 当前区间曲线），口径不劈叉；轮询失败保留上次成功数据、只把原因记进 error——监控页被一次网络抖动清成空白比显示旧数据更糟。区间档 1h/6h/24h/7d 对应后端窗口上限 8 天，`limit` 取 720 |
| 指标采集链路 | Agent 暴露 `/api/v1/metrics`，由 **Hub 侧 `internal/metricsync` 定时拉取**（默认 30s，可配）写入 `metrics_latest`（实时快照）+ `metrics_snapshots`（历史点）。单台超时 10s、一台失败不影响其余；容器指标拿不到降级为 `null`（前端据此区分「无容器」与「空列表」）；历史保留 14 天、每小时清过期。**没有这个轮询器，所有指标端点恒 404**——它是 S7 图表的唯一数据源 |
| 容器页的口径（S7.3） | ①**两个数据源分开降级**：身份/状态来自 Agent 实时转发（`GET /servers/{id}/containers`，失败即错误）；CPU/内存占用来自 `metrics_latest` 的 `ContainerStat`（拿不到只显示「—」不算失败；`MemLimit=0` 表示未设上限，此时百分比无意义、不展示）。占用按容器 ID 合并，stats 的 ID 与清单不一致时退到按名字匹配。②**删除的 cid 用容器名**：后端 `guard.Confirm` 要求 confirm 与 URL 里的 cid 完全一致，让用户输名字而不是 64 位 ID（Engine 接受名字作容器引用）。③**确认范围**：remove 输名确认 + force/v 勾选；stop/kill 简单确认（kill 带信号下拉，白名单由后端校验）；start/restart/pause/unpause 直接执行、toast 回报。④**日志端点是全表唯一 NoEnvelope**：裸流 `text/plain`，成功无 `{data}` 信封、失败又是 `{error}` 信封，不能套 `request()`，单独直 fetch；follow 用 ReadableStream 边收边吐、AbortController 停止（流寿命归浏览器 ctx）；文本 2 MB 上限从头部截断。⑤**确认弹层的 Transition 拆两条**（遮罩淡入/面板滑入）——slide 的 transform 若包住 `position:fixed` 的面板，变换中的祖先会成为固定定位的包含块，面板错位 |
| 镜像页的口径（S7.4） | ①**pull 的成败看流内 Err，不看状态码**——Engine 把拉取错误放在进度流里，HTTP 仍是 200，故 api 层把 `Err` 非空统一成 throw，上层与网络失败走同一条 catch。②**delete 的 ref 原样拼路径、不做 encodeURIComponent**——路由是尾通配符 `{ref...}`，编码后的 `%2F` 过不了匹配；字符集由后端 `ValidImageRef` 权威校验（Hub 拼 Agent 路径前、Agent 拼 Engine 路径前各一道），前端 `looksLikeImageRef` 只做「快速失败」不做权威判定（语义对错交给 Engine，404/400 原样透传）。③**prune 两段式在前端闭环**——后端 prune 无 dry-run 参数（S4.1 实测），「展示」与「确认」都在 UI：先 `GET images?dangling=true` 把将删清单拉进确认框（N 个、合计大小），用户确认后才 `POST prune`；结果按 Deleted（本体删掉）/Untagged（只解引用）分开计数——「删了 3 个镜像」与「解了 2 个 tag」不是一回事。④**RepoTags 为空即 dangling**（nil 与 [] 同判，UI 不区分）；显示口径与 `docker images` 习惯对齐：默认不含中间层（-a 才含），「只看悬空」是 prune 的预演。⑤**与容器页同构但不提前抽象**：全局页 + 主机切换器 + 30s 轮询 + loading/refreshing 分离，store 骨架复制 containers store 改字段（等第三个同类页再提炼共用 store）；`.link` 文字按钮样式在两页各存一份（scoped 隔离，提升为 forms.css 公共类留待 S7.6 文件页一起做） |
| Compose 页的口径（S7.5） | ①**两段式写入**——diff 预览强制先行（`POST diff` 不落盘不备份）→ AppConfirm 带 `+added/-removed` 统计（解析跳过 `+++`/`---` 文件头行）与 `.bak.<RFC3339>` 备份说明 → `PUT`（Agent 侧备份落盘成功才覆盖原文件）→ toast 展示 backup_file 路径；写后编辑态保留可续编，`reload()` 刷新清单。②**动作确认映射**：up/restart/pull 直行 + toast 首行输出（完整 compose 输出进 `<details>` 折叠区）；down（停全部容器）简单确认。③**编辑态全部页面本地 ref、不进 store**——临时 UI 态跨路由存活会留下半个编辑会话，store 只持 servers/projects 清单数据；编辑中禁用主机切换器防误切丢态。④**零依赖编辑器**——纯 textarea + `--font-mono`（Tab 插两空格），diff 用 `<pre>` 按代码 diff 通行约定着色（增行绿删行红，与金融涨红跌绿口径相反、勿混）。⑤**项目名走 `encodeURIComponent`、镜像 ref 尾通配符不编码**——compose 路由是普通路径段（Go 1.22 ServeMux 解码 %XX），与 S7.4 镜像 ref 的 `{ref...}` 形态相反，两处差异由路由形态决定、代码注释互相注明。⑥**`ResolveProject` 按名取首个匹配**（后端现状）——白名单内跨目录重名时后者不可达，前端不自行造唯一键。**备份列表无端点**（备份只是宿主机文件 `<file>.bak.<RFC3339>`），已拆 S7.11 待后端立项 |
| 文件管理页的口径（S7.6） | ①**单层列表 + 面包屑**——后端 `List(depth)` 返回**扁平列表而非树**，前端每层 `depth=1` 自己维护路径栈，面包屑可回跳任意层（不做懒加载整树）。②**根槽位静态探测**——后端**无「列根」端点**（根的数量与宿主机路径敏感、不外泄，root 只能按索引寻址），前端静态槽位 0..3 并行试列：200 亮、400 灰；**ok 槽的目录内容直接充当首屏**，不额外发请求；全 bad 时按主机在不在线区分 error（在线=未配 `--fs-root`，离线=网络问题），不静默显示空目录。③**危险操作确认**：delete **输名确认**（确认框要用户输入目标文件名——后端 delete **无备份不可恢复**，与 Compose 的覆盖写有 `.bak.<RFC3339>` 备份截然不同，确认文案须点明）；rename/mkdir 输入即执行；覆盖写在确认框说明备份行为。④**同页内嵌编辑面板**：预览 → 编辑 → `PUT`，**无 diff 段**（files 无 diff 端点，与 Compose 页两段式写入是不同形态，勿照抄）；二进制文件（content-type 嗅探非文本）只读展示 + 下载；**1 MiB 前端阈值就近拦断**（api 层另有 `protocol.MaxFileUploadBytes`=16 MiB 硬顶）。⑤**两处与前三页刻意分叉**：**无轮询**——文件清单非实时数据（容器状态会变、文件不会自己变），只有手动 refresh、无 timer；**目录位置进 store**（`currentRoot`/`currentPath`，面包屑/文件表/工具栏共享且切走回来停原目录是合理预期），编辑态仍是页面本地 ref（同 Compose 页纪律）。⑥**规则权威全在 Agent 侧**：`--fs-root` 多根白名单、`--fs-deny` 只能追加的内置黑名单（命中文件不出现在列表）、写前备份、软链末段拒写/删/改名——前端一律不复制这些规则，只做快速失败；**上传非端到端流式**（16 MiB，Hub/Agent 都读内存验签），故上传与编辑共用同一套前端阈值思路。⑦**截断两处**：`maxEntries`（默认 2000）截断清单、`--fs-max-file` 截断正文（`X-DD-Truncated: 1` 原样透传 UI 标注）。⑧**`.link` 提升为 forms.css 公共类**（S7.4 镜像页注释留话的落地）：Containers/Images/Compose 三页各删一份逐字重复的 scoped 定义，公共集取三页并集（`--warn` 仅 Containers 有、`--danger` 三页都有）；`format.ts` 补 `formatRFC3339`（`mod_time` 是 RFC3339 字符串，与 `formatWhen` 的 Unix 秒是两套口径，不混用）。**stat 端点刻意不接**（列表项已带 mode/size/mod_time 元信息），克制待需要时再接 |
| 审计页的口径（S7.7） | ①**全局页、无主机切换器**——审计是 Hub 单点写入的全站数据（middleware.Audit 在 Hub 侧留痕，行上带 server_id 但语义是「哪台机上的操作」而非「按机查询的数据」），与 files/containers 的按机维度相反；server_id 只是过滤维度之一。②**倒序 + `before_id` 游标分页**——后端 `ORDER BY id DESC`，`next_before=0` 表示没有更多；append-only 表 id 单调，游标不因 from/to/server_id 等过滤维度漂移（与 offset 分页的本质区别）；limit 显式发 100 不吃后端缺省（改需前后端一起发版）。③**过滤 draft/applied 两态**——输入进 draft，点「查询」才 apply（action 是前缀文本输入，输入即查询会每字符发一次请求）；时间用 `datetime-local` 本地时区输入，api 层 `isoSeconds` 归一秒级 RFC3339 再发出（表 ts 是秒级 Z 后缀 TEXT、按字典序比较，带毫秒/偏移的串混比会错位——`'.' < 'Z'`；后端 handler 收到后再归一一遍，双保险）。④**无轮询 + 查询不写审计**——审计是「已发生事件」的记录不是实时状态，只手动刷新；后端查询端点刻意不包 middleware.Audit（高频读留痕会淹掉真操作记录），故刷新/翻页/导出都不留痕、前端无需节流。⑤**CSV 导出仅当前已加载行**——游标分页下没翻到的页不在文件里，导出按钮旁与翻页区两处明示「要全量先翻到底」；序列化在 `utils/csv.ts` 纯函数：RFC 4180 转义（含逗号/引号/换行才包引号、内部引号翻倍——target/detail/error 是自由文本，不转义会产出列数随数据漂移的废文件）+ UTF-8 BOM（Excel 认 BOM 才按 UTF-8 打开中文）+ CRLF 行尾。⑥**`server_id=0` 显示「—」、CSV 导原始 0**——UI 层的空值替换不进数据层（CSV 是给工具消费的）；**行展开是页面本地 ref**（「当前展开看哪一行」是易失 UI 态，同 S7.5/S7.6 纪律），过滤条件才进 store（切走回来停原过滤是合理预期） |
| 设置页的口径（S7.8） | ①**单页五分区、全局页无主机切换器**——设置是 Hub 单点配置、不按机维度（与审计页同构、与 files/containers 按机维度相反），五块按「身份 → 凭据 → 边界 → 通道 → 兜底」排：改密 / API Token / 保护名单 / TLS 指纹 / 整库备份。②**四枚设置端点一律 AuthSession**——泄漏的 API Token 不得改安全配置、不得拖库（同 Token 管理端点口径；`ServeSession` 不查 scope，会话用户恒可用，受限的只有 Token）；PUT 与备份包 `middleware.Audit`（改安全配置与拖库是审计表一等公民，动作 `settings.protected.update` / `admin.backup`），两枚 GET 不包（读无副作用，留痕淹掉真操作记录）。③**保护名单存储 JSON 数组化**——逗号分隔会把含逗号正则（量词 `{2,3}`、字符类 `[a-z,0-9]`）切碎，那是放空安全边界的数据事故；`ParseList` JSON 数组优先，S7.8 前存量（逗号分隔/单条）读取兼容、不迁移，`[` 开头但非合法 JSON 回落整体一条；PUT 逐条 `regexp.Compile` 预检，坏正则 400 带「第 N 条」且库无写入，空数组清除；`containerGuard` 逐请求读 settings（保存即生效无需重启）。④**TLS 指纹不展示过期时间**——自签模式回 hex 冒号分隔 SHA-256 供人工比对，其余模式空串 + `self_signed=false` 并明说无指纹可比对；指纹来自 `tlsx.EnsureSelfSigned` 第三返回值（此前只打启动日志）。⑤**备份走 `VACUUM INTO` 一致快照**——临时文件落 `<dataDir>/backups/` 用完即删，`sqlQuote` 单引号翻倍拼 SQL 字面量，NoEnvelope 裸流下载（`application/vnd.sqlite3` + Content-Disposition）；前端明示「快照等同数据库副本」（含 master.key）的密级。⑥**改密成功后强制登出**——后端撤销全部会话（含当前），前端据此登出跳登录页而非留在页内；Token 一次性明文只在签发结果里带回，关闭面板即无处可查 |
| 卷与网络页的口径（S7.9） | ①**单页两分区 + 全局页主机切换器**——卷与网络是按机维度的资源（与容器/镜像页同构，与审计/设置的无切换器相反）；一页两分区而非两个独立页：一次切换、一份 store、一个侧栏项，排查时通常连着看（「这个容器挂了什么卷、在哪个网络」）。②**只做只读两条、无写端点**——删除卷即删数据，prune 的 dangling 判定由 Engine 做出、误判不可逆，与「第一要务是不把现有容器和数据搞坏」冲突；清理走宿主机 docker CLI（有人工判断在场）。③**不做 inspect 端点**——`/volumes` 清单已含完整卷对象（含 UsageData）、`/networks` 清单已自带 Containers 映射，对本页展示零增量；按需再补（同 S7.6 stat 端点克制口径）。④**两个端点形态不同**：`/volumes` 是**对象包数组**、`/networks` 是**裸数组**，两处解码模板不通用；且网络自带接入容器名单（「谁接入」零额外请求）、卷只给 `UsageData.RefCount` 计数**不给名单**——差异源自 Engine 数据形态，UI 上注明以免被当成页面没做。⑤**卷的 Mountpoint 不进 DTO**（宿主机绝对路径，与文件模块「对外不回宿主机绝对路径」同口径；不解析就不存在、无从泄漏），有反射用例钉死该字段不许出现；**`UsageData` 用指针**——null 表示「Engine 没统计」，退化成值类型会被读成「0 个容器在用」，用户据此误判卷可删。⑥**前端两数据 `Promise.all` 一把拉**（任一失败整块报错重试——半屏空白「有卷无网」比整块重试更易被读成「这台机没有网络」）、**保留 30s 轮询**（与文件页「无轮询」分叉：文件不会自己变，容器启停会改网络接入名单）、展开看接入名单是页面本地 ref（易失 UI 态，同 S7.5/S7.6/S7.7 纪律）。**踩坑留档**：新页面的 CSS 令牌名必须逐个对照 `tokens.css`——本页初版用了 8 个不存在的名字（`--fg-strong`/`--bg-raised`/`--danger-soft`/`--border-danger`/`--font-xs\|sm\|base\|lg`），真实命名是 `--fg-default`/`--bg-elev-2`/`--danger-subtle`/`--danger-border`/`--text-xs\|sm\|base\|xl`；**构建与类型检查都不报、只在浏览器里静默回退** |
| Compose 备份的口径（S7.11） | ①**做在 Compose 页编辑面板内，不新开页**——备份是项目的附属，与「选中项目 → 编辑面板」同构；懒加载，展开才拉清单（多数编辑会话根本不看备份）。②**预览复用现有 diff 端点**——拿备份正文去算「与当前文件的差」，与写入的两段式完全同款，**不新开预览端点**（Q1-A）。③**恢复前必须再备份当前内容**——恢复本身就是一次覆盖写，走 WriteFile 即自动带上这一步；不做的话回滚是**单向门**：恢复到旧版本后就回不去了（Q2-A）。④**恢复成功后重新拉当前文件并对齐草稿**——宿主机已被覆盖，若不对齐，「有未保存修改」就是假的，再点写入会把旧草稿盖回去，那等于一次无备份的破坏性写入。⑤**只回文件名/大小/时间**（Q4-A，与文件模块「不回宿主机绝对路径」同口径；compose 模块既有的 `ConfigFile`/`BackupFile` 是绝对路径，属历史形状、新端点不跟着）；**不提供删除**（Q3-A）。⑥**备份名含 `+` 与 `:`**（RFC3339 时区偏移），故读走查询串、恢复走请求体，不进 URL 路径段——与「项目名 `encodeURIComponent` 进路径段」是两套处理，别混（S7.5 已踩过一次编码坑）。⑦**备份态全部页面本地 ref、不进 store**（同编辑态纪律），换项目与退出面板时 `resetBackups()` 清空 |
| 产物内嵌（go:embed） | 前端产物经 `go:embed` 进 Hub 二进制，交付物仍是单个可执行文件。**embed 声明在 `web/assets.go`（`package assets`，`//go:embed all:dist`），不在 `internal/assets`**——`go:embed` 的 pattern 只能引用当前包目录树内的路径（不许 `..`、不许跨包），在 assets 包里写 `all:web/dist` 编译不过。`internal/assets` 以别名 import `github.com/yy1319hua/dockdeck/web` 取用 |
| 构建顺序与降级 | 打包前必须**先 `npm run build` 再 `go build`**，否则嵌进去的只是占位目录。`internal/assets` 取资源分两级：dist 内有 `index.html` → 用真产物；否则 → 回退 S1 占位页，**保证 fresh clone 只跑 `go build` 也能起 Hub 且不白屏**。该降级依赖 GAP-8（`web/dist` 目录随仓库存在，含入库的 `.gitkeep`）——目录没了，embed 会直接编译失败 |

## 构建与交付

`make dist` 产出 `linux/arm64` 与 `linux/amd64` 各一套 hub+agent（`bin/dockdeck-<cmd>-<os>-<arch>`；
当前平台单编走 `make hub` / `make agent`），Dockerfile 在 `deploy/Dockerfile.hub`、`deploy/Dockerfile.agent`
（多阶段、`FROM --platform=$BUILDPLATFORM` 交叉编译，见 S9.2）。镜像 `--platform linux/arm64,linux/amd64` buildx 推送。

产物校验 `make verify-dist`（`tools/verify_dist.py`，stdlib-only 机读 ELF 头，不依赖 file/binutils），
每产物五项：**架构**（e_machine 与文件名对账，挡交叉编译串台）、**静态性**（无 PT_INTERP，
是构建侧 `CGO_ENABLED=0` + `netgo`/`osusergo` 的二进制侧印证；静态 PIE 同样无 PT_INTERP，予以放行）、
**大小预算**（hub ≤ 24MiB / agent ≤ 16MiB，当前实际约 2 倍余量，挡依赖回归膨胀）、
**版本注入**（COMMIT 短串在产物字节内可检——漏 ldflags 时 `internal/version` 回退 unknown/dev，
运行期无法回嘴自己是哪版）。

**镜像构建路线：GitHub Actions**（S9.6 立项落地：打 tag 触发 → buildx 多架构 → 推送 `ghcr.io/yy1319hua/dockdeck-{hub,agent}`，即 compose 默认镜像地址）。私有库免费额度（2026-10 核实）：Actions 2,000 Linux 分钟/月、超额默认花费上限 $0（停跑不赊账）；GHCR 容器镜像存储与带宽当前免费（GitHub 文档原话 currently free、变更前提前通知，且不占 500MB Packages 配额）；workflow 用自带 `GITHUB_TOKEN`（`permissions: packages: write`）即可，无需额外 secret。**registry 可达性勘误**：S9.4 曾记「auth.docker.io 本网络不通」——系跳板沙箱 DNS 污染（对 docker.io 答 Meta IPv6 段且无路由）致的误判；sanbox 实测可达（`registry-1.docker.io/v2/` 返 401 即通），Actions runner 更无此问题。**基础镜像维持浮动 minor tag**（golang:1.24-alpine / node:22-alpine / alpine:3，自动收安全更新；否决钉 digest 的理由是需要主动 bump——S9.2 的「取不到 digest」理由已作废）。当前实测 digest（2026-10-17 取，备查/将来钉死用）：

- `golang:1.24-alpine` → `sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191`
- `node:22-alpine` → `sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402`
- `alpine:3` → `sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6`

开发机沙箱无 docker，Go 侧单测用 `httptest` 模拟 Docker Engine API 跑。

## 备份与恢复

`/srv/dockdeck/{data,secrets,compose-backups}` 为全部状态，`backup.sh` 打成 `tar.gz`
（先 `VACUUM INTO` 出一致性 db，再打包，`sha256` 记录，保留 14 份轮转，可选 rclone 外拷）。
恢复：解包到同路径 → `compose up -d` → `verify.sh` 冒烟。备份文件内含 master.key，权限 0600 且不外发。
面板内应急下载（S7.8）：`GET /admin/backup` 走 `VACUUM INTO` 出一致性快照经 HTTP 直下（仅会话 + 落审计），与 `backup.sh` 并存——快照等同数据库副本（含 master.key），密级与权限按 `secrets/` 对待，轮转仍归 backup.sh。

## 已验证的现场事实（2026-10-13 实测，具体主机名/地址一律不入库）

- tailnet：4 台 linux 节点在线且互相 `tailscale ping` 通；跨主机的常规管理端口被阻，
  因此一切通信只能由 Hub 主动发起的 tailnet HTTP 承担，不能指望反向 SSH。
  （2026-10-19 更新：已新增 Agent 主动反向长连接模式，该约束不再是唯一选项。）
- 目标宿主机：tailscale 1.102.2、Docker Compose v2 系、运维账号具备 docker 组权限；
  已有多个长期运行容器（面板类、监控 agent 类、开发沙箱类），**这些就是必须保护的存量**。
- 宿主机防火墙处于激活状态——部署阶段必须先验证 tailscale0 上的入站未被拦截，
  再决定是否需要 tailscale 自身的 `accept` 规则，**不改防火墙端口策略**。
- 主机名、tailnet IP、账号名见工作区 `creds/`（工作树之外，绝不入库）。
