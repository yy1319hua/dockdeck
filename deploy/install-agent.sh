#!/usr/bin/env bash
# ============================================================================
# DockDeck Agent 安装脚本（S10.1）
#
# 把 deploy/README.md 的手工四步收敛成一条命令，可重复执行：
#   预检 → 解析绑定 IP → 读已有 .env → 登录 GHCR → 拉镜像
#   → 落 .env / token / compose → up -d → 探活 → 查纳管日志
#
# 用法：
#   sudo ./install-agent.sh --hub-url https://100.64.0.1:8443 --server-name node2
#   sudo ./install-agent.sh                    # 全交互
#   sudo ./install-agent.sh --check            # 只渲染 compose 自检
#
# 幂等：已有 .env 不覆盖（重复执行 = 拉 .env 里那个 tag + up -d）；--force 才重写。
# 凭据：join_secret 走 read -s，只写 600 的 secrets/agent.token 与 .env，
#       不进命令行；token 文件内容 docker inspect 看不到。
# ============================================================================
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly AGENT_PORT=9527
readonly DEFAULT_DIR=/opt/dockdeck
readonly DEFAULT_GHCR_USER=yy1319hua
readonly IMAGE_REPO=ghcr.io/yy1319hua/dockdeck-agent
readonly CONTAINER_NAME=dockdeck-agent

BIND_IP=""
HUB_URL=""
SERVER_NAME=""
JOIN_SECRET=""
TAG="latest"
INSTALL_DIR="$DEFAULT_DIR"
PULL_USER="$DEFAULT_GHCR_USER"
PULL_TOKEN_FILE=""
DO_LOGIN=1
FORCE=0
CHECK_ONLY=0

info() { printf '[agent] %s\n' "$*"; }
warn() { printf '[agent] ⚠️  %s\n' "$*" >&2; }
die() { printf '[agent] ❌ %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
DockDeck Agent 安装脚本

用法：install-agent.sh [选项]

  --hub-url <URL>         Hub 地址，含端口，如 https://100.64.0.1:8443
  --server-name <NAME>    在 Hub 上显示的机器名，默认本机短主机名
  --join-secret <SECRET>  Hub「添加服务器」生成的一次性纳管密钥（30 分钟有效）
  --bind-ip <IP>          本机监听的具体 IP（默认取 tailscale ip -4 的第一条）
  --tag <TAG>             镜像 tag，默认 latest（已有 .env 时以 .env 为准）
  --dir <PATH>            安装目录，默认 /opt/dockdeck
  --pull-user <USER>      GHCR 用户名（GitHub 账号），默认 yy1319hua
  --pull-token-file <P>   从文件读 GHCR token（无人值守）；缺省交互式输入
  --skip-login            匿名拉取失败时也不尝试登录（公开镜像用不到）
  --check                 只渲染 compose 自检，不登录、不拉取、不启动
  --force                 重写已存在的 .env
  -h, --help              显示本帮助

退出码：0 成功；非 0 预检/登录/拉取/启动/探活失败。
EOF
}

while (( $# > 0 )); do
    case "$1" in
        -h|--help|--skip-login|--check|--force) ;;
        --*) (( $# >= 2 )) || die "$1 缺少参数" ;;
    esac
    case "$1" in
        --hub-url) HUB_URL="$2"; shift 2 ;;
        --server-name) SERVER_NAME="$2"; shift 2 ;;
        --join-secret) JOIN_SECRET="$2"; shift 2 ;;
        --bind-ip) BIND_IP="$2"; shift 2 ;;
        --tag) TAG="$2"; shift 2 ;;
        --dir) INSTALL_DIR="$2"; shift 2 ;;
        --pull-user) PULL_USER="$2"; shift 2 ;;
        --pull-token-file) PULL_TOKEN_FILE="$2"; shift 2 ;;
        --skip-login) DO_LOGIN=0; shift ;;
        --check) CHECK_ONLY=1; shift ;;
        --force) FORCE=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) usage >&2; die "未知参数：$1" ;;
    esac
done

# ---------------------------------------------------------------- 预检
require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

require_cmd docker
docker compose version >/dev/null 2>&1 \
    || die "需要 Docker Compose v2（docker compose 子命令）；旧版 docker-compose 不支持本模板"

[[ -n "$TAG" ]] || die "--tag 不能为空"
[[ -n "$INSTALL_DIR" ]] || die "--dir 不能为空"
[[ -n "$PULL_USER" ]] || die "--pull-user 不能为空"

# ---------------------------------------------------------------- 绑定 IP
is_ipv4() {
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]
}

detect_bind_ip() {
    command -v tailscale >/dev/null 2>&1 || return 1
    local ip
    ip="$(tailscale ip -4 2>/dev/null | head -n 1 || true)"
    [[ -n "$ip" ]] || return 1
    printf '%s' "$ip"
}

ip_is_local() {
    command -v ip >/dev/null 2>&1 || return 0
    ip -4 -o addr show 2>/dev/null | grep -qw -- "$1"
}

if [[ -z "$BIND_IP" ]]; then
    BIND_IP="$(detect_bind_ip || true)"
    if [[ -z "$BIND_IP" ]]; then
        die "无法自动获取 tailscale IP（tailscale 未安装或未登录），请用 --bind-ip 显式指定"
    fi
    info "未指定 --bind-ip，自动取 tailscale IP：$BIND_IP"
fi

is_ipv4 "$BIND_IP" || die "--bind-ip 需要 IPv4 具体地址，得到：$BIND_IP"
[[ "$BIND_IP" == "0.0.0.0" ]] && die "拒绝绑定 0.0.0.0：只允许 tailnet / 内网 / 回环的具体 IP"

if ! ip_is_local "$BIND_IP"; then
    if (( CHECK_ONLY )); then
        warn "$BIND_IP 不属于本机网卡（自检模式继续）"
    else
        die "$BIND_IP 不属于本机任何网卡，Agent 会绑定失败。请确认地址或换 --bind-ip"
    fi
fi

# ---------------------------------------------------------------- Hub 地址
normalize_hub_url() {
    local u="$1"
    u="${u%/}"
    [[ "$u" == http://* || "$u" == https://* ]] \
        || die "--hub-url 必须以 http:// 或 https:// 开头，如 https://100.64.0.1:8443"
    local hostpart="${u#*://}"
    if [[ "$hostpart" != *:* ]]; then
        u="${u}:8443"
        warn "--hub-url 未带端口，按 Hub 默认端口处理：$u（反代场景请显式写 :443）"
    fi
    printf '%s' "$u"
}

if [[ -z "$SERVER_NAME" ]]; then
    SERVER_NAME="$(hostname -s 2>/dev/null || hostname)"
fi
[[ -n "$SERVER_NAME" ]] || die "--server-name 不能为空"
[[ "$SERVER_NAME" =~ ^[A-Za-z0-9._-]+$ ]] \
    || die "--server-name 只允许字母数字与 . _ -，得到：$SERVER_NAME"

# ---------------------------------------------------------------- 已有 .env 优先
ENV_FILE="$INSTALL_DIR/.env"
env_get() {
    local key="$1" file="$2" line k v
    [[ -r "$file" ]] || return 1
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line%$'\r'}"
        [[ "$line" == \#* ]] && continue
        [[ "$line" == *=* ]] || continue
        k="${line%%=*}"
        v="${line#*=}"
        k="${k//[[:space:]]/}"
        if [[ "$k" == "$key" ]]; then
            printf '%s' "${v%\"}"
            return 0
        fi
    done < "$file"
    return 1
}

KEEP_ENV=0
if [[ -f "$ENV_FILE" ]] && (( ! FORCE )); then
    KEEP_ENV=1
    existing="$(env_get DD_BIND_IP "$ENV_FILE" || true)"
    if [[ -n "$existing" && "$existing" != "$BIND_IP" ]]; then
        warn "已有 .env 的 DD_BIND_IP=$existing 与本次 $BIND_IP 不同；以 .env 为准"
        BIND_IP="$existing"
    fi
    existing="$(env_get DD_HUB_URL "$ENV_FILE" || true)"
    if [[ -n "$existing" ]]; then
        if [[ -n "$HUB_URL" && "$existing" != "$HUB_URL" ]]; then
            warn "已有 .env 的 DD_HUB_URL=$existing 与本次 $HUB_URL 不同；以 .env 为准"
        fi
        HUB_URL="$existing"
    fi
    existing="$(env_get DD_AGENT_JOIN_SECRET "$ENV_FILE" || true)"
    if [[ -n "$existing" ]]; then
        if [[ -n "$JOIN_SECRET" && "$existing" != "$JOIN_SECRET" ]]; then
            warn "已有 .env 里已存纳管密钥，与本次传入的不同；以 .env 为准（要换请加 --force）"
        fi
        JOIN_SECRET="$existing"
    fi
    existing="$(env_get DD_AGENT_IMAGE "$ENV_FILE" || true)"
    if [[ -n "$existing" ]]; then
        IMAGE="$existing"
        info "沿用 .env 里的镜像：$IMAGE"
    fi
    info "保留已有 .env（要重写请加 --force）"
fi
[[ -n "${IMAGE:-}" ]] || IMAGE="${IMAGE_REPO}:${TAG}"

if [[ -z "$HUB_URL" ]] && (( ! CHECK_ONLY )); then
    [[ -t 0 ]] || die "当前不是交互终端；请用 --hub-url 指定 Hub 地址"
    printf 'Hub 地址（如 https://100.64.0.1:8443）：' >&2
    read -r HUB_URL || true
fi
[[ -n "$HUB_URL" ]] && HUB_URL="$(normalize_hub_url "$HUB_URL")"
if [[ -z "$HUB_URL" ]] && (( ! CHECK_ONLY )); then
    die "--hub-url 不能为空（自注册必须知道 Hub 在哪）"
fi

if [[ -z "$JOIN_SECRET" ]] && (( ! CHECK_ONLY )); then
    [[ -t 0 ]] || die "当前不是交互终端；请用 --join-secret 指定纳管密钥"
    printf '纳管密钥 join_secret（输入不回显，Hub「添加服务器」生成，30 分钟内有效）：' >&2
    read -rs JOIN_SECRET || true
    printf '\n' >&2
fi
if (( ! CHECK_ONLY )); then
    [[ -n "$JOIN_SECRET" ]] || die "纳管密钥不能为空"
    [[ "$JOIN_SECRET" =~ [[:space:]] ]] && die "纳管密钥含空白字符，请重新复制（可能多带了换行）"
fi

# ---------------------------------------------------------------- 自检模式
if (( CHECK_ONLY )); then
    info "自检模式：只渲染 compose，不登录、不拉取、不启动"
    DD_BIND_IP="$BIND_IP" \
    DD_HUB_URL="${HUB_URL:-https://127.0.0.1:8443}" \
    DD_AGENT_JOIN_SECRET="${JOIN_SECRET:-selfcheck-placeholder}" \
    DD_AGENT_IMAGE="$IMAGE" \
        docker compose -f "$SCRIPT_DIR/docker-compose.yml" --profile agent config
    info "渲染通过"
    exit 0
fi

# ---------------------------------------------------------------- 守护进程、socket、端口
docker info >/dev/null 2>&1 || die "无法连接 Docker 守护进程（需要 root 或 docker 组权限）"
require_cmd curl

[[ -S /var/run/docker.sock ]] \
    || die "找不到 /var/run/docker.sock；Agent 必须能访问本机 Docker Engine 的 socket"

port_in_use() {
    local port="$1"
    command -v ss >/dev/null 2>&1 || return 1
    [[ -n "$(ss -ltnH "sport = :${port}" 2>/dev/null || true)" ]]
}

if port_in_use "$AGENT_PORT"; then
    die "端口 ${AGENT_PORT} 已被占用；Agent 无法启动。先停掉占用者，或确认已有 Agent 在跑"
fi

# ---------------------------------------------------------------- GHCR 登录
ghcr_login() {
    local token=""
    if [[ -n "$PULL_TOKEN_FILE" ]]; then
        [[ -r "$PULL_TOKEN_FILE" ]] || die "--pull-token-file 不可读：$PULL_TOKEN_FILE"
        token="$(tr -d '\r\n' < "$PULL_TOKEN_FILE")"
    else
        [[ -t 0 ]] || die "当前不是交互终端；请用 --pull-token-file 提供 token，或加 --skip-login"
        printf 'GHCR 用户名（GitHub 账号）[%s]：' "$PULL_USER" >&2
        local input=""
        read -r input || true
        [[ -n "$input" ]] && PULL_USER="$input"
        printf 'GHCR token（输入不回显，需 read:packages）：' >&2
        read -rs token
        printf '\n' >&2
    fi
    [[ -n "$token" ]] || die "GHCR token 为空"

    if ! printf '%s' "$token" | docker login ghcr.io -u "$PULL_USER" --password-stdin >/dev/null; then
        die "docker login ghcr.io 失败。请确认用户名与 PAT，且 PAT 具备 read:packages 权限"
    fi
    info "已登录 ghcr.io（用户 $PULL_USER）"
}

# 2026-10-19 起镜像随公开库发布，匿名即可拉：先直接 pull，只有失败才谈登录。
# 这样部署机不必再持有任何 GHCR 凭据——凭据越少越好。
info "拉取镜像：$IMAGE（公开镜像，匿名拉取）"
if ! docker pull "$IMAGE"; then
    warn "匿名拉取失败：$IMAGE"
    if (( DO_LOGIN )) && { [[ -t 0 ]] || [[ -n "$PULL_TOKEN_FILE" ]]; }; then
        warn "改为登录 ghcr.io 后重试（仅在镜像转回私有、或本地残留失效凭据时才需要）"
        ghcr_login
        if ! docker pull "$IMAGE"; then
            warn "登录后仍然失败；检查网络与镜像 tag 是否已发布"
            exit 1
        fi
    else
        warn "若提示 unauthorized / denied / 401，说明该镜像需要登录："
        warn "  docker login ghcr.io    # 用户名 = GitHub 账号，PAT 需 read:packages"
        warn "  然后重跑本脚本（不要加 --skip-login）"
        exit 1
    fi
fi

# ---------------------------------------------------------------- 落盘
mkdir -p "$INSTALL_DIR" || die "无法创建安装目录：$INSTALL_DIR"

install_compose() {
    local src="$SCRIPT_DIR/docker-compose.yml"
    local dst="$1/docker-compose.yml"
    [[ -f "$src" ]] || die "找不到 compose 模板：$src"
    [[ "$dst" -ef "$src" ]] && return 0
    if [[ -f "$dst" ]] && ! cmp -s "$src" "$dst"; then
        local bak="${dst}.bak.$(date +%Y%m%d%H%M%S)"
        cp -p "$dst" "$bak"
        warn "已有 compose 与本脚本自带版本不同，旧文件已备份：$bak"
    fi
    install -m 644 "$src" "$dst"
}

install_compose "$INSTALL_DIR"

if (( KEEP_ENV )); then
    :
else
    if [[ -f "$ENV_FILE" ]]; then
        cp -p "$ENV_FILE" "${ENV_FILE}.bak.$(date +%Y%m%d%H%M%S)"
        info "--force：旧 .env 已备份"
    fi
    (
        umask 077
        cat > "$ENV_FILE" <<EOF
# 由 deploy/install-agent.sh 生成于 $(date -u '+%Y-%m-%dT%H:%M:%SZ')
# 手工改动会被保留：重跑脚本不会覆盖本文件，除非加 --force。
DD_BIND_IP=${BIND_IP}
DD_HUB_URL=${HUB_URL}
DD_AGENT_JOIN_SECRET=${JOIN_SECRET}
DD_AGENT_IMAGE=${IMAGE}
EOF
    )
    info "已写入 $ENV_FILE"
fi
chmod 600 "$ENV_FILE"

# 令牌文件：compose 用 create_host_path: false 绑定挂载，缺文件会直接启动失败。
# 内容不加换行（应用按原样比对），权限 600。
SECRET_DIR="$INSTALL_DIR/secrets"
TOKEN_FILE="$SECRET_DIR/agent.token"
mkdir -p "$SECRET_DIR"
chmod 700 "$SECRET_DIR"
(
    umask 077
    printf '%s' "$JOIN_SECRET" > "$TOKEN_FILE"
)
chmod 600 "$TOKEN_FILE"
info "已写入令牌文件 $TOKEN_FILE"

# ---------------------------------------------------------------- 启动
info "启动 Agent（--profile agent）"
docker compose --project-directory "$INSTALL_DIR" -f "$INSTALL_DIR/docker-compose.yml" \
    --profile agent up -d

# ---------------------------------------------------------------- 探活
# Agent 没有免鉴权健康端点：所有路由都在 /api/v1 后面。无 token 请求 /api/v1/info
# 得到 401，正好证明「端口已监听 + TLS 已起 + 鉴权生效」，且不把 token 放上命令行。
URL="https://${BIND_IP}:${AGENT_PORT}/api/v1/info"

wait_ready() {
    local deadline=$(( SECONDS + 60 )) code=""
    while (( SECONDS < deadline )); do
        code="$(curl -ksS -o /dev/null -w '%{http_code}' --max-time 3 "$URL" 2>/dev/null || true)"
        [[ "$code" == "401" ]] && return 0
        sleep 2
    done
    printf '%s' "$code"
    return 1
}

info "探活：$URL（期望 401）"
if ! last_code="$(wait_ready)"; then
    warn "60 秒内未就绪（最后状态码：${last_code:-无响应}）"
    warn "容器已保留，排查：docker logs --tail 100 ${CONTAINER_NAME}"
    exit 1
fi

# ---------------------------------------------------------------- 纳管结果
# 自注册是后台 best-effort（失败只告警不阻断），所以「进程起来了」不等于「纳管成功」。
# 这里读启动日志确认，失败只降级为告警——不影响 Agent 继续提供服务。
enroll_state="unknown"
deadline=$(( SECONDS + 20 ))
while (( SECONDS < deadline )); do
    logs="$(docker logs "$CONTAINER_NAME" 2>&1 || true)"
    if grep -q '已纳管为 Hub 服务器' <<<"$logs"; then
        enroll_state="ok"
        break
    fi
    if grep -q '纳管自注册失败' <<<"$logs"; then
        enroll_state="failed"
        break
    fi
    sleep 2
done

# ---------------------------------------------------------------- 汇报
printf '\n✅ Agent 已就绪\n\n'
printf '  监听地址   https://%s:%s/\n' "$BIND_IP" "$AGENT_PORT"
printf '  机器名     %s\n' "$SERVER_NAME"
printf '  Hub        %s\n' "$HUB_URL"
printf '  安装目录   %s\n\n' "$INSTALL_DIR"

case "$enroll_state" in
    ok)
        printf '  纳管状态   已自注册，Hub 服务器列表应能看到「%s」\n\n' "$SERVER_NAME"
        ;;
    failed)
        warn "纳管自注册失败（Agent 仍在正常提供服务）。常见原因："
        warn "  1) join_secret 已过期（30 分钟）——回 Hub 重新「添加服务器」生成，再重跑本脚本"
        warn "  2) Hub 地址不可达——先试 curl -k ${HUB_URL}/api/v1/health"
        warn "  查看具体错误：docker logs --tail 50 ${CONTAINER_NAME}"
        printf '\n'
        ;;
    *)
        printf '  纳管状态   20 秒内未见自注册日志，请到 Hub 服务器列表确认；\n'
        printf '             未出现则看 docker logs --tail 50 %s\n\n' "$CONTAINER_NAME"
        ;;
esac
