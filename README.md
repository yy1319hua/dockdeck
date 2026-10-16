# DockDeck

多服务器**监控与 Docker 管理**面板，监控主线参照哪吒监控。**一个 Hub，管 N 台宿主机**——
每台宿主机跑一个轻量 Go Agent，采集主机指标、只读 Docker socket 并在受限目录内做文件操作；
Hub 负责登录、聚合、审计与界面，并对外提供 OpenAPI 供 App 直连调用。

- 技术栈：Go（`net/http` 标准库路由）+ Vue 3 + SQLite（`modernc.org/sqlite`，纯 Go 无 CGO）
- 架构：兼容 `linux/arm64` 与 `linux/amd64`，`CGO_ENABLED=0` 静态编译
- 访问：账号密码登录 + Tailscale 私网或内网反代，**不开任何公网端口、不改防火墙**
- 含：主机监控（CPU/内存/磁盘/网络/负载/在线率/历史曲线）、容器管理、日志、镜像、Compose、
  文件管理、审计日志、OpenAPI + API Token
- 不含（明确排除）：服务拨测、告警通知、Web 终端、免登录公开状态页

> 状态：开发中。当前进度见 `docs/ARCHITECTURE.md`；架构与接口契约见 `docs/ARCHITECTURE.md`。

## 为什么是 Hub + Agent

面板要管的不是一台机器，而是散在各处的宿主机。让浏览器或 App 直连每台机器的 Docker API 意味着
N 份证书、N 套口令、N 个监听端口——不可维护，也不安全。DockDeck 把信任边界收在一处：
客户端只面对 Hub，Hub 只面对自己纳管的 Agent，Agent 监听在 tailscale 网卡或指定网卡上、
只认一个每机独立的 token。审计日志因此天然集中在 Hub，权限模型也只需要在一处守。

## OpenAPI 与 App 调用

UI 与 App 共用同一组 `/api/*` 端点。契约与运行时路由**同源生成**：`cmd/hub --dump-openapi <dir>` 复用运行时
同一份路由声明写出 `docs/openapi.json` 与 `docs/openapi.yaml`（两者都入库，勿手改），运行时暴露在
`GET /api/openapi.json`。spec 与路由有一致性测试保证（漏注解即测试红）。

App 侧使用 Bearer API Token（与浏览器 cookie 会话互不越权），token 只在创建时明文显示一次，
库里存 SHA-256。scope 共六项：`read:metrics`、`read:containers`、`write:containers`、`read:files`、
`write:files`、`read:audit`（字典与顺序以 `GET /api/v1/scopes` 为准，即 `internal/apitoken.AllScopes`）。

```
curl -H "Authorization: Bearer <token>" \
     "https://<hub-tailnet-ip>:8443/api/v1/servers/1/metrics?since=-1h&step=60s"
```

## 快速开始（部署）

Hub 与 Agent 都跑在宿主机上（默认 tailscale IP，也支持内网 IP + 反代）：

```
# Hub（在你想当管理机的那台上）
deploy/install-hub.sh                      # 取 tailnet / 内网 IP → 生成密钥 → 建卷 → compose up

# Agent（在每台被管理的机器上，由 Hub 侧推送或手动执行）
deploy/install-agent.sh --name <节点名> --hub https://<hub-tailnet-ip>:8443
```

脚本细节、参数与回滚见 `docs/DEPLOY.md`。首次访问用 Hub 安装时输出的自签证书指纹核对，
再在界面上完成建号。

## 存量容器保护

面板跑在一台已经有生产容器的机器上，所以这不是可选项：

- 写 Compose 文件前先落 `*.bak.<时间戳>`，改动以 diff 预览确认后才提交
- 破坏性操作三重门：保护名单命中即拒、prune 必须 dry-run 先行并列出待删清单、删除需输入名字确认
- Hub/Agent 自身容器、卷、网络在 API 层标记 `protected`，界面上直接灰置
- Agent 不做批量重启、不改 restart policy、不动宿主机 systemd 单元

## 文件管理边界

文件读写是新加的写能力，因此锁得比别处更紧：

- Agent 必须显式配 `--fs-root` 指定可访问目录，否则该模块根本不注册路由
- 路径先解引用符号链接、再做白名单前缀校验，拒绝 `..` 逃逸，校验发生在任何 IO 之前
- 私钥、凭据目录、`.env` 一类 pattern 不可读不可写
- 没有 exec、没有终端：上传的文件不会被执行
- 全部写操作需 `write:files` scope 并落审计

## 备份与恢复

全部状态在 `/srv/dockdeck/{data,secrets,compose-backups}`：

```
scripts/backup.sh          # VACUUM INTO 一致性快照 + tar.gz + sha256 + 轮转保留
scripts/restore.sh <归档>
scripts/verify.sh          # API 冒烟 + 部署前后 docker ps 快照 diff（存量不变性断言）
```

## 开发

```
export PATH=$PATH:/usr/local/go/bin        # 沙箱里 Go 不在默认 PATH
TZ=Asia/Shanghai go test ./...             # 固定时区，防时间类用例假红
cd web && npm ci && npm run build          # 产物 web/dist 由 go:embed 吸收
make build                                 # hub/agent × arm64/amd64
```

开发沙箱内**没有 docker、没有 tailscale**；需要真实引擎验证时在 ARM 宿主机上做，
或转发宿主机的 docker socket。多 AI 接力的协作与自检约定见 `docs/ARCHITECTURE.md`。

## 许可

私有仓库 + 源码公开可用：采用 [PolyForm Noncommercial License 1.0.0](https://polyformproject.org/licenses/noncommercial/1.0.0)。

你可以自由阅读、使用、修改、分发本项目——**仅限非商业目的**。想把 DockDeck 用于商业场景
（售卖、作为商业产品服务交付、闭源商用分发等）需另行取得授权。个人自用、学习、研究、
以及慈善/教育/公共研究等非营利组织使用均在许可范围内。

仓库保持私有是访问控制，与许可不冲突：许可管的是「拿到副本的人能做什么」。
源码注释一律用 `SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0` 单行标识，
不在每个文件重复许可正文。
