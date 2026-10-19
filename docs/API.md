# DockDeck Hub API 使用指南

面向 App / 脚本 / 自动化的调用方。讲清四件事：**怎么拿钥匙、钥匙能开哪扇门、门怎么开、哪里容易踩空**。

**契约本体是 spec，不是本文**：`docs/openapi.json`（与 `docs/openapi.yaml` 同内容）由
`cmd/hub --dump-openapi <dir>` 复用运行时同一份路由登记表生成，运行时也经 `GET /api/openapi.json`
提供。端点全量、参数、响应码一律以 spec 为准；本文与 spec 冲突时以 spec 为准（那也是 bug，请告知维护者）。
生成物**勿手改**——改注解后重新生成，有红测盯着产物与生成器是否一致。

---

## 1. 访问方式与暴露面

Hub 进程本身只绑 tailnet / 内网的具体 IP，**不监听 `0.0.0.0`**；公网入口由 **Cloudflare Tunnel**
承担——`cloudflared` 主动出站连 Cloudflare，**不开放任何入站端口**。两种入口：

| 入口 | 基址 | 适用场景 |
| --- | --- | --- |
| 公网（生产） | `https://hub.example.com` | App、外部脚本、第三方集成 |
| tailnet 直连 | `https://<hub-tailnet-ip>:8443` | 内网运维；CF 故障/限流时的备用入口 |

```bash
HUB="${HUB:-https://hub.example.com}"
```

**TLS 说明**：自签证书只存在于 `cloudflared → Hub` 这一段（Tunnel 侧已开 `No TLS Verify`）；
公网侧是 Cloudflare 的受信证书，**App 不需要信任任何自签证书**。只有走 tailnet 直连时才需要
人工比对指纹（设置页 `GET /settings/tls-fingerprint`）。

### 1.1 生成客户端 SDK

核心只读端点（servers / containers / images / compose / files / metrics / audit / scopes）的
200 响应都指向**具体 schema**（`ServerList`、`ContainerList`、`MetricsEnvelope` …），不是泛型
`object`，可以直接生成带类型的客户端：

```bash
openapi-generator-cli generate \
  -i https://hub.example.com/api/openapi.json \
  -g typescript-fetch     # 或 swift5 / kotlin / dart
  -o ./sdk
```

生成后只需配两样：**base URL** 与 **Bearer 拦截器**（`Authorization: Bearer ddk_...`）。
建议按最小权限签发 Token：只读 App 只需 `read:metrics` + `read:containers`（见 §5、§6）。

---

## 2. 两套身份（并存，互不信任）

| 身份 | 凭据 | 用于 | 反面 |
| --- | --- | --- | --- |
| 浏览器会话 | `dd_session` cookie：HttpOnly、SameSite=Strict、TLS 下 Secure；空闲 8h / 绝对 72h | UI、人手动操作 | 命令行要用 cookie jar 维护；`SameSite=Strict` 也意味着跨站前端拿不到它 |
| API Token | `Authorization: Bearer ddk_...` | App、脚本、自动化 | 认证与 Token 管理、设置页、备份端点一律调不通 |

三条硬规则：

1. **Bearer 优先**：同时带 cookie 和 Bearer 时只认 Bearer，不做身份合并——一枚只读 Token
   不能借浏览器会话的高权限越权。
2. 带 Bearer 去调「仅会话」端点 → `401 unauthenticated`（Hub 只读 cookie，看不见 Bearer）。
3. **会话用户不受 scope 限制**（登录即全权限）；scope 只约束 Token。

仅会话端点（Token 一律 401）：

```
POST   /api/v1/auth/logout
POST   /api/v1/auth/password
GET    /api/v1/api-tokens
POST   /api/v1/api-tokens
DELETE /api/v1/api-tokens/{id}
GET    /api/v1/settings/protected-containers
PUT    /api/v1/settings/protected-containers
GET    /api/v1/settings/tls-fingerprint
GET    /api/v1/admin/backup
POST   /api/v1/servers/invite
```

理由是一致的：**给 Token 这些权限等于让泄漏的 Token 自行续命**——自签新 Token、改掉保护名单、
把整库快照拖走。要自动化这些动作，只能拿浏览器会话去做。

公开端点（无凭据）：`GET /api/v1/health`、`GET /api/v1/scopes`、`GET /api/openapi.json`、
`GET /api/v1/auth/me`、`POST /api/v1/auth/login`、`POST /api/v1/auth/setup`。
`/auth/me` 未登录也返回 200，用 `setup_required` 提示前端引导首启建号。

---

## 3. 信封与错误码

成功 `{"data": …}`，失败 `{"error":{"code":"…","message":"…"}}`。**请判 `code`，不要判中文 `message`**：
`code` 是稳定契约，`message` 会随版本改动、只为给人看。

Hub 层错误码（鉴权与业务入口）：

| code | HTTP | 出现时机 |
| --- | --- | --- |
| `unauthenticated` | 401 | 无 cookie 打「仅会话」端点；带 Bearer 打这些端点也是这个 |
| `session_invalid` | 401 | cookie 会话过期/被撤销（改密与登出会撤销会话） |
| `invalid_credentials` | 401 | 用户名或密码错（不区分「用户不存在」与「密码错」，避免枚举账号） |
| `token_invalid` | 401 / 403 | 401=Token 无效、已撤销、已过期；403=Token 的创建者被禁用或已删除 |
| `scope_denied` | 403 | Token 缺少该端点所需 scope（`message` 里直说缺哪个） |
| `account_disabled` | 403 | 账户被禁用 |
| `account_locked` | 429 | 连续失败 5 次，锁 15 分钟 |
| `rate_limited` | 429 | 触发 IP 限流：登录/建号/纳管注册突发 10 次、每 30s 补 1 |
| `already_initialized` | 409 | 已有账户后再调 `/auth/setup` |
| `weak_password` | 400 | 密码短于 8 位 |
| `name_taken` | 409 | Token 名称已被占用 |
| `bad_ttl` | 400 | `ttl` 不在白名单（见 §5） |
| `bad_pattern` | 400 | 保护名单里第 N 条正则编译失败（`message` 指出第几条） |
| `invite_expired` | 410 | 纳管邀请已过期或已用掉 |
| `invalid_join_secret` | 401 | 纳管注册时的 `join_secret` 认不出来 |
| `bad_request` | 400 | 参数/请求体不合法，含非法的 `X-DD-Request-Id` |

转发层错误码（来自 Agent / Engine，Hub 原样透传、不二次造词）：

| code | HTTP | 含义 |
| --- | --- | --- |
| `bad_request` | 400 | 本地预检没过（容器 ID/镜像 ref 字符集、`tail` 形态、动作词不在白名单） |
| `forbidden` | 403 | 服务器已禁用，或命中保护名单（删除容器时） |
| `path_escape` | 400 | 路径逃逸：`..`、软链越界，realpath 后不在根白名单内 |
| `path_denied` | 403 | 路径合法但命中敏感文件黑名单（私钥、凭据目录等） |
| `not_found` | 404 | 服务器/容器/镜像/项目/路径不存在 |
| `conflict` | 409 | 对象存在但状态不允许：运行中未 `force` 删容器、镜像被容器占用、目标已存在或目录非空 |
| `too_large` | 413 | 上传内容超过单文件上限 16 MiB |
| `upstream_unavailable` | 502 | Agent 不可达或超时 |
| `upstream_error` | 502 | Agent 可达但返回异常（含 Engine 流内业务失败，如仓库不存在） |
| `internal` | 500 | Hub 自身故障；**也包括 Agent 令牌解密失败**（宁可 500 也不把「认证失败」伪装成「网络不通」） |

客户端的三分法：**403 是「你不该」，404/409 是「对象状态不对」，502 是「那台机器不行」，500 是「Hub 自己不行」**。
只有 502/500 值得退避重试，403/409 重试多少次都一样。

---

## 4. 幂等键 `X-DD-Request-Id`

- 所有端点都接受这个可选头：1–64 字符，限 `[A-Za-z0-9._-]`。**形态非法直接 400**——这个值要进审计库、
  进响应头、转发给 Agent，放行 CR/LF 等于给响应拆分开门。
- 不带时 Hub 现场生成一个，并在响应头**原样回显**（规范名 `X-DD-Request-Id`；旧名 `X-Request-Id`
  同步回显，入站也认它，仅为兼容老客户端）。
- 写操作会把该键写进审计行 `request_id`，并转发给 Agent；一次操作的 Hub 审计行与 Agent 日志可用它对齐。
- **它保证的是「审计对账」，不是「同一键只执行一次」**：Agent 无状态，写操作不做执行级去重、也不自动重试。
  要重试就自己先 GET 现状再决定（见 §7）。

---

## 5. 创建 API Token

签发 Token 是「仅会话」端点，所以完整流程是：**先建号/登录（拿 cookie）→ 签发 → 立刻存下明文**。

```bash
JAR=$(mktemp)                     # cookie jar

# ① 是否需要首启建号（未登录也 200）
curl -sS "$HUB/api/v1/auth/me" | jq .data          # {"username":"","setup_required":true}

# ② 建号（仅首次可用，成功即登录）或登录
curl -sS -c "$JAR" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<密码，至少 8 位>"}' "$HUB/api/v1/auth/setup"
curl -sS -c "$JAR" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<密码>"}' "$HUB/api/v1/auth/login"

# ③ 签发：给 iPad 上的 App 只读监控 + 容器只读，90 天有效
TOKEN=$(curl -sS -b "$JAR" -H 'Content-Type: application/json' \
  -d '{"name":"app-ipad","scopes":["read:metrics","read:containers"],"ttl":"90d"}' \
  "$HUB/api/v1/api-tokens" | jq -r '.data.plaintext')
echo "$TOKEN" | cut -c1-8         # ddk_xxxx——明文只有这一次，丢了只能重建

# ④ 立刻验证一把（下面所有示例都用它）
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers" | jq

# ⑤ 不用了就撤销（按 ID，不是按名字）
curl -sS -b "$JAR" -X DELETE "$HUB/api/v1/api-tokens/3" | jq    # {"data":{"revoked":3}}
```

签发成功的响应（`201`）：

```json
{
  "data": {
    "token": {
      "id": 3,
      "name": "app-ipad",
      "scopes": ["read:containers", "read:metrics"],
      "created_at": "2026-10-17T06:40:00Z",
      "active": true
    },
    "plaintext": "ddk_…",
    "warning": "明文仅此一次可见，请立即保存；本端点不会再次返回它。"
  }
}
```

字段规范：

| 字段 | 规则 |
| --- | --- |
| `name` | 非空、≤64 字符、**全局唯一**（重名 `409 name_taken`）。建议「用途+载体」，如 `app-ipad`、`backup-script`、`grafana` |
| `scopes` | 至少一项；未知值 400；重复自动去重、返回按字典序。**写不蕴含读**（见 §6） |
| `ttl` | 只认 `7d` / `30d` / `90d` / `1y` / `never`；**省略等价于 `never`（不过期）**；其它值 `400 bad_ttl` |

细节与边界：

- 明文形态是 `ddk_` + 32 字节 base64url；**库里只存 SHA-256**，所以没有「找回 Token」这回事，只有重建。
- `token` 对象永不含明文与哈希；`expires_at` / `last_used_at` / `revoked_at` 字段**省略即未设/未用/未撤销**。
- `last_used_at` 有 60 秒写入节流——它是「最近用过」的粗估，不是精确时间戳。
- `GET /api/v1/api-tokens` 列出全部 Token 的元数据与 `active`（被撤销或已过期即 `false`）。
- `DELETE /api/v1/api-tokens/{id}` 成功返回 `{"data":{"revoked":<id>}}`；**已撤销的再撤返回 404**
  （不是幂等 200），目标不存在同样 404。
- Token 的权限边界来自创建者的身份：创建者被禁用或删除后，Token 立即不可用（`403 token_invalid`）。

---

## 6. scope 说明

`GET /api/v1/scopes` 是**唯一字典**（顺序与中文说明的定义处），App 的授权界面直接读它，别在本地硬编码。
scope 是「端点级开关」，不是「角色」——它不表达「管理员/只读用户」，只表达「这枚 Token 能碰哪些资源」。

**`read:metrics`** —— 读取主机指标与监控历史

```
GET /api/v1/servers
GET /api/v1/servers/{id}/metrics
GET /api/v1/servers/{id}/metrics/overview
```

**`read:containers`** —— 读取容器 / 镜像 / 卷 / 网络 / Compose 清单与日志

```
GET /api/v1/servers/{id}/containers
GET /api/v1/servers/{id}/containers/{cid}
GET /api/v1/servers/{id}/containers/{cid}/logs
GET /api/v1/servers/{id}/images
GET /api/v1/servers/{id}/volumes
GET /api/v1/servers/{id}/networks
GET /api/v1/servers/{id}/compose
GET /api/v1/servers/{id}/compose/{project}/logs
GET /api/v1/servers/{id}/compose/{project}/file
GET /api/v1/servers/{id}/compose/{project}/backups
GET /api/v1/servers/{id}/compose/{project}/backups/content
```

**`write:containers`** —— 启停/删除容器、镜像拉取与清理、Compose 写动作与 YAML 写入

```
POST   /api/v1/servers/{id}/containers/{cid}/{action}
DELETE /api/v1/servers/{id}/containers/{cid}
POST   /api/v1/servers/{id}/images/pull
DELETE /api/v1/servers/{id}/images/{ref}
POST   /api/v1/servers/{id}/images/prune
POST   /api/v1/servers/{id}/compose/{project}/{action}
POST   /api/v1/servers/{id}/compose/{project}/diff
PUT    /api/v1/servers/{id}/compose/{project}/file
POST   /api/v1/servers/{id}/compose/{project}/backups/restore
```

> 删除镜像的路径参数在路由侧写作 `{ref...}`（要容纳 `library/nginx:latest` 里的 `/`），
> 在 spec 与本文里统一写作 `{ref}`（口径见 `docs/ARCHITECTURE.md`「路径模板口径」）。

**`read:files`** —— 读取白名单根目录内的文件列表、元信息与正文

```
GET /api/v1/servers/{id}/files
GET /api/v1/servers/{id}/files/stat
GET /api/v1/servers/{id}/files/content
```

**`write:files`** —— 在白名单根目录内写入、建目录、删除、改名

```
PUT  /api/v1/servers/{id}/files/content
POST /api/v1/servers/{id}/files/{action}     # action ∈ mkdir | delete | rename
```

**`read:audit`** —— 读取审计日志

```
GET /api/v1/audit_logs
```

四条口径（照着用就不会意外扩权）：

1. **写不蕴含读**。要「列出容器并重启其中一个」，`read:containers` 与 `write:containers` 都得勾。
2. **不为新模块加新 scope**。卷、网络、Compose 备份都复用已有的 read/write:containers——
   同族同权限，避免 scope 表无限膨胀。
3. **「会改系统」的一律归 write**，包括 `POST /compose/{project}/diff`（只算不写，但它是写入流程的一段，
   与 PUT 同一把钥匙）与备份恢复（恢复本身就是一次覆盖写）。
4. scope 不足返回 `403 scope_denied`，`message` 直说缺哪个；Token 本身无效则是 `401 token_invalid`——
   客户端应把这两者分开处理（前者提示用户补权限，后者提示重新发 Token）。

---

## 7. 跨端点约定与踩坑清单

**时间参数口径不统一（有意为之，不是疏漏）**

- metrics 的 `from`/`to` 与审计的 `from`/`to` **只认 RFC3339**；
- 容器日志的 `since`/`until` 认 **Unix 秒或 RFC3339**（Hub 出门前预检、Agent 落 Engine 前归一成 Unix 秒，
  因为实测 Engine 只认 Unix 秒，RFC3339 再正确也会 500）；
- 日志时间里的 `+`（如 `2026-10-16T20:00:00+08:00`）**必须转义**：不转义会被查询串解析成空格，
  时区信息静默丢失。curl 用 `--data-urlencode`（配 `-G`，且该请求不能再带请求体）。

**窗口与上限**

| 端点 | 默认 | 上限 |
| --- | --- | --- |
| `metrics/overview` | 最近 1 小时 | 窗口 8 天（超出自动截成最近 8 天）；`limit` 默认 720、上限 1440（超出取最新） |
| `audit_logs` | 最近 7 天 | `limit` 默认 100、上限 1000（超出静默夹到 1000，不报错） |
| 指标保留期 | — | 7 天（更早的曲线查不到了，别把它当长期存储） |

**裸流端点没有信封**：容器日志、Compose 日志、文件正文、整库快照。它们的处理方式不同：

- 失败发生在响应头之前 → 仍是正常信封（404/502 的 JSON），客户端照 §3 解析；
- 一旦开始流式 → 只能用「连接关闭」表达中断，**没有错误码可用**。客户端要能区分「读完了」与「断了」
  （长连接下后者是常态，重连是调用方的决策）。
- 文件正文的元信息在**响应头**：`X-DD-File-Path`（相对路径，不给宿主机绝对路径）、`X-DD-File-Size`、
  `X-DD-Truncated: 1`（正文被单文件上限截断）。看到 `Truncated` 就别把它当完整文件缓存。
- 容器日志 `export=1` 时是 Engine 原始字节流 + `Content-Disposition`，可以直接喂给 `docker` CLI 消费。

**请求体的两套上限**

- 用标准 JSON 解码的端点：上限 1 MiB，且**拒绝未知字段**——把字段名拼错不会静默变成默认值，直接 400。
  这一条能省掉大量「为什么我的参数没生效」。
- 容器动作与删除容器的选项体：上限 4 KB，可省略（省略即用引擎默认）。
- `PUT /files/content` 例外：**请求体就是文件内容本身**（不套信封），上限 16 MiB
  （`protocol.MaxFileUploadBytes`，Hub 与 Agent 共用同一常量，不存在「Hub 放行、Agent 413」）。本模块**无分片**，
  更大的文件走宿主机。

**其它容易踩的**

- 空清单编码为 `[]` 而不是 `null`。唯一例外：`metrics` 实时快照的 `containers` 为 `null` 表示
  「这台机器上还没有容器指标」，与「有指标但列表为空」不是一回事。
- **字段命名不统一，以 spec 为准**：容器清单、镜像清单是 Engine 原生大写（`Id`/`Names`/`Image`/`State`…）；
  受管主机清单也是 Go 字段名大写（`ID`/`Name`/`BaseURL`/`Arch`/`DockVer`/`Enabled`/`LastSeen`/`LastError`）；
  其余是我们自己的 `snake_case`。别指望一个 `camelCase` 映射函数吃遍所有端点。
- 服务器不存在 → 404；**服务器已禁用 → 403**：实时转发（容器/镜像/Compose/文件）对已禁用服务器一律拒绝，
  但历史指标仍可查（禁用拒绝的是「实时」，不是「历史」）。
- **删除容器有两道门**：`confirm` 必须与目标 ID 或名字**完全一致**（缺失或不符 → `400 bad_request`，
  `message` 会告诉你该回传什么），且目标不能命中保护名单（命中 → `403 forbidden`）。保护名单在设置页维护，
  缺省 `^dockdeck-`（即 DockDeck 自身）。`confirm` 是「防误删确认」，不是幂等键。
- `force=1` / `v=1` 这类开关**查询串与请求体取并集**（任一处置位即生效），且只认 `1`；
  `all=` / `dangling=` 只认 `true`/`false`/`1`/`0`，拼错直接 400——清单少给一条远比报错难排查。
- **Compose 没有 prune**：镜像清理走 `images/prune`；Compose 写动作只有 `up`/`down`/`restart`/`pull`。
- 改 Compose YAML **不会直接改动运行中的容器**，要再执行一次 `up` 才生效。
- 写操作不自动重试、不做执行级去重：一次失败即如实回报，重试与否由调用方决定（涉及状态变更时先 GET 现状）。

---

## 8. curl 示例

下面所有示例都假设已按 §5 拿到 `$TOKEN`；读写 scope 已按 §6 勾好。

```bash
# 公开端点：存活探针与 scope 字典
curl -sS "$HUB/api/v1/health"                       # {"data":{"ok":true}}
curl -sS "$HUB/api/v1/scopes" | jq -r '.data[] | "\(.scope)\t\(.description)"'
```

**读：主机、指标、容器**

```bash
# 受管主机清单（字段是大写，见 §7）
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers" \
  | jq -r '.data[] | "\(.ID)\t\(.Name)\t\(.Enabled)\t\(.LastSeen)"'

# 实时指标快照（服务器不存在或从未采集 → 404）
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/metrics" \
  | jq '{ts: .data.ts,
         cpu: .data.metrics.cpu.usage_percent,
         mem: (.data.metrics.memory.used / .data.metrics.memory.total * 100 | round)}'
# 内存给的是字节数（total/used/available…），百分比要自己算——服务端不代人做算术

# 历史曲线：默认最近 1 小时，窗口上限 8 天
curl -sS -G -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'from=2026-10-17T00:00:00Z' \
  --data-urlencode 'to=2026-10-17T06:00:00Z' \
  --data-urlencode 'limit=720' \
  "$HUB/api/v1/servers/1/metrics/overview" | jq '{from: .data.from, points: (.data.points|length)}'

# 容器清单（all=true 含已停止）
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/containers?all=true" \
  | jq -r '.data[] | "\(.Id[0:12])\t\(.Names[0])\t\(.State)\t\(.Image)"'

# 容器详情：Engine inspect JSON 原样透传
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/containers/web-1" | jq '.data.Config.Image'
```

**容器日志（裸流）**

```bash
# 最近 200 行
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/containers/web-1/logs?tail=200"

# 跟随（Ctrl-C 结束；--no-buffer 才能边到边看，否则 curl 会攒缓冲）
curl -sS --no-buffer -H "Authorization: Bearer $TOKEN" \
  "$HUB/api/v1/servers/1/containers/web-1/logs?follow=1&tail=50&timestamps=1"

# 带时区的起点：+ 必须转义（--data-urlencode），否则时区静默丢失
curl -sS --no-buffer -G -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'since=2026-10-16T20:00:00+08:00' \
  --data-urlencode 'follow=1' \
  "$HUB/api/v1/servers/1/containers/web-1/logs"

# 导出下载（octet-stream + Content-Disposition）
curl -sS -H "Authorization: Bearer $TOKEN" \
  "$HUB/api/v1/servers/1/containers/web-1/logs?export=1" -o web-1.log
```

**写：容器操作（需要 `write:containers`）**

```bash
REQ=$(uuidgen)     # 可选：审计对账键；省略则由 Hub 生成并在响应头回显

curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H "X-DD-Request-Id: $REQ" \
  "$HUB/api/v1/servers/1/containers/web-1/restart" | jq

# stop 可给优雅期（SIGTERM 后等待秒数）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"timeout_sec":30}' "$HUB/api/v1/servers/1/containers/web-1/stop" | jq

# resize 走查询串（Engine 原生约定，行列正整数）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  "$HUB/api/v1/servers/1/containers/web-1/resize?h=40&w=120" | jq

# 删除：confirm 必须与 ID/名字完全一致，否则 400；命中保护名单 403
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$HUB/api/v1/servers/1/containers/web-1?confirm=web-1&force=1&v=1" | jq
```

**镜像**

```bash
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/images?all=true" \
  | jq -r '.data[] | "\(.Id[0:19])\t\(.Size)\t\(.RepoTags)"'

# 清理悬空镜像：两段式——先看清单（第一段），确认后才 prune（第二段）
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/images?dangling=true" | jq '.data | length'
curl -sS -X POST -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/images/prune" \
  | jq '{deleted: (.data.ImagesDeleted|length), reclaimed: .data.SpaceReclaimed}'

# 拉取（长操作，Hub 侧超时 10 分钟）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"ref":"alpine:3.20"}' "$HUB/api/v1/servers/1/images/pull" | jq

# 删除（ref 可含 / 与 :；被容器占用 → 409）
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/images/alpine:3.20" | jq
```

**Compose（YAML 两段式：先 diff 再写）**

```bash
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/compose" \
  | jq -r '.data[] | "\(.name)\t\(.status)\t\(.config_file)"'

curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/compose/web/file" \
  | jq -r '.data.content' > compose.draft

# 预览：只算不写、不落盘、不产生备份
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data-binary "$(jq -Rs '{content: .}' < compose.draft)" \
  "$HUB/api/v1/servers/1/compose/web/diff" | jq -r '.data.diff'

# 确认写：Agent 侧先落 <file>.bak.<定宽奈秒时间戳> 再覆盖，响应回传备份路径
curl -sS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data-binary "$(jq -Rs '{content: .}' < compose.draft)" \
  "$HUB/api/v1/servers/1/compose/web/file" | jq '.data.backup_file'

# 让改动生效（改 YAML 不会自动作用于运行中的容器）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/compose/web/up" \
  | jq -r '.data.output'

# 备份：列 / 读 / 恢复（恢复是覆盖写，会再产生一份新备份——回滚不是单向门）
curl -sS -H "Authorization: Bearer $TOKEN" "$HUB/api/v1/servers/1/compose/web/backups" | jq '.data.backups'
curl -sS -G -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'file=docker-compose.yml.bak.2026-10-17T10:00:00.000000001+08:00' \
  "$HUB/api/v1/servers/1/compose/web/backups/content" | jq -r '.data.content'
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"file":"docker-compose.yml.bak.2026-10-17T10:00:00.000000001+08:00"}' \
  "$HUB/api/v1/servers/1/compose/web/backups/restore" | jq
```

**文件（读走 `read:files`，写走 `write:files`）**

```bash
curl -sS -G -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'path=app' --data-urlencode 'depth=1' \
  "$HUB/api/v1/servers/1/files" | jq -r '.data.entries[] | "\(.mode)\t\(.size)\t\(.name)"'

curl -sS -G -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'path=app/config.yaml' "$HUB/api/v1/servers/1/files/stat" | jq '.data'

# 下载正文：元信息在响应头，正文是裸流
curl -sS -D headers.txt -G -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'path=app/config.yaml' \
  "$HUB/api/v1/servers/1/files/content" -o config.yaml
grep -i '^x-dd-file\|^x-dd-truncated' headers.txt

# 写回：请求体就是文件内容；路径直接写在查询串里
# （注意：--data-urlencode 不能和 -G 一起用在这个请求上，-G 会把正文也塞进 URL；
#   路径含空格/中文时要自己编码，例如 jq -rn --arg p 'a b.txt' '$p|@uri'）
curl -sS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/octet-stream' \
  --data-binary @config.yaml \
  "$HUB/api/v1/servers/1/files/content?path=app/config.yaml" | jq '.data.backup_file'

# 写动作：URL 段与请求体 action 必须一致（不一致 400）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"mkdir","path":"app/newdir"}' "$HUB/api/v1/servers/1/files/mkdir" | jq
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"rename","path":"app/a.yml","dest":"app/b.yml"}' \
  "$HUB/api/v1/servers/1/files/rename" | jq
```

**审计（`read:audit`，游标翻页）**

```bash
curl -sS -G -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'action=container' --data-urlencode 'result=error' \
  --data-urlencode 'limit=50' "$HUB/api/v1/audit_logs" \
  | jq '{count: (.data.logs|length), next: .data.next_before, sample: .data.logs[0]}'

# 下一页：把上一页的 next_before 当 before_id；next_before=0 表示到底了
curl -sS -G -H "Authorization: Bearer $TOKEN" --data-urlencode 'before_id=1234' \
  "$HUB/api/v1/audit_logs" | jq '.data.next_before'

# 单条审计行的样子
# {"id":1234,"ts":"2026-10-17T06:40:00Z","user_id":1,"username":"admin","server_id":1,
#  "action":"container.restart","target":"web-1","detail":"","result":"ok","error":"",
#  "ip":"…","request_id":"<你回传的 X-DD-Request-Id>"}
```

**设置页与备份（仅会话）**

```bash
curl -sS -b "$JAR" "$HUB/api/v1/settings/tls-fingerprint" | jq '.data'

# 保护名单整名单覆盖；空数组即清除（回落默认名单 ^dockdeck-）；坏正则 400 bad_pattern
curl -sS -b "$JAR" -X PUT -H 'Content-Type: application/json' \
  -d '{"patterns":["^dockdeck-","^db-[0-9]{2,3}$"]}' \
  "$HUB/api/v1/settings/protected-containers" | jq '.data'

# 整库快照（含全部凭据密文，等同受同等保护的数据库副本，别放临时目录）
curl -sS -b "$JAR" "$HUB/api/v1/admin/backup" -o dockdeck-snapshot.db
```

**纳管新 Agent（仅会话签发邀请 → Agent 自注册）**

```bash
curl -sS -b "$JAR" -X POST -H 'Content-Type: application/json' \
  -d '{"name_hint":"node-4","ttl_min":30}' "$HUB/api/v1/servers/invite" | jq '.data'

# Agent 侧（容器启动时自动做，这里只是等价物）：
curl -sS -X POST -H 'Authorization: Bearer <join_secret>' -H 'Content-Type: application/json' \
  -d '{"name":"node-4","base_url":"http://<agent-host>:9199"}' "$HUB/api/v1/servers/enroll" | jq
```

邀请默认 30 分钟过期、上限 24 小时；`join_secret` 明文只回显一次，**同一个 secret 重复 enroll 是幂等的**
（返回既有 `id` 并刷新 `base_url`）——Agent 重启或改址不需要重新签发邀请。

---

## 9. 客户端示例（TypeScript）

选 TypeScript 的理由：`fetch` 零依赖、浏览器/Node 18+/Deno 通用，且与仓库前端同栈
（浏览器侧的真实客户端见 `web/src/api/client.ts`——**它是 cookie 会话版，不要往里加 Bearer**）。
需要 Python（`requests`）或 Go 版本的话说一声，逻辑是同构的。

```ts
// ddk.ts —— DockDeck Hub 最小客户端
export class ApiError extends Error {
  constructor(readonly status: number, readonly code: string, message: string) {
    super(message)
    this.name = 'ApiError'
  }
  /** 权限不足：提示用户去补 scope，重试无意义 */
  get needScope() { return this.code === 'scope_denied' }
  /** 上游问题：可以退避重试 */
  get retryable() { return this.code === 'upstream_unavailable' || this.code === 'upstream_error' }
}

interface Envelope<T> { data?: T; error?: { code: string; message: string } }

export class Hub {
  constructor(private base: string, private token: string) {}

  private headers(extra: HeadersInit = {}): Headers {
    const h = new Headers(extra)
    h.set('Authorization', `Bearer ${this.token}`)
    h.set('Accept', 'application/json')
    // 幂等/对账键：审计行会记下它，排查时能与服务端对账
    h.set('X-DD-Request-Id', crypto.randomUUID())
    return h
  }

  /** 读一个信封端点。非 2xx 一律抛 ApiError，并保留服务端的 code。 */
  private async call<T>(path: string, init: RequestInit = {}): Promise<T> {
    const res = await fetch(this.base + path, { ...init, headers: this.headers(init.headers) })
    const raw = await res.text()
    let body: Envelope<T> | null = null
    try { body = raw ? (JSON.parse(raw) as Envelope<T>) : null } catch { /* 网关插进来的 HTML 等 */ }
    if (!res.ok) {
      throw new ApiError(res.status, body?.error?.code ?? 'http_error', body?.error?.message ?? res.statusText)
    }
    return body?.data as T
  }

  private post<T>(path: string, payload: unknown): Promise<T> {
    return this.call<T>(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
  }

  listServers(): Promise<ServerRow[]> {
    return this.call<ServerRow[]>('/api/v1/servers')
  }

  latestMetrics(id: number): Promise<{ ts: string; metrics: { cpu: { usage_percent: number } } }> {
    return this.call(`/api/v1/servers/${id}/metrics`)
  }

  /** 容器写操作。写操作不做执行级去重，调用方负责「先看再动」。 */
  containerAction(id: number, cid: string, action: string, body?: unknown): Promise<unknown> {
    return this.call(`/api/v1/servers/${id}/containers/${encodeURIComponent(cid)}/${action}`, {
      method: 'POST',
      ...(body === undefined ? {} : { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }),
    })
  }

  /** 删除容器：confirm 必须是目标 ID 或名字（服务端硬性要求） */
  removeContainer(id: number, cid: string, opts: { force?: boolean; volumes?: boolean } = {}) {
    const q = new URLSearchParams({ confirm: cid })
    if (opts.force) q.set('force', '1')
    if (opts.volumes) q.set('v', '1')
    return this.call(`/api/v1/servers/${id}/containers/${encodeURIComponent(cid)}?${q}`, { method: 'DELETE' })
  }

  /** 文件写入：请求体就是文件内容本身 */
  writeFile(id: number, path: string, content: string) {
    return this.call<{ backup_file?: string; size: number }>(
      `/api/v1/servers/${id}/files/content?path=${encodeURIComponent(path)}`,
      { method: 'PUT', headers: { 'Content-Type': 'application/octet-stream' }, body: content },
    )
  }

  /** 日志是裸流：没有信封，逐行吐给调用方；连接断开即结束（不会抛错误）。 */
  async *logStream(id: number, cid: string, opts: { tail?: string; follow?: boolean } = {}) {
    const q = new URLSearchParams()
    if (opts.follow) q.set('follow', '1')
    if (opts.tail) q.set('tail', opts.tail)
    const res = await fetch(
      `${this.base}/api/v1/servers/${id}/containers/${encodeURIComponent(cid)}/logs?${q}`,
      { headers: this.headers() },
    )
    if (!res.ok) {
      const body = (await res.json().catch(() => null)) as Envelope<never> | null
      throw new ApiError(res.status, body?.error?.code ?? 'http_error', body?.error?.message ?? res.statusText)
    }
    if (!res.body) return
    const reader = res.body.getReader()
    const dec = new TextDecoder()
    let buf = ''
    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      // stream: true —— 逐块解码，避免把多字节字符劈在两个 chunk 之间
      buf += dec.decode(value, { stream: true })
      for (let i = buf.indexOf('\n'); i >= 0; i = buf.indexOf('\n')) {
        yield buf.slice(0, i)
        buf = buf.slice(i + 1)
      }
    }
    if (buf) yield buf
  }
}

/** 受管主机清单：字段是 Go 字段名（大写），映射只做一次，别散落各处 */
interface ServerRow { ID: number; Name: string; Enabled: boolean; LastSeen: string }
```

用法：

```ts
const hub = new Hub(process.env.HUB!, process.env.DDK_TOKEN!)

try {
  for (const s of await hub.listServers()) {
    console.log(`${s.ID}\t${s.Name}\tenabled=${s.Enabled}\tlast_seen=${s.LastSeen || '从未'}`)
  }

  // 日志跟随：Ctrl-C 结束
  for await (const line of hub.logStream(1, 'web-1', { follow: true, tail: '50' })) {
    console.log(line)
  }

  // 破坏性动作：先确认现状，再动手，最后回读
  await hub.containerAction(1, 'web-1', 'restart')
  await hub.writeFile(1, 'app/config.yaml', 'log_level: debug\n')
} catch (e) {
  if (e instanceof ApiError) {
    console.error(`[${e.status} ${e.code}] ${e.message}`)
    if (e.needScope) console.error('这枚 Token 权限不够：去设置页给它补 scope 或换一枚。')
    else if (e.retryable) console.error('上游（Agent/Engine）出问题，稍后重试。')
  } else {
    throw e
  }
}
```

---

## 10. 兼容性口径

- spec 为 `openapi: 3.0.3`（App 侧工具链兼容性最稳）。`info.version` 目前是手工常量；
  接入真实构建版本后由构建设置注入。
- **稳定的**：路径、方法、`code` 取值、scope 名称、信封结构。
- **可能变的**：`message` 文案、`detail` 之类的自由文本字段、spec 的说明文字。
- **向后兼容的增量**：新增端点、新增 scope（旧 Token 不会因此获得新权限）、新增响应字段。
- 客户端请对未知响应字段保持宽容；对未知 `code` 回落到「按 HTTP 状态码处理」而不是崩掉。

有疑问时以两份来源为准：`docs/openapi.json`（全量契约）与 `GET /api/v1/scopes`（scope 字典）。
本文与它们冲突，就是本文该改。
