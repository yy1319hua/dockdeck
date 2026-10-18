# DockDeck 安全模型（SECURITY）

> 部署走查见 `docs/DEPLOY.md`；排障见 `docs/TROUBLESHOOT.md`。

## 1. 信任边界

- **信任域内**：Tailnet（如 Tailscale）或内网内、能访问 Hub/Agent 绑定 IP 的调用方。
- **信任域外**：公网直接暴露不在支持范围。所有「谁都能访问」的端点都依赖网络层隔离把守。
- Hub 持有全量数据（账号、Token、审计、被管主机连接）；Agent 持有本机 Docker 控制权。因此 Agent 只接受来自 Hub 的调用。

## 2. 鉴权模式概览

路由按 `AuthMode` 三档：

| 模式 | 含义 | 失败 |
|------|------|------|
| `AuthNone` | 公开，无需身份 | — |
| `AuthSession` | Cookie 会话（`RequireAuth`，注入 UserPrincipal）| 401 |
| `AuthScope` | API Token，按 scope 放行 | 401 / 403 |

## 3. 免认证端点（重要，有意设计）

以下端点**不要求身份**即可访问，因为它们只暴露非敏感的元信息或服务发现，安全依赖**网络层隔离（Tailnet/内网）**：

- `GET /api/v1/health` —— 健康检查，恒 200（仅证明进程与 TLS 已起）。
- `GET /api/openapi.json` —— 完整 API 面的 OpenAPI 描述（**整个 API 契约可读**）。
- `GET /api/v1/scopes` —— API Token 的 scope 字典。
- `POST /api/v1/auth/login` —— 登录入口。

> ⚠️ **设计说明（GAP-4）**：`/api/openapi.json` 与 `/api/v1/scopes` 为 `AuthNone` 是**有意设计，不是遗漏**。它们不含任何凭据或业务数据，暴露完整 API 描述反而便于合法客户端自举。安全不来自「隐藏接口」，而来自「调用需鉴权的接口必须带有效会话/Token」+「网络层只允许信任域内访问」。**切勿将 Hub/Agent 直接绑定到公网 `0.0.0.0` 且不隔离**。

## 4. Hub 会话鉴权

- 登录成功后下发 Cookie 会话；`RequireAuth` 中间件校验，失败返回 401。
- 密码修改、登出、受保护资源均走 `AuthSession`。
- 会话与账号体系由 Hub 独占管理，Agent 不参与账号逻辑。

## 5. API Token（scope 模型）

- Hub 签发 API Token，能力由签发时勾选的 **scope** 决定。
- scope 字典：`GET /api/v1/scopes`（公开）。
- 调用方持 Token 访问受 `AuthScope` 保护的接口，按 scope 放行；缺 scope 返回 403。

## 6. Hub ↔ Agent 通信（HMAC 双重校验）

Agent 只做「被 Hub 调用」一件事，所有请求强制 **Bearer + HMAC 双重校验**：

- **共享密钥**：Agent 的 Token 是 Hub→Agent 的**共享密钥明文**，同时用于 Bearer 校验与 HMAC 签名（见 `internal/config`）。
- **每个请求都签名**：头含 `Authorization: Bearer <token>`、`X-Timestamp`、以及 `X-Signature`（HMAC-SHA256 的十六进制小写结果）。
- **签名串口径**：`timestamp` / `method` / `URI` / `body`（查询串与请求体参与）。算法与 canonical 格式见 `internal/agenthttp` / `internal/agentcli` / `internal/crypto`。
- **服务端校验顺序**：先挡无凭证请求 → 再挡过期时间戳（防重放窗口）→ 最后做相对昂贵的 HMAC 计算。
- 该 Token 走 `--token-file` 文件挂载（600），不进环境变量、不进命令行（`docker inspect` 不可见）。

## 7. 传输安全（TLS）

- Hub 默认 `self`（自签证书，指纹见设置页）。
- `none` = 明文，仅允许**回环**监听，用于前面有反向代理终结 TLS 的场景。
- Agent 默认不对外暴露 API，仅接受来自 Hub 的 HMAC 校验请求。

## 8. 网络暴露模型

- 服务 `network_mode: host`，应用层只绑**具体 IP**（`DD_BIND_IP`），`0.0.0.0` 被 `config.Validate` 拒绝。
- 端口绑定由 `--listen ${DD_BIND_IP}:<端口>` 完成，无 `ports:` 发布（host 模式下 `ports` 被忽略）。
- **暴露面靠 Tailnet/内网隔离把守**，不在应用层做公网防护。

## 9. 凭据与密钥管理

| 凭据 | 形态 | 管理 |
|------|------|------|
| GHCR 拉镜像 PAT | `read:packages` GitHub PAT | 部署机 `docker login ghcr.io`；只给最小 scope（纯读包），碰不到私有代码 |
| Agent Token | Hub→Agent 共享密钥 | `secrets/agent.token`（600），文件挂载，不进环境变量 |
| Hub 数据主密钥 / 自签证书 | 命名卷内 | 随 Hub 数据卷备份（`backup.sh`）|

- `.env` 与 `secrets/` 在 `.gitignore` 覆盖，**永不入库**。
- 开发侧：`pre-commit` 凭据扫描（`credscan-patterns.txt`）拦截疑似密钥入库；工作区持证闸门（`.CANONICAL_WORKSPACE`）防止在非正牌副本提交。

## 10. 容器隔离

- Agent 以 `root` 跑（socket 属主 `root:docker`，非 root 摸不到）；边界 = socket **`:ro`** 只读 + 应用侧自我保护名单（不在自身容器上执行破坏性操作）。
- Hub 以非 root（`uid 10000`）运行，**不挂** docker.sock。
- `container_name` 固定，Agent 的 `--self-container` 与之联动，「不动自己」跨重建稳定。

## 11. 部署安全清单

- [ ] Hub/Agent 只绑 Tailnet/内网具体 IP，绝不 `0.0.0.0` 公网暴露。
- [ ] GHCR PAT 仅 `read:packages`，定期轮换。
- [ ] `secrets/agent.token` 权限 600，且 `docker inspect` 不可见。
- [ ] `.env` / `secrets/` 已确认在 `.gitignore` 内。
- [ ] 部署后跑 `verify.sh` 确认冒烟 + 存量容器不变性。

## 12. 参见

- `docs/DEPLOY.md` —— 部署走查
- `docs/TROUBLESHOOT.md` —— 排障
- `docs/API.md` / `docs/openapi.json` —— API 面
