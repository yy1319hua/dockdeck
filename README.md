# DockDeck

多服务器 Docker 管理面板。**一个 Hub，管 N 台宿主机**——每台宿主机跑一个轻量 Go Agent，
只读 Docker socket 并按需执行操作；Hub 负责登录、聚合、审计与界面。

- 技术栈：Go（`net/http` 标准库路由）+ Vue 3 + SQLite（`modernc.org/sqlite`，纯 Go 无 CGO）
- 架构：兼容 `linux/arm64` 与 `linux/amd64`，`CGO_ENABLED=0` 静态编译
- 访问：账号密码登录 + Tailscale 私网，**不开任何公网端口、不改防火墙**
- 覆盖能力：服务器监控、容器管理、日志、镜像、Compose、审计日志

> 状态：开发中。当前进度见 `docs/ARCHITECTURE.md`；架构与接口契约见 `docs/ARCHITECTURE.md`。

## 为什么是 Hub + Agent

面板要管的不是一台机器，而是散在各处的宿主机。让浏览器直连每台机器的 Docker API 意味着
N 份证书、N 套口令、N 个监听端口——不可维护，也不安全。DockDeck 把信任边界收在一处：
浏览器只面对 Hub，Hub 只面对自己纳管的 Agent，Agent 只监听在 tailscale 网卡上、
只认一个每机独立的 token。审计日志因此天然集中在 Hub，权限模型也只需要在一处守。

## 快速开始（部署）

Hub 与 Agent 都跑在宿主机自己的 tailscale IP 上：

```
# Hub（在你想当管理机的那台上）
deploy/install-hub.sh                      # 取 tailnet IP → 生成密钥 → 建卷 → compose up

# Agent（在每台被管理的机器上，由 Hub 侧推送或手动执行）
deploy/install-agent.sh --name armbian --hub https://<hub-tailnet-ip>:8443
```

脚本细节、参数与回滚见 `docs/DEPLOY.md`。首次访问用 Hub 安装时输出的自签证书指纹核对，
再在界面上完成建号。

## 存量容器保护

面板跑在一台已经有生产容器的机器上，所以这不是可选项：

- 写 Compose 文件前先落 `*.bak.<时间戳>`，改动以 diff 预览确认后才提交
- 破坏性操作三重门：保护名单命中即拒、prune 必须 dry-run 先行并列出待删清单、删除需输入名字确认
- Hub/Agent 自身容器、卷、网络在 API 层标记 `protected`，界面上直接灰置
- Agent 不做批量重启、不改 restart policy、不动宿主机 systemd 单元

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
