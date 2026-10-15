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
| `GET .../compose` `POST .../compose/{project}/{action}` `GET .../compose/{project}/logs` `GET/PUT .../compose/{project}/file` `POST .../compose/{project}/diff` | `read:`/`write:containers` | Compose（list/写动作/日志；YAML 读写与 diff 预览。Agent 侧 S4.2+S4.3 落地，**Hub 侧转发 S4.7 补齐**。写 YAML 走 `ComposeDirs` 白名单而非 `fs-root`，故归 `write:containers` 而非 `write:files`） |
| `GET .../files?path=` `GET .../files/stat?path=` `GET .../files/content?path=` | `read:files` | 文件列表/元信息/读取（**scope 由原定的 `read:metrics` 改为 `read:files`**：与「写不蕴含读、读文件≠读指标」的既有口径一致。Agent 侧 S5.1 已落地，Hub 侧路由已随 S5.5 落地） |
| `PUT .../files/content` `POST .../files/{action}` | `write:files` | 写/删/建目录/改名，限根白名单，写前自动 `*.bak.<RFC3339>` 备份（Agent 侧 S5.1 已落地，Hub 侧路由已随 S5.5 落地） |
| `GET /audit` `GET /audit/export` | `read:audit` | 审计查询与导出 |
| `GET /ws` | 会话 | WebSocket：`log`、`stats` 推送 |
| `GET /api/openapi.json` | 公开 | spec 本体 |

Agent API 前缀 `/api/v1`：`/info` `/metrics` `/containers*` `/images*` `/compose*` `/logs` `/files*`，
语义与 Hub 一一对应；Hub 只做鉴权、审计、聚合，不重新实现 Docker 逻辑。

### Engine 契约实测基线（2026-10-14，node1 生产机 + ARM 宿主机，Docker Engine 29.1.3 / API 1.52）

以下行为在 node1（10.0.0.10，Docker 29.1.3）与 ARM 宿主机（同版本）上**只读实测**确认，
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
| 认证错误语义 | 登录失败后端**统一回 `invalid_credentials`**（「用户不存在」与「口令错」刻意不区分，避免泄漏用户名是否存在），前端**不得**自作主张提示「用户名不存在」；`account_locked`(429)、`account_disabled`(403)、`already_initialized`(409) 的语义由后端 message 直接呈现。建号唯一硬规则是口令 ≥ 8 位（后端 `weak_password`），前端先拦一次省往返，后端仍复查 |
| 跳转安全 | 登录后 `redirect` 参数**只接受站内路径**：必须以单个 `/` 开头且不以 `//` 开头——后者是协议相对地址，会把用户送到外站（开放重定向） |
| 总览页的三条硬约束 | ①**在线以后端 `servers.last_seen` 为准**：轮询器成败都更新它（只在成功时写会让掉线机器永远显示在线）。②**网络速率不能从实时快照算**：后端 `NetStat` 是开机累计值，前端只有最新一次快照，故用历史曲线最后两点求差；点数不足时显示累计值并标注，不许编造速率。③**uPlot 是 canvas，不解析 CSS 变量**：配色在运行时从 `:root` 读 `--chart-*`（值仍出自 tokens.css）。刷新周期 30s 与采集对齐，页面卸载必须停表 |
| servers 列表的字段口径 | 后端 `ServerSummary` 无 json tag，线上字段是 Go 导出名（**`ID`/`Name`/`BaseURL`/`Enabled`/`LastSeen`/`LastError`**）。前端在 `api/servers.ts` 用 `toServer` 单点映射成 camelCase——后端若补 json tag，改这一处即可 |
| 详情页的口径（S7.2） | ①**一图一指标，不塞多序列双 Y 轴**：四项指标量纲不同（百分比 vs 字节），双轴图像两个指标相关实则误导；网络上下行同量纲是唯一画两条线的例外，且画的是**速率**（相邻点差分，首点无前值留空断线，不画成 0）。②**进程只有总数**：后端无进程列表端点，详情页显示 `HostMetrics.processes` 总数并注明「明细待后端立项」，不造进程表。③**`loading` 与 `refreshing` 分开**：首载/切区间走 `loading`（区间按钮禁用防并发），后台轮询走 `refreshing`（只显示「刷新中…」，不动按钮）；轮询与首载共用同一条 `pull`（实时快照 + 当前区间曲线），口径不劈叉；轮询失败保留上次成功数据、只把原因记进 error——监控页被一次网络抖动清成空白比显示旧数据更糟。区间档 1h/6h/24h/7d 对应后端窗口上限 8 天，`limit` 取 720 |
| 指标采集链路 | Agent 暴露 `/api/v1/metrics`，由 **Hub 侧 `internal/metricsync` 定时拉取**（默认 30s，可配）写入 `metrics_latest`（实时快照）+ `metrics_snapshots`（历史点）。单台超时 10s、一台失败不影响其余；容器指标拿不到降级为 `null`（前端据此区分「无容器」与「空列表」）；历史保留 14 天、每小时清过期。**没有这个轮询器，所有指标端点恒 404**——它是 S7 图表的唯一数据源 |
| 容器页的口径（S7.3） | ①**两个数据源分开降级**：身份/状态来自 Agent 实时转发（`GET /servers/{id}/containers`，失败即错误）；CPU/内存占用来自 `metrics_latest` 的 `ContainerStat`（拿不到只显示「—」不算失败；`MemLimit=0` 表示未设上限，此时百分比无意义、不展示）。占用按容器 ID 合并，stats 的 ID 与清单不一致时退到按名字匹配。②**删除的 cid 用容器名**：后端 `guard.Confirm` 要求 confirm 与 URL 里的 cid 完全一致，让用户输名字而不是 64 位 ID（Engine 接受名字作容器引用）。③**确认范围**：remove 输名确认 + force/v 勾选；stop/kill 简单确认（kill 带信号下拉，白名单由后端校验）；start/restart/pause/unpause 直接执行、toast 回报。④**日志端点是全表唯一 NoEnvelope**：裸流 `text/plain`，成功无 `{data}` 信封、失败又是 `{error}` 信封，不能套 `request()`，单独直 fetch；follow 用 ReadableStream 边收边吐、AbortController 停止（流寿命归浏览器 ctx）；文本 2 MB 上限从头部截断。⑤**确认弹层的 Transition 拆两条**（遮罩淡入/面板滑入）——slide 的 transform 若包住 `position:fixed` 的面板，变换中的祖先会成为固定定位的包含块，面板错位 |
| 产物内嵌（go:embed） | 前端产物经 `go:embed` 进 Hub 二进制，交付物仍是单个可执行文件。**embed 声明在 `web/assets.go`（`package assets`，`//go:embed all:dist`），不在 `internal/assets`**——`go:embed` 的 pattern 只能引用当前包目录树内的路径（不许 `..`、不许跨包），在 assets 包里写 `all:web/dist` 编译不过。`internal/assets` 以别名 import `github.com/yy1319hua/dockdeck/web` 取用 |
| 构建顺序与降级 | 打包前必须**先 `npm run build` 再 `go build`**，否则嵌进去的只是占位目录。`internal/assets` 取资源分两级：dist 内有 `index.html` → 用真产物；否则 → 回退 S1 占位页，**保证 fresh clone 只跑 `go build` 也能起 Hub 且不白屏**。该降级依赖 GAP-8（`web/dist` 目录随仓库存在，含入库的 `.gitkeep`）——目录没了，embed 会直接编译失败 |

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
