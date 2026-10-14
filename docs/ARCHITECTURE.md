# DockDeck 架构

多服务器监控与 Docker 管理面板，监控主线参照哪吒监控（Nezha）。一个 Hub 管 N 台宿主机，
每台宿主机跑一个 Agent 采集主机指标、按需读写 docker.sock 与受限文件目录。
仅 Tailscale 私网访问，不开任何公网端口。对外提供 OpenAPI，App 与浏览器 UI 共用同一组端点。

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
浏览器 / App ──HTTPS──► Hub(:8443, 只绑 tailnet IP)
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

宿主机有公网/局域网接口，**不能绑 0.0.0.0，不能改防火墙**。做法：容器发布端口时绑定到
tailscale 网卡的具体 IP，`docker-proxy` 只在该地址监听，其它接口上根本没有监听。

```
-p <TAILNET_IP>:8443:8443     # Hub
-p <TAILNET_IP>:9527:9527     # Agent
```

`TAILNET_IP` 由部署脚本用 `tailscale ip -4` 现取，写进 `.env`，不硬编码进仓库。
Agent 启动时校验绑定地址属于 tailscale0，否则拒绝启动（`--allow-insecure-bind` 例外）。

## 认证

两套并存、互不越权的鉴权，均通过同一 `/api/*` 端点组：

- **cookie 会话（浏览器 UI）**：用户名 + 口令，**Argon2id**（口令永不入库，只存 hash+参数）。
  会话为服务端签发的随机 token，库里存 SHA-256，HttpOnly + SameSite=Strict cookie，
  空闲 8h / 绝对 72h 过期；改密、登出、失败超阈值即撤销。
- **Bearer API Token（App / 自动化）**：仅接受 `Authorization: Bearer <token>`。
  token 为服务端生成的随机串，**库里只存 SHA-256，明文仅在创建时显示一次**；
  带 scope 与过期时间，撤销后立即失效。scope：
  `read:metrics`（主机与容器指标只读）、`read:containers`（容器/镜像/Compose 清单只读）、
  `write:containers`（容器操作）、`write:files`（文件写）、`read:audit`（审计只读）。
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

- 对外契约文件：`docs/openapi.yaml`，由源码注释生成，运行时经 `GET /api/openapi.json` 暴露同一份 spec。
- **spec 与路由一致性有测试保证**：注册表与 spec 逐条比对，路由缺注解即测试红（S8.2）。
- 版本：`openapi: 3.0.3`（App 侧工具链兼容性最稳）。
- 鉴权声明：`securitySchemes` 提供 `cookieAuth` 与 `bearerAuth` 两种。
- UI 与 App 共用端点集，不为 App 另开一套路径。

## HTTP 契约

Hub API 前缀 `/api/v1`，响应统一 `{ "data": ..., "error": { "code", "message" } }`。

| Hub | scope | 说明 |
| --- | --- | --- |
| `POST /auth/login` `/auth/logout` `GET /auth/me` `POST /auth/password` | 会话 | 仅 cookie；Token 不可调用 |
| `GET/POST/PATCH/DELETE /servers*` `POST /servers/{id}/test` | 会话 | 纳管管理，UI 专属 |
| `GET/POST/DELETE /api-tokens*` | 会话 | Token 签发与撤销，UI 专属 |
| `GET /servers/{id}/overview` `GET /servers/{id}/metrics?since=&until=&step=` | `read:metrics` | **监控主线** |
| `GET /servers/overview` | `read:metrics` | 多主机聚合（哪吒式总览） |
| `GET /servers/{id}/containers` `GET .../containers/{cid}` | `read:containers` | 容器清单与详情 |
| `POST .../containers/{cid}/{action}` | `write:containers` | start/stop/restart/pause/unpause/kill/resize |
| `DELETE .../containers/{cid}` | `write:containers` | 需 `confirm=<name>` 且非保护对象 |
| `GET .../containers/{cid}/logs?tail=&since=&until=&follow=` | `read:containers` | 日志 |
| `GET .../images` `POST .../images/pull` `DELETE .../images/{ref}` `POST .../prune` | `read:`/`write:containers` | 镜像 |
| `GET .../compose` `GET/PUT .../compose/{project}/file` `POST .../compose/{project}/{action}` | `read:`/`write:containers` | Compose |
| `GET .../files?path=` `GET .../files/content?path=` | `read:metrics` | 文件列表与读取 |
| `PUT .../files/content` `POST .../files/{action}` | `write:files` | 写/删/建目录/改名，限根白名单 |
| `GET /audit` `GET /audit/export` | `read:audit` | 审计查询与导出 |
| `GET /ws` | 会话 | WebSocket：`log`、`stats` 推送 |
| `GET /api/openapi.json` | 公开 | spec 本体 |

Agent API 前缀 `/api/v1`：`/info` `/metrics` `/containers*` `/images*` `/compose*` `/logs` `/files*`，
语义与 Hub 一一对应；Hub 只做鉴权、审计、聚合，不重新实现 Docker 逻辑。

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
3. **破坏性动作三重门**：保护名单命中即拒（`settings.protected_containers` 正则，默认含 `dockdeck-*`、
   `watchtower`、部署时录入的现有容器名）；`prune` 类必须 `dry_run` 先行并在 UI 展示将被删清单；
   删除容器要求输入名字确认。所有此类操作进审计。
4. **不碰自身**：Hub/Agent 容器自身、其 volume、其网络在 API 层标记 `protected`，UI 灰置。
5. **不猜语义**：Agent 只转发 Docker API，不批量重启、不自动更新、不改 restart policy、不动宿主机 systemd 单元。
6. **失败即停**：Agent 对任何 5xx 不重试写操作；写操作全部幂等键 `X-DD-Request-Id` 落审计。

### 幂等键 X-DD-Request-Id（写操作纪律的组成）

- **形态**：请求头 `X-DD-Request-Id`，1-64 字符，限 `[A-Za-z0-9._-]`。非法值 Hub 直接 400——这个值要进审计库、进响应头、转发给 Agent，放行 CR/LF 等于给响应拆分开门。旧头 `X-Request-Id` 仅入站兼容。
- **缺省生成**：客户端不带时 Hub 现场生成（8 字符随机），响应头 `X-DD-Request-Id` 原样回显，客户端重试时带上它即可对账。
- **落审计**：写操作的审计行记录该键（`audit.request_id`），与响应头回显值一致。
- **转发 Agent**：Hub → Agent 的写请求原样转发该头；Agent 的引擎错误日志带它落行，Hub 审计与 Agent 日志可对同一次操作。
- **边界（明确不做的）**：不做执行级去重——Agent 无状态、可随意重装，记忆键等于制造漂移源；写操作不重试（见上第 6 条），重复提交由调用方负责。该键的保证是**审计关联与排查对账**，不是「同一键只执行一次」。

## 前端

Vue 3 + Vite + TS，`vue-router` + `pinia`，图表 uPlot，无重型 UI 框架，自带深色设计系统。
构建产物 `web/dist` 经 `go:embed` 进二进制，交付物仍是单个可执行文件。
页面：**服务器监控总览（主视图）** / 单主机详情（指标·容器·日志·镜像·Compose·文件）/
审计 / API Token 与设置。状态与数据须登录才显示，无免登录公开页。
危险操作二次确认，输入目标名才可提交。

## 构建与交付

`make build` 产出 `linux/arm64` 与 `linux/amd64` 各一套 hub+agent，加 `docker/Dockerfile.hub`、
`Dockerfile.agent`（多阶段、`FROM --platform=$BUILDPLATFORM` 交叉编译）。
镜像 `--platform linux/arm64,linux/amd64` buildx 推送。开发机沙箱无 docker，
构建验证在 ARM 宿主机上经 SSH 转发其 docker socket 完成；Go 侧用 `httptest` 模拟 Docker Engine API 跑单测。

## 备份与恢复

`/srv/dockdeck/{data,secrets,compose-backups}` 为全部状态，`backup.sh` 打成 `tar.gz`
（先 `VACUUM INTO` 出一致性 db，再打包，`sha256` 记录，保留 14 份轮转，可选 rclone 外拷）。
恢复：解包到同路径 → `compose up -d` → `verify.sh` 冒烟。备份文件内含 master.key，权限 0600 且不外发。

## 已验证的现场事实（2026-10-13 实测，具体主机名/地址一律不入库）

- tailnet：4 台 linux 节点在线且互相 `tailscale ping` 通；跨主机的常规管理端口被阻，
  因此一切通信只能由 Hub 主动发起的 tailnet HTTP 承担，不能指望反向 SSH。
- 目标宿主机：tailscale 1.102.2、Docker Compose v2 系、运维账号具备 docker 组权限；
  已有多个长期运行容器（面板类、监控 agent 类、开发沙箱类），**这些就是必须保护的存量**。
- 宿主机防火墙处于激活状态——部署阶段必须先验证 tailscale0 上的入站未被拦截，
  再决定是否需要 tailscale 自身的 `accept` 规则，**不改防火墙端口策略**。
- 主机名、tailnet IP、账号名见工作区 `creds/`（工作树之外，绝不入库）。
