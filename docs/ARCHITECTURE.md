# DockDeck 架构

多服务器 Docker 管理面板。一个 Hub 管 N 台宿主机，每台宿主机跑一个 Agent 读 docker.sock。
仅 Tailscale 私网访问，不开任何公网端口。

## 组件

```
浏览器 ──HTTPS──► Hub(:8443, 只绑 tailnet IP)
                   ├─ Go 单二进制，前端 go:embed
                   ├─ SQLite（用户/服务器/会话/审计/指标快照）
                   └─ Agent Client ──HTTP+Bearer──► Agent(:9527, 每台宿主机)
                                                      └─ Docker Engine API (unix socket)
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

- 用户侧：用户名 + 口令，**Argon2id**（口令永不入库，只存 hash+参数）。
  会话为服务端签发的随机 token，库里存 SHA-256，HttpOnly + SameSite=Strict cookie，
  空闲 8h / 绝对 72h 过期；改密、登出、失败超阈值即撤销。
- Hub 侧：自签证书 + 指纹写入设置页；可用 `DD_TLS_MODE=acme|self|none` 切换。
  `none` 只允许监听 127.0.0.1，用于反代场景。
- Hub→Agent：每台服务器一个 256bit 随机 token，`Authorization: Bearer`；
  库中以机器主密钥（`/srv/dockdeck/secrets/master.key`，0600，首次启动生成）做 AES-256-GCM 加密存储。
  写操作附带 `X-DD-Timestamp` + `X-DD-Signature`（HMAC-SHA256 over method+path+body），Agent 校验，防重放 ±120s。

## 数据模型（SQLite，WAL）

| 表 | 作用 |
| --- | --- |
| `users` | username、argon2 hash+参数、禁用标记、last_login |
| `sessions` | token hash、user、过期、IP、UA |
| `servers` | name、base_url、token(密)、arch、docker 版本、启用、last_seen、最近一次健康结果 |
| `audit_logs` | ts、user、server、action、target、detail(JSON)、result、error、IP |
| `metrics_snapshots` | server、ts、cpu%/mem/disk/net、每容器 cpu/mem（滚动 7 天，30s 采样） |
| `settings` | key/value：看板阈值、备份策略、保护名单等 |

驱动固定 `modernc.org/sqlite`（纯 Go，无 CGO）——这是 `CGO_ENABLED=0` 交叉编译 ARM64/x86_64 的前提。
迁移为 `internal/db/migrations/*.sql` 顺序执行，带 `schema_migrations` 版本表。

## HTTP 契约

Hub API 前缀 `/api/v1`，响应统一 `{ "data": ..., "error": { "code", "message" } }`。

| Hub | 说明 |
| --- | --- |
| `POST /auth/login` `/auth/logout` `GET /auth/me` `POST /auth/password` | 登录态 |
| `GET /servers` `POST /servers` `PATCH /servers/{id}` `DELETE /servers/{id}` `POST /servers/{id}/test` | 纳管 |
| `GET /servers/{id}/overview` `GET /servers/{id}/metrics` | 监控 |
| `GET /servers/{id}/containers` `GET .../containers/{cid}` `POST .../{action}` | 容器 action: start/stop/restart/pause/unpause/kill/update/resize |
| `DELETE .../containers/{cid}` | 需 `confirm=<name>` 且非保护对象 |
| `GET .../containers/{cid}/logs?tail=&since=&until=&follow=` `GET .../export` | 日志 |
| `GET .../images` `POST .../images/pull` `DELETE .../images/{ref}` `POST .../prune` | 镜像 |
| `GET .../volumes` `GET .../networks` | 附带只读 |
| `GET .../compose` `GET/PUT .../compose/{project}/file` `POST .../compose/{project}/{action}` | action: up/down/restart/pull/logs |
| `GET /audit` | 审计查询（过滤 user/server/action/区间） |
| `GET /ws` | WebSocket：`log`、`stats` 推送 |

Agent API 前缀 `/api/v1`：`/info` `/stats` `/containers*` `/images*` `/compose*` `/logs` `/exec-logs`，
语义与 Hub 一一对应；Hub 只做鉴权、审计、聚合，不重新实现 Docker 逻辑。

## 现有容器与数据保护（不可协商）

1. **只读默认**：Hub 不主动写宿主机任何文件；唯一写入路径是用户显式提交 Compose YAML。
2. **备份优先于变更**：写 Compose 文件前先落 `*.bak.<RFC3339>`；SQLite 用 `VACUUM INTO` 出快照，不复制运行中的 db 文件。
3. **破坏性动作三重门**：保护名单命中即拒（`settings.protected_containers` 正则，默认含 `dockdeck-*`、
   `watchtower`、部署时录入的现有容器名）；`prune` 类必须 `dry_run` 先行并在 UI 展示将被删清单；
   删除容器要求输入名字确认。所有此类操作进审计。
4. **不碰自身**：Hub/Agent 容器自身、其 volume、其网络在 API 层标记 `protected`，UI 灰置。
5. **不猜语义**：Agent 只转发 Docker API，不批量重启、不自动更新、不改 restart policy、不动宿主机 systemd 单元。
6. **失败即停**：Agent 对任何 5xx 不重试写操作；写操作全部幂等键 `X-DD-Request-Id` 落审计。

## 前端

Vue 3 + Vite + TS，`vue-router` + `pinia`，图表 uPlot，无重型 UI 框架，自带深色设计系统。
构建产物 `web/dist` 经 `go:embed` 进二进制，交付物仍是单个可执行文件。
页面：登录 / 服务器总览 / 单服务器详情（容器·日志·镜像·Compose·监控）/ 审计 / 设置。
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
