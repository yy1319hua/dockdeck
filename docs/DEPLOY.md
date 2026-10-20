# DockDeck 部署指南（DEPLOY）

> 面向运维 / 部署者的端到端部署走查。模板底层说明见 `deploy/README.md`；安全模型见 `docs/SECURITY.md`；排障见 `docs/TROUBLESHOOT.md`。

## 1. 架构与角色

DockDeck 由两类角色组成，各自独立部署（Hub 用 `deploy/docker-compose.yml`，Agent 用 `deploy/docker-compose.agent.yml`，v0.2.6 起拆分）：

| 角色 | 职责 | 监听端口 | 挂 docker.sock | 数据 |
|------|------|------|----------------|------|
| **Hub** | 控制面：Web UI、账号/会话鉴权、API Token、审计、跨 Agent 聚合 | 8443 (TLS) | 否 | 命名卷 `dockdeck-hub-data`（SQLite / 主密钥 / 自签证书 / 整库备份）|
| **Agent** | 每台被管服务器一个：代理本机 Docker Engine | **无**（反向长连接，v0.2.6 起唯一形态）| 是（`:ro` 只读）| 无持久数据 |

- Hub 与 Agent **通常不在同一台机器**；单机双角色（Hub + 本机 Agent）也受支持（两个模板分别 up）。
- Agent 通过 Hub 在「添加服务器」中生成的 **join secret**（30 分钟有效）自注册纳管；认领成功后该 secret 即成为该机的长期令牌。
- Agent **不监听任何端口**：启动后主动向 Hub 拨出 wss 长连接，NAT 后 / 无公网 IP 同样可纳管。因此 Agent 机不需要绑定 IP、不占端口。

## 2. 前置条件

**共同**

- Docker Engine + Docker Compose **v2**（`docker compose` 子命令）。
- 能访问 ghcr.io 即可：镜像随公开库发布，**匿名可拉，部署机不需要任何凭据**（仅转回私有时才需 `read:packages` 的 PAT）。

**Hub 机额外**

- 一个具体可达的绑定 IP：Tailscale 网卡 IP（`tailscale ip -4`）或内网具体 IP。**`0.0.0.0` 会被应用层拒绝**。

**Agent 机额外**

- 出站可达 Hub 地址（`https://<hub IP 或域名>:8443`）；无需入站端口、无需绑定 IP。

**网络暴露前提（重要）**

- 服务以 `network_mode: host` 运行、应用层只绑**具体 IP**，不发布到 `0.0.0.0`。
- 暴露面依赖 **Tailnet（如 Tailscale）或内网隔离** 把守——公网直接暴露不在支持范围（见 `docs/SECURITY.md`「网络暴露模型」）。

## 3. 快速开始

在仓库 `deploy/` 目录下操作。

### 3.1 Hub 机

```bash
cd deploy
sudo ./install-hub.sh --bind-ip 100.64.0.1      # 绑定 IP，默认自动取 tailscale ip -4
```

脚本会交互式要求 GHCR 登录（token 走 `read -s` + `--password-stdin`，**不进命令行、不落盘**）。

### 3.2 Agent 机

```bash
cd deploy
sudo ./install-agent.sh --hub-url https://100.64.0.1:8443 --server-name node2
```

`--hub-url` 指向 Hub 的 Tailnet/内网地址；`--server-name` 是在 Hub 侧显示的服务器名。

## 4. 安装脚本做了什么

`install-hub.sh` / `install-agent.sh` 把手工步骤收敛成一条命令，可重复执行：

1. **预检**：docker / compose v2 / docker socket。
2. **解析绑定 IP**（仅 Hub）：默认 `tailscale ip -4` 第一条；`--bind-ip` 可强制；校验 IP 属本机网卡。
3. **拉镜像**：公开镜像直接匿名 `pull`（2026-10-19 起不再需要 GHCR 凭据）。
4. **登录兜底**：仅当匿名失败且当前是交互终端时，才提示 `docker login` 后重试；仍失败即停。
5. **落盘**：`.env`（600）、Agent 侧 `secrets/agent.token`（600，无尾随换行）。
6. **启动**：`compose up -d`。
7. **就绪确认**：Hub 探 `/api/v1/health`（恒 200）；Agent 无监听端口，以**容器进入 running + 日志出现「已纳管」**判活。

- **幂等**：已有 `.env` 不覆盖（重跑 = 按 `.env` 里的 tag 拉取 + `up -d`）；`--force` 才重写并留 `.bak`。
- **仅自检**：`./install-hub.sh --check` 只渲染 compose，不登录 / 不拉取 / 不启动。
- 纳管自注册在 Agent 启动期**同步完成**（v0.2.6 起）：Hub 不可达会无限重试，凭据/邀请类错误直接退出——脚本按容器状态与启动日志汇报。

## 5. 手工部署（脚本不可用时）

```bash
cd deploy
cp .env.example .env
# 编辑 .env：DD_BIND_IP 必填（tailscale ip -4 或内网具体 IP）——Hub 机用

# Agent 机另备共享令牌文件（权限 600，路径在 .gitignore 内）
install -m 600 /dev/null secrets/agent.token
printf '%s' '<Hub 侧纳管时生成的 Agent 令牌>' > secrets/agent.token
# Agent 的 .env 只需三项：DD_HUB_URL / DD_AGENT_JOIN_SECRET / DD_AGENT_IMAGE
# （Hub 地址是 v0.2.6 起的硬前提，留空则 Agent 启动即失败）

# Hub 机（不带 --profile 则什么都不起，防误起）
docker compose --profile hub up -d     # Hub 机
# Agent 机（独立模板，单服务无 profile）
docker compose -f docker-compose.agent.yml up -d

# 启动前自检（不需要守护进程）
docker compose --profile hub config                        # Hub
docker compose -p dockdeck-agent -f docker-compose.agent.yml config   # Agent
```

镜像默认 `ghcr.io/yy1319hua/dockdeck-{hub,agent}:latest`，由 `.github/workflows/ci.yml` 在打 `v*` tag 时经 GitHub Actions 构建推送（双架构、免 QEMU）。

## 6. 网络与暴露面

| 决策 | 理由 |
|------|------|
| `network_mode: host` + 应用层 `--listen ${DD_BIND_IP}` | Hub 禁绑 `0.0.0.0`（`config.Validate`）；桥接网络容器 IP 运行时分配、写不进启动参数。host 下 Tailscale 网卡直接可见，暴露面由应用绑定白名单把守，且少一层 docker-proxy |
| `DD_BIND_IP` 必填（`${VAR:?…}` fail-fast） | 仅 Hub 需要；compose 层报错比容器起来后应用层报错更早、更明确 |
| Agent token 走 `--token-file` 挂载 | `docker inspect` 看不到文件内容；600 权限；路径不入库（否决环境变量 `DD_AGENT_TOKEN`）|
| socket `:ro` + Agent root | socket 属主 `root:docker`；`:ro` 与自我保护名单双边界 |
| 固定 `container_name` + `--self-container` | guard 的「不碰自身容器」跨重建稳定 |
| Hub / Agent 双模板（v0.2.6 拆分） | compose 对未激活 profile 的服务也做变量插值：旧单文件里 Agent 的 `${DD_BIND_IP:?}` 让「只起 Agent 的机器」莫名失败；拆开后 Hub 模板还能靠 profile 防误起 |

## 7. 配置项（`.env.example`）

| 变量 | 必填 | 说明 |
|------|------|------|
| `DD_BIND_IP` | ✅（仅 Hub） | Hub 对外绑定 IP（tailscale / 内网具体 IP）；不填 compose 直接报错，不带着空地址启动 |
| `DD_HUB_TLS_MODE` | — | `self`（自签，默认，指纹见设置页）/ `none`（明文，仅允许回环，反代场景用）|
| `DD_HUB_IMAGE` / `DD_AGENT_IMAGE` | — | 镜像与版本；不填用 compose 默认值 |
| `DD_HUB_URL` | ✅（Agent，v0.2.6 起） | Hub 地址 `https://<hub IP 或域名>:8443`；反向唯一形态下这是硬前提，留空 Agent 启动即失败 |
| `DD_AGENT_JOIN_SECRET` | ✅（Agent） | 一次性纳管密钥（30 分钟有效）；认领后即长期令牌 |

> Agent 令牌走 `secrets/agent.token` 文件（600），**不**走环境变量。

## 8. 部署后验证

`deploy/verify.sh`（S10.3，只读、可重复）覆盖两类断言：

1. **冒烟**：Hub `/api/v1/health`→200、`/api/v1/api-tokens`→401（会话鉴权生效）；Agent 无监听端口（v0.2.6 起反向唯一形态），断言**容器在跑 + 启动日志含「已纳管」**，并核对 `.env` 的 `DD_HUB_URL`（硬前提）。
2. **存量容器不变性**：部署没有扰动宿主机上既有的其它容器。基线 `${dir}/.verify-baseline` 首跑惰性建立，`--init-baseline` 可锁基准；`--protect` 指定必保容器（如 `watchtower`/`komari`）；非 `dockdeck-` 前缀容器须在运行。

```bash
sudo ./verify.sh                # 自动探测角色，全量验证
sudo ./verify.sh --role hub     # 只验 Hub
sudo ./verify.sh --protect watchtower,komari
sudo ./verify.sh --init-baseline   # 只记录当前容器基线后退出
```

退出码：0 全部通过；1 有断言失败；2 用法 / 预检错误。

## 9. 升级

```bash
cd deploy
# 1. 改 .env 里的镜像 tag（或保持 latest）
vim .env
# 2. 拉新镜像并重启对应角色（Agent 用独立模板）
docker compose --profile hub pull && docker compose --profile hub up -d
docker compose -p dockdeck-agent -f docker-compose.agent.yml pull \
  && docker compose -p dockdeck-agent -f docker-compose.agent.yml up -d
```

Hub 数据在命名卷，升级不丢；升级前建议先备份（见下）。

## 10. 备份与恢复

- `deploy/backup.sh`：VACUUM INTO + tar + sha256 清单 + 轮转，备份落宿主 `/opt/dockdeck/backups`（不进卷）。
- `deploy/restore.sh`：sha256 校验 → 恢复前快照 → 交互确认 → 停容器 → 清卷 → 解包 → 起容器。
- 也支持 Hub 设置页一键备份 / 恢复，不直接碰卷。

> 真机 backup-restore 演练留 S11.5。

## 11. 常用运维命令

```bash
docker compose --profile hub logs -f        # 看 Hub 日志
docker compose -p dockdeck-agent -f docker-compose.agent.yml logs -f   # 看 Agent 日志
docker compose --profile hub ps              # Hub 状态
docker compose -p dockdeck-agent -f docker-compose.agent.yml ps        # Agent 状态
docker compose --profile hub down            # 停止 Hub（不删卷）
docker compose config                        # 渲染自检（须带 --profile）
```

日志策略：json-file 轮转 10m × 3 份，防止容器日志吃满磁盘。

## 12. 参见

- `deploy/README.md` —— 模板与设计要点
- `docs/SECURITY.md` —— 安全模型、鉴权、网络暴露
- `docs/TROUBLESHOOT.md` —— 排障手册
- `docs/API.md` / `docs/openapi.json` —— API 面
