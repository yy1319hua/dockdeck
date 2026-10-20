# DockDeck 排障手册（TROUBLESHOOT）

> 部署/运行期常见问题的定位与处理。部署流程见 `docs/DEPLOY.md`；安全模型见 `docs/SECURITY.md`。

## 1. 部署前自检清单

- [ ] 已装 Docker Engine + Compose v2（`docker compose version` 有输出）
- [ ] `docker` 能连 socket（`docker ps` 不报权限错）
- [ ] Hub 机：已确定绑定 IP（`tailscale ip -4` 或 `ip -4 addr show eth0`）；Agent 机不需要（v0.2.6 起无监听端口）
- [ ] 能访问 ghcr.io（公开镜像，无需 PAT）
- [ ] 在 `deploy/` 目录下操作

## 2. 预检阶段失败

| 现象 | 原因 | 处理 |
|------|------|------|
| `缺少命令：docker` | 未装 Docker | 安装 Docker Engine |
| `需要 Docker Compose v2` | 旧版 `docker-compose` | 升级到 Compose v2（plugin）|
| 端口被占用 | 8443 已被占（Hub） | `ss -ltnp \| grep 8443` 查占用进程并让出；Agent 自 v0.2.6 起无监听端口，不存在占用 |
| socket 摸不到 | 当前用户不在 docker 组 / 无 root | 用 `sudo` 跑安装脚本 |

## 3. GHCR 拉取失败 / 镜像拉不到

- **症状**：`pull` 步骤报 `denied` / `manifest unknown`。
- **前提**：镜像随公开库发布，**匿名即可拉**，正常情况不需要任何登录。
- **排查顺序**：
  1. tag 不存在（发版失败或 tag 拼错）→ `docker manifest inspect ghcr.io/yy1319hua/dockdeck-hub:<tag>`，404 即未发布。
  2. 机器上残留失效的 ghcr 旧凭据 → `docker logout ghcr.io` 再重试；**过期凭据会得到 `denied`，比匿名更糟**（这是 2026-10-19 实际踩到的坑）。
  3. 网络到 ghcr.io 不通 → 换网络或配代理。
  4. 仅当项目转回私有库时，才需要 `read:packages` 的 PAT 登录。

## 4. `.env` 报错 / 配置错误

| 现象 | 处理 |
|------|------|
| `DD_BIND_IP: 必填` | 编辑 Hub 机 `.env` 填 `DD_BIND_IP`（tailscale/内网具体 IP）；留空 compose 直接 fail-fast。Agent 机用 `docker-compose.agent.yml`，不需要此项 |
| Agent 启动即退出、日志提示缺 Hub 地址 | v0.2.6 起 `DD_HUB_URL` 是硬前提（反向唯一形态）：编辑 `.env` 填入 `https://<hub>:8443` 后重启 |
| 绑定 `0.0.0.0` 被拒 | `0.0.0.0` 被应用层 `config.Validate` 拒绝；改用具体 IP |
| `none` TLS 下仍想对外 | `none` 仅允许回环；对外需 `self` 或前面加反代 |

## 5. Agent 令牌问题

| 现象 | 处理 |
|------|------|
| `secrets/agent.token` 缺失导致启动失败 | `install -m 600 /dev/null secrets/agent.token` 后写入 Hub 侧生成的令牌 |
| 权限不对 | 确保 600：`chmod 600 secrets/agent.token` |
| 自注册失败 / 容器反复重启 | v0.2.6 起纳管是启动期硬前提：Hub 不可达会无限重试（容器在跑但无「已纳管」日志），凭据/邀请错误直接退出（restart 反复拉起）。查 `DD_HUB_URL`/`DD_AGENT_JOIN_SECRET`；join secret 30 分钟有效，过期重生成 |
| `docker inspect` 看得到 token | 误用了 `DD_AGENT_TOKEN` 环境变量；改用 `--token-file` 文件挂载 |

## 6. socket 挂载失败

- compose 中 `create_host_path: false`：宿主没有 `/var/run/docker.sock` 就**当场失败**（不会悄悄建空目录顶替）。
- 处理：确认 Docker daemon 已起、`/var/run/docker.sock` 存在；Agent 机必须装并运行 Docker。

## 7. 探活不通过

| 角色 | 期望 | 不通过排查 |
|------|------|-----------|
| Hub `/api/v1/health` | 200 | 容器没起来 → `docker compose --profile hub logs`；TLS/端口绑错 → 查 `.env` 的 `DD_BIND_IP` |
| Agent 容器状态 + 「已纳管」日志 | running + 日志含「已纳管」 | 容器不在 → `docker logs --tail 100 dockdeck-agent` 看退出原因（多为 Hub 不可达或凭据错）；容器在跑但无「已纳管」→ 还在重试，确认 Hub 地址与网络 |

> v0.2.6 起 Agent 无监听端口，没有可探的 HTTP 端点；「容器在跑 + 已纳管」就是就绪信号（纳管在启动期同步完成）。

## 8. 用 verify.sh 验证

部署后跑 `sudo ./verify.sh`：

- 全绿（退出 0）= 冒烟 + 存量容器不变性都过。
- 命中「本次不校验不变性」告警 = 首次无基线，先跑 `--init-baseline` 锁基准再验。
- 存量容器被改动 = 检查 `--protect` 是否漏列必保容器，或确有非 `dockdeck-` 容器被停。

## 9. 升级 / 回滚

- 升级：改 `.env` tag → `pull` → `up -d`；Agent 用独立模板：
  `docker compose -p dockdeck-agent -f docker-compose.agent.yml pull && docker compose -p dockdeck-agent -f docker-compose.agent.yml up -d`。
- 回滚：把 `.env` tag 改回旧版本 → 同上（命名卷数据保留）。
- 如需回退数据：`deploy/restore.sh`（须先有 `backup.sh` 产物）。

## 10. 日志与状态

```bash
docker compose --profile hub logs -f        # Hub 日志
docker compose -p dockdeck-agent -f docker-compose.agent.yml logs -f   # Agent 日志
docker compose --profile hub ps              # Hub 状态
docker compose -p dockdeck-agent -f docker-compose.agent.yml ps        # Agent 状态
docker compose config                        # 渲染自检，须带 --profile
```

## 11. 卸载 / 重建

```bash
docker compose --profile hub down     # 停 Hub（不删卷，数据保留）
docker compose -p dockdeck-agent -f docker-compose.agent.yml down   # 停 Agent
# 彻底清数据（危险）：docker volume rm dockdeck-hub-data
```

重建：重跑 `install-hub.sh` / `install-agent.sh`（已有 `.env` 不覆盖，除非 `--force`）。

## 12. 参见

- `docs/DEPLOY.md`、`docs/SECURITY.md`
- `deploy/README.md`、`deploy/verify.sh`
