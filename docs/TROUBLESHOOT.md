# DockDeck 排障手册（TROUBLESHOOT）

> 部署/运行期常见问题的定位与处理。部署流程见 `docs/DEPLOY.md`；安全模型见 `docs/SECURITY.md`。

## 1. 部署前自检清单

- [ ] 已装 Docker Engine + Compose v2（`docker compose version` 有输出）
- [ ] `docker` 能连 socket（`docker ps` 不报权限错）
- [ ] 已确定绑定 IP（`tailscale ip -4` 或 `ip -4 addr show eth0`）
- [ ] 已备 GHCR PAT（`read:packages`）
- [ ] 在 `deploy/` 目录下操作

## 2. 预检阶段失败

| 现象 | 原因 | 处理 |
|------|------|------|
| `缺少命令：docker` | 未装 Docker | 安装 Docker Engine |
| `需要 Docker Compose v2` | 旧版 `docker-compose` | 升级到 Compose v2（plugin）|
| 端口被占用 | 8443/9527 已被占 | `ss -ltnp \| grep -E '8443\|9527'` 查占用进程并让出 |
| socket 摸不到 | 当前用户不在 docker 组 / 无 root | 用 `sudo` 跑安装脚本 |

## 3. GHCR 拉取 401 / 镜像拉不到

- **症状**：`pull` 步骤报 401 / `manifest unknown`。
- **根因**：
  1. 未登录或登录 token 无效 → `docker login ghcr.io`，用户名 = GitHub 账号，密码 = PAT。
  2. PAT **缺少 `read:packages`** 权限 → 到 GitHub 重新生成带 `read:packages` 的 PAT。
  3. 镜像尚未发布（release job 未跑）→ 确认是否打了 `v*` tag 或跑了手动发布；私有库镜像默认不存在于公开查询。
- **GHCR 握手提示**：`GET /v2/` 返回 401 是**协议第一步**（挑战），不代表失败；需按 `WWW-Authenticate` 用 basic 换 bearer 再拉。

## 4. `.env` 报错 / 配置错误

| 现象 | 处理 |
|------|------|
| `DD_BIND_IP: 必填` | 编辑 `.env` 填 `DD_BIND_IP`（tailscale/内网具体 IP）；留空 compose 直接 fail-fast |
| 绑定 `0.0.0.0` 被拒 | `0.0.0.0` 被应用层 `config.Validate` 拒绝；改用具体 IP |
| `none` TLS 下仍想对外 | `none` 仅允许回环；对外需 `self` 或前面加反代 |

## 5. Agent 令牌问题

| 现象 | 处理 |
|------|------|
| `secrets/agent.token` 缺失导致启动失败 | `install -m 600 /dev/null secrets/agent.token` 后写入 Hub 侧生成的令牌 |
| 权限不对 | 确保 600：`chmod 600 secrets/agent.token` |
| 自注册失败 | 检查 `DD_HUB_URL`/`DD_AGENT_JOIN_SECRET` 是否填对；join secret 30 分钟有效，过期重生成 |
| `docker inspect` 看得到 token | 误用了 `DD_AGENT_TOKEN` 环境变量；改用 `--token-file` 文件挂载 |

## 6. socket 挂载失败

- compose 中 `create_host_path: false`：宿主没有 `/var/run/docker.sock` 就**当场失败**（不会悄悄建空目录顶替）。
- 处理：确认 Docker daemon 已起、`/var/run/docker.sock` 存在；Agent 机必须装并运行 Docker。

## 7. 探活不通过

| 角色 | 期望 | 不通过排查 |
|------|------|-----------|
| Hub `/api/v1/health` | 200 | 容器没起来 → `docker compose --profile hub logs`；TLS/端口绑错 → 查 `.env` 的 `DD_BIND_IP` |
| Agent `/api/v1/info` | **401** | 返回 200 反而异常（说明没鉴权）；返回连接拒绝 → Agent 容器没起 / 端口错 |

> 探活口径刻意不同：Agent 无免鉴权健康端点，故用 401 证伪「端口 + TLS + 鉴权」三件事。

## 8. 用 verify.sh 验证

部署后跑 `sudo ./verify.sh`：

- 全绿（退出 0）= 冒烟 + 存量容器不变性都过。
- 命中「本次不校验不变性」告警 = 首次无基线，先跑 `--init-baseline` 锁基准再验。
- 存量容器被改动 = 检查 `--protect` 是否漏列必保容器，或确有非 `dockdeck-` 容器被停。

## 9. 升级 / 回滚

- 升级：改 `.env` tag → `docker compose --profile <角色> pull` → `up -d`。
- 回滚：把 `.env` tag 改回旧版本 → `pull` → `up -d`（命名卷数据保留）。
- 如需回退数据：`deploy/restore.sh`（须先有 `backup.sh` 产物）。

## 10. 日志与状态

```bash
docker compose --profile hub   logs -f
docker compose --profile agent logs -f
docker compose --profile hub   ps
docker compose config            # 渲染自检，须带 --profile
```

## 11. 卸载 / 重建

```bash
docker compose --profile hub down     # 停 Hub（不删卷，数据保留）
docker compose --profile agent down   # 停 Agent
# 彻底清数据（危险）：docker volume rm dockdeck-hub-data
```

重建：重跑 `install-hub.sh` / `install-agent.sh`（已有 `.env` 不覆盖，除非 `--force`）。

## 12. 参见

- `docs/DEPLOY.md`、`docs/SECURITY.md`
- `deploy/README.md`、`deploy/verify.sh`
