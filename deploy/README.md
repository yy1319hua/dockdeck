# DockDeck Compose 部署

S9.2 的两份镜像（`Dockerfile.hub` / `Dockerfile.agent`）对应两个角色：**Hub**
（控制面，一台）与 **Agent**（每台被管服务器一个）。两者通常不在同一台机器；
单机双角色（Hub + 本机 Agent）也受支持。

v0.2.6 起模板拆成两份（原因见下「设计要点」）：

| 模板 | 角色 | 启动方式 |
| --- | --- | --- |
| `docker-compose.yml` | Hub（+ Cloudflare Tunnel，按 profile） | `docker compose --profile hub up -d` |
| _(Agent 无模板)_ | Agent 由面板「添加服务器」生成命令 | Hub 面板「添加服务器」→ 复制命令到目标机执行 |

Agent 只有**反向长连接**一种形态（v0.2.6 起）：不监听端口，启动后主动连 Hub
的 WebSocket，NAT 后 / 无公网 IP 同样可纳管。因此 Agent 机不需要绑定 IP。

## Hub 用安装脚本，Agent 用容器部署

Hub 机用 `install-hub.sh` 一条命令装好：

```bash
sudo ./install-hub.sh --bind-ip 100.64.0.1
```

Agent 机则直接用容器部署：Hub 面板「添加服务器」给出 `docker run` / `compose`
两种命令，贴到目标机执行即可（join_secret 在面板生成，30 分钟内有效）。
Agent 不再分发安装脚本——命令由面板按当前参数现生成，不会与面板版本错配。

Hub 脚本做的事：预检（docker / compose v2 / socket）→ 取绑定 IP（默认
`tailscale ip -4`）→ 拉镜像（公开镜像，匿名）→ 写 `.env` 与 `secrets/agent.token`
（均 600）→ `compose up -d` → 就绪确认（探 `/api/v1/health`，恒 200）。

在装不了 docker 守护进程的机器上可以只做渲染自检：`./install-hub.sh --check`。
重跑不会覆盖已有 `.env`（要重写加 `--force`）；完整参数见 `--help`。

## 手工步骤（脚本不可用时）

镜像默认 `ghcr.io/yy1319hua/dockdeck-{hub,agent}:latest`，随公开库发布故为**公开镜像**，
匿名即可拉取，**部署机不需要任何 GHCR 凭据**：

```bash
docker pull ghcr.io/yy1319hua/dockdeck-hub:v0.2.1   # 无需 docker login
```

只有镜像转回私有时才需要 `docker login ghcr.io`（PAT 需 `read:packages`）。

镜像由 `.github/workflows/ci.yml` 在打 `v*` tag 时经 GitHub Actions 构建并推送
（双架构、免 QEMU；tag 名与 `latest` 两个 tag）。

```bash
# 1. 环境文件（.env 不入库；.gitignore 已覆盖）
cp .env.example .env
#    编辑 .env：DD_BIND_IP 必填（tailscale ip -4 或内网具体 IP）——Hub 机用

# 2.（仅 Agent 机）共享令牌文件——权限 600，路径在 .gitignore 内
install -m 600 /dev/null secrets/agent.token
printf '%s' '<Hub 侧纳管时生成的 Agent 令牌>' > secrets/agent.token
#    Agent 的 .env 另需 DD_HUB_URL / DD_AGENT_JOIN_SECRET（v0.2.6 起前者是硬前提）

# 3. 按角色启动（Hub 不带 --profile 则什么都不起，防误起）
docker compose --profile hub up -d                        # Hub 机
# Agent 机：用面板「添加服务器」生成的 compose 命令（仓库不提供手写模板）

# 4. 启动前自检 / 查看渲染结果（不需要守护进程）
docker compose --profile hub config                       # Hub
# Agent 自检：用面板生成的 compose 文件名替换后 config
```

升级：改 `.env` 里的镜像 tag → `pull` → 再 `up -d`；Agent 用
`面板「添加服务器」重新生成的命令拉新镜像并重建。
Hub 数据（SQLite / 主密钥 / 自签证书 / 整库备份）在命名卷 `dockdeck_dockdeck-hub-data`
（`dockdeck_` 前缀来自文件顶部 `name: dockdeck`）；备份与恢复走 Hub 设置页，不直接碰卷。

## 设计要点（为什么这样做）

| 决策 | 理由 | 被否掉的替代 |
| --- | --- | --- |
| `network_mode: host` + 应用层 `--listen ${DD_BIND_IP}` | Hub 禁绑 0.0.0.0（`config.Validate`）；桥接网络容器 IP 运行时分配、写不进启动参数。host 下 tailscale0 直接可见，暴露面由应用绑定白名单把守，且少一层 docker-proxy | 桥接 + `-p BIND_IP:8443:8443`：要么 entrypoint 探测容器 IP，要么让应用绑 0.0.0.0 破自己的校验契约 |
| `DD_BIND_IP` 必填（`${VAR:?…}` fail-fast） | 仅 Hub 需要；compose 层报错比容器起来后应用层报错更早、更明确 | 留空默认：`:8443` 空 host 要等应用启动才炸 |
| Agent token 走 `--token-file` 绑定挂载 | `docker inspect` 看不到文件内容；600 权限；路径不入库 | `DD_AGENT_TOKEN` 环境变量：`docker inspect` 可见，共享服务器上即泄漏 |
| socket `:ro` + Agent root | socket 属主 `root:docker`，非 root 摸不到；`:ro` 与自我保护名单双边界 | 入宿主 docker 组：GID 逐机不同、运行时才知 |
| 固定 `container_name` + `--self-container` | guard 的 IsSelf 是双向前缀匹配；hostname 默认是容器短 ID（重建即变），固定名让「不动自己」稳定 | 默认 hostname：每次重建自我保护失效 |
| Hub 单模板 + Agent 由面板生成（v0.2.9 起 Agent 不再随仓库提供模板） | compose 对**未激活 profile 的服务也做变量插值**（实测）：旧单文件里 Agent 的 `${DD_BIND_IP:?}` 让「只起 Agent 的机器」（.env 无 DD_BIND_IP）即使 `--profile agent` 也直接报错；拆出独立 Hub 模板后靠 profile 防误起，Agent 进一步改为面板现生成命令，彻底消掉手写模板的维护面 | 占位默认值：把必填校验降级成静默默认值，错误配置能活到运行期 |

未做（后续按需加固）：`read_only` + tmpfs、healthcheck（v0.2.6 起 Agent 无监听
端口，健康检查只能走 `docker inspect` 容器状态或日志）、镜像 digest
钉死（S9.4 产物校验时锁）。

## 文件清单

| 文件 | 说明 |
| --- | --- |
| `docker-compose.yml` | Hub 模板本体（入库，v0.2.6 起只含 Hub + Tunnel） |
| `install-hub.sh` | Hub 一键安装（S10.1，入库） |
| `.env.example` | 环境变量样例（入库）；实用时复制为 `.env`（**不入库**） |
| `README.md` | 本文件 |
| `.env` / `secrets/` | 本地凭据，`.gitignore` 覆盖，**永不入库** |
