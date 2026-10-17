# DockDeck Compose 部署

`docker-compose.yml` 把 S9.2 的两份镜像（`Dockerfile.hub` / `Dockerfile.agent`）组成
双角色部署模板：**Hub**（控制面，一台）与 **Agent**（每台被管主机一个）。两者通常
不在同一台机器；单机双角色（Hub + 本机 Agent）也受支持。

## 快速开始

```bash
# 1. 环境文件（.env 不入库；.gitignore 已覆盖）
cp .env.example .env
#    编辑 .env：DD_BIND_IP 必填（tailscale ip -4 或内网具体 IP）

# 2.（仅 Agent 机）共享令牌文件——权限 600，路径在 .gitignore 内
install -m 600 /dev/null secrets/agent.token
printf '%s' '<Hub 侧纳管时生成的 Agent 令牌>' > secrets/agent.token

# 3. 按角色启动（不带 --profile 则什么都不起，防误起）
docker compose --profile hub   up -d     # Hub 机
docker compose --profile agent up -d     # Agent 机
docker compose --profile hub --profile agent up -d   # 单机双角色

# 4. 启动前自检 / 查看渲染结果（不需要守护进程；--profile 必须带，
#    否则 profile 过滤会把服务全滤掉、渲染出空 services）
docker compose --profile hub --profile agent config
```

升级：改 `.env` 里的镜像 tag → `docker compose --profile <角色> pull` → 再 `up -d`。
Hub 数据（SQLite / 主密钥 / 自签证书 / 整库备份）在命名卷 `dockdeck_dockdeck-hub-data`
（`dockdeck_` 前缀来自文件顶部 `name: dockdeck`）；备份与恢复走 Hub 设置页，不直接碰卷。

## 设计要点（为什么这样做）

| 决策 | 理由 | 被否掉的替代 |
| --- | --- | --- |
| `network_mode: host` + 应用层 `--listen ${DD_BIND_IP}` | Hub 禁绑 0.0.0.0（`config.Validate`）；桥接网络容器 IP 运行时分配、写不进启动参数。host 下 tailscale0 直接可见，暴露面由应用绑定白名单把守，且少一层 docker-proxy | 桥接 + `-p BIND_IP:8443:8443`：要么 entrypoint 探测容器 IP，要么让应用绑 0.0.0.0 破自己的校验契约 |
| `DD_BIND_IP` 必填（`${VAR:?…}` fail-fast） | 两个角色都需要它；compose 层报错比容器起来后应用层报错更早、更明确 | 留空默认：`:8443` 空 host 要等应用启动才炸 |
| Agent token 走 `--token-file` 绑定挂载 | `docker inspect` 看不到文件内容；600 权限；路径不入库 | `DD_AGENT_TOKEN` 环境变量：`docker inspect` 可见，共享主机上即泄漏 |
| socket `:ro` + Agent root | socket 属主 `root:docker`，非 root 摸不到；`:ro` 与自我保护名单双边界 | 入宿主 docker 组：GID 逐机不同、运行时才知 |
| 固定 `container_name` + `--self-container` | guard 的 IsSelf 是双向前缀匹配；hostname 默认是容器短 ID（重建即变），固定名让「不动自己」稳定 | 默认 hostname：每次重建自我保护失效 |
| 单文件双 profiles | 一份约定（镜像名 / 重启策略 / 日志）不重复；同机双角色一条命令 | 两份独立文件：约定重复易漂移，双角色两个 project 难管 |

未做（后续按需加固）：`read_only` + tmpfs（Agent 证书目录要可写，行为变化需另行
验证）、healthcheck（要绑 IP 且自签证书需 `--no-check-certificate`）、镜像 digest
钉死（S9.4 产物校验时锁）。

## 文件清单

| 文件 | 说明 |
| --- | --- |
| `docker-compose.yml` | 模板本体（入库） |
| `.env.example` | 环境变量样例（入库）；实用时复制为 `.env`（**不入库**） |
| `README.md` | 本文件 |
| `.env` / `secrets/` | 本地凭据，`.gitignore` 覆盖，**永不入库** |
