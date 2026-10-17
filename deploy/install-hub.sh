#!/usr/bin/env bash
# ============================================================================
# DockDeck Hub 安装脚本（S10.1）
#
# 把 deploy/README.md 的手工四步收敛成一条命令，可重复执行：
#   预检 → 解析绑定 IP → 读已有 .env → 登录 GHCR → 拉镜像 → 落 .env / compose
#   → up -d → 探活
#
# 用法：
#   sudo ./install-hub.sh                      # 全交互（绑定 IP 自动取 tailscale）
#   sudo ./install-hub.sh --bind-ip 100.64.0.1
#   sudo ./install-hub.sh --check              # 只渲染 compose 自检，不登录/不拉取/不启动
#
# 幂等：已有 .env 不覆盖（重复执行 = 拉 .env 里那个 tag + up -d）；--force 才重写。
# 凭据：GHCR token 走 read -s + docker login --password-stdin，不进命令行、不落盘。
# ============================================================================
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly HUB_PORT=8443
readonly DEFAULT_DIR=/opt/dockdeck
readonly DEFAULT_GHCR_USER=yy1319hua
readonly IMAGE_REPO=ghcr.io/yy1319hua/dockdeck-hub
readonly HEALTH_PATH=/api/v1/health

BIND_IP=""
TAG="latest"
TLS_MODE="self"
INSTALL_DIR="$DEFAULT_DIR"
PULL_USER="$DEFAULT_GHCR_USER"
PULL_TOKEN_FILE=""
DO_LOGIN=1
FORCE=0
CHECK_ONLY=0

info() { printf '[hub] %s\n' "$*"; }
warn() { printf '[hub] ⚠️  %s\n' "$*" >&2; }
die() { printf '[hub] ❌ %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
DockDeck Hub 安装脚本

用法：install-hub.sh [选项]

  --bind-ip <IP>          Hub 监听的具体 IP（默认取 tailscale ip -4 的第一条）
  --tag <TAG>             镜像 tag，默认 latest（已有 .env 时以 .env 为准）
  --tls-mode <self|none>  TLS 模式，默认 self（自签）；none 仅允许回环
  --dir <PATH>            安装目录，默认 /opt/dockdeck
  --pull-user <USER>      GHCR 用户名（GitHub 账号），默认 yy1319hua
  --pull-token-file <P>   从文件读 GHCR token（无人值守）；缺省交互式输入
  --skip-login            跳过 docker login（已登录过或镜像已公开）
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
        --bind-ip) BIND_IP="$2"; shift 2 ;;
        --tag) TAG="$2"; shift 2 ;;
        --tls-mode) TLS_MODE="$2"; shift 2 ;;
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

case "$TLS_MODE" in
    self|none) ;;
    *) die "--tls-mode 只能是 self 或 none，得到：$TLS_MODE" ;;
esac

[[ -n "$TAG" ]] || die "--tag 不能为空"
[[ -n "$INSTALL_DIR" ]] || die "--dir 不能为空"
[[ -n "$PULL_USER" ]] || die "--pull-user 不能为空"

# ---------------------------------------------------------------- 绑定 IP
is_ipv4() {
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]
}

is_loopback() {
    [[ "$1" == 127.* ]]
}

detect_bind_ip() {
    command -v tailscale >/dev/null 2>&1 || return 1
    local ip
    ip="$(tailscale ip -4 2>/dev/null | head -n 1 || true)"
    [[ -n "$ip" ]] || return 1
    printf '%s' "$ip"
}

ip_is_local() {
    command -v ip >/dev/null 2>&1 || return 0   # 无 ip 命令时不阻断
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
[[ "$BIND_IP" == "0.0.0.0" ]] && die "拒绝绑定 0.0.0.0：DockDeck 只允许 tailnet / 内网 / 回环的具体 IP"

if [[ "$TLS_MODE" == "none" ]] && ! is_loopback "$BIND_IP"; then
    die "TLS_MODE=none 仅允许回环监听（应用层强制），当前 --bind-ip=$BIND_IP；对外请用 --tls-mode self"
fi

if ! ip_is_local "$BIND_IP"; then
    if (( CHECK_ONLY )); then
        warn "$BIND_IP 不属于本机网卡（自检模式继续）"
    else
        die "$BIND_IP 不属于本机任何网卡，Hub 会绑定失败。请确认地址或换 --bind-ip"
    fi
fi

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
    existing="$(env_get DD_HUB_TLS_MODE "$ENV_FILE" || true)"
    if [[ -n "$existing" && "$existing" != "$TLS_MODE" ]]; then
        warn "已有 .env 的 DD_HUB_TLS_MODE=$existing 与本次 $TLS_MODE 不同；以 .env 为准"
        TLS_MODE="$existing"
    fi
    existing="$(env_get DD_HUB_IMAGE "$ENV_FILE" || true)"
    if [[ -n "$existing" ]]; then
        IMAGE="$existing"
        info "沿用 .env 里的镜像：$IMAGE"
    fi
    info "保留已有 .env（要重写请加 --force）"
fi
[[ -n "${IMAGE:-}" ]] || IMAGE="${IMAGE_REPO}:${TAG}"

# ---------------------------------------------------------------- 自检模式
if (( CHECK_ONLY )); then
    info "自检模式：只渲染 compose，不登录、不拉取、不启动"
    DD_BIND_IP="$BIND_IP" \
    DD_HUB_TLS_MODE="$TLS_MODE" \
    DD_HUB_IMAGE="$IMAGE" \
        docker compose -f "$SCRIPT_DIR/docker-compose.yml" --profile hub config
    info "渲染通过"
    exit 0
fi

# ---------------------------------------------------------------- 守护进程与端口
docker info >/dev/null 2>&1 || die "无法连接 Docker 守护进程（需要 root 或 docker 组权限）"
require_cmd curl

port_in_use() {
    local port="$1"
    command -v ss >/dev/null 2>&1 || return 1
    [[ -n "$(ss -ltnH "sport = :${port}" 2>/dev/null || true)" ]]
}

if port_in_use "$HUB_PORT"; then
    die "端口 ${HUB_PORT} 已被占用；Hub 无法启动。先停掉占用者，或确认已有 Hub 在跑（此时无需重装）"
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

if (( DO_LOGIN )); then
    ghcr_login
else
    info "按 --skip-login 跳过登录，直接尝试拉取"
fi

# ---------------------------------------------------------------- 拉取（失败即停）
info "拉取镜像：$IMAGE"
if ! docker pull "$IMAGE"; then
    warn "镜像拉取失败：$IMAGE"
    warn "若上面提示 unauthorized / denied / 401，多半是 GHCR 未登录或 token 过期："
    warn "  docker login ghcr.io    # 用户名 = GitHub 账号，密码 = 有 read:packages 的 PAT"
    warn "  然后重跑本脚本（可加 --skip-login 避免重复登录）"
    exit 1
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
# 由 deploy/install-hub.sh 生成于 $(date -u '+%Y-%m-%dT%H:%M:%SZ')
# 手工改动会被保留：重跑脚本不会覆盖本文件，除非加 --force。
DD_BIND_IP=${BIND_IP}
DD_HUB_TLS_MODE=${TLS_MODE}
DD_HUB_IMAGE=${IMAGE}
EOF
    )
    info "已写入 $ENV_FILE"
fi
chmod 600 "$ENV_FILE"

# ---------------------------------------------------------------- 启动
info "启动 Hub（--profile hub）"
docker compose --project-directory "$INSTALL_DIR" -f "$INSTALL_DIR/docker-compose.yml" \
    --profile hub up -d

# ---------------------------------------------------------------- 探活
scheme=https
[[ "$TLS_MODE" == "none" ]] && scheme=http
URL="${scheme}://${BIND_IP}:${HUB_PORT}${HEALTH_PATH}"

# /api/v1/health 恒返回 200，不查库、不校验身份（cmd/hub/main.go:192）——
# 它证明的是「HTTP/TLS 已起来」，正是安装脚本要的判定。
wait_health() {
    local deadline=$(( SECONDS + 60 )) code=""
    while (( SECONDS < deadline )); do
        code="$(curl -ksS -o /dev/null -w '%{http_code}' --max-time 3 "$URL" 2>/dev/null || true)"
        [[ "$code" == "200" ]] && return 0
        sleep 2
    done
    printf '%s' "$code"
    return 1
}

info "探活：$URL"
if ! last_code="$(wait_health)"; then
    warn "60 秒内未就绪（最后状态码：${last_code:-无响应}）"
    warn "容器已保留，排查：docker logs --tail 100 dockdeck-hub"
    exit 1
fi

# ---------------------------------------------------------------- 汇报
printf '\n✅ Hub 已就绪\n\n'
printf '  面板地址   %s://%s:%s/\n' "$scheme" "$BIND_IP" "$HUB_PORT"
printf '  首次使用   打开上面的地址会跳到 /setup 建管理员账号（脚本不代建）\n'
printf '  数据卷     dockdeck_dockdeck-hub-data（SQLite / 主密钥 / 自签证书）\n'
printf '  安装目录   %s\n' "$INSTALL_DIR"
if [[ "$TLS_MODE" == "self" ]]; then
    printf '  自签证书   浏览器首次访问会告警；指纹在 设置 → 安全，或 docker logs dockdeck-hub\n'
fi
printf '\n下一步：其余节点各自执行 deploy/install-agent.sh 装 Agent。\n'
