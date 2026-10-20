#!/usr/bin/env bash
# ============================================================================
# DockDeck Agent 安装脚本（S10.1）
#
# 把 deploy/README.md 的手工几步收敛成一条命令，可重复执行：
#   预检 → 读已有 .env → 登录 GHCR → 拉镜像
#   → 落 .env / token / compose → up -d → 查纳管日志
#
# 用法：
#   sudo ./install-agent.sh --hub-url https://100.64.0.1:8443 --server-name node2
#   sudo ./install-agent.sh                    # 全交互
#   sudo ./install-agent.sh --check            # 只渲染 compose 自检
#
# v0.2.6 起 Agent 只有反向长连接一种形态：不监听端口，无需绑定 IP，
# 也就不再检测/校验 BIND_IP、不查端口占用。探活信号 = 容器在跑 + 已纳管日志。
#
# 幂等：已有 .env 不覆盖（重复执行 = 拉 .env 里那个 tag + up -d）；--force 才重写。
# 凭据：join_secret 走 read -s，只写 600 的 secrets/agent.token 与 .env，
#       不进命令行；token 文件内容 docker inspect 看不到。
# ============================================================================
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly COMPOSE_FILE=docker-compose.agent.yml
readonly DEFAULT_DIR=/opt/dockdeck
readonly DEFAULT_GHCR_USER=yy1319hua
readonly IMAGE_REPO=ghcr.io/yy1319hua/dockdeck-agent
readonly CONTAINER_NAME=dockdeck-agent
readonly PROJECT_NAME=dockdeck-agent

HUB_URL=""
SERVER_NAME=""
JOIN_SECRET=""
TAG="latest"
IMAGE=""
INSTALL_DIR="$DEFAULT_DIR"
PULL_USER="$DEFAULT_GHCR_USER"
PULL_TOKEN_FILE=""
DO_LOGIN=1
FORCE=0
CHECK_ONLY=0
FS_ROOTS=""
COMPOSE_DIRS=""
SELF_CONTAINER=""
DISK_MOUNTS=""

info() { printf '[agent] %s\n' "$*"; }
warn() { printf '[agent] ⚠️  %s\n' "$*" >&2; }
die() { printf '[agent] ❌ %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
DockDeck Agent 安装脚本

用法：install-agent.sh [选项]

  --hub-url <URL>         Hub 地址，含端口，如 https://100.64.0.1:8443
  --server-name <NAME>    在 Hub 上显示的机器名，默认本机短服务器名
  --join-secret <SECRET>  Hub「添加服务器」生成的一次性纳管密钥（30 分钟有效）
  --tag <TAG>             镜像 tag，默认 latest（已有 .env 时以 .env 为准）
  --image <REF>           完整镜像引用（可含加速域名），默认 ghcr.io/yy1319hua/dockdeck-agent:<tag>；配了就从指定源拉取
  --dir <PATH>            安装目录，默认 /opt/dockdeck
  --pull-user <USER>      GHCR 用户名（GitHub 账号），默认 yy1319hua
  --pull-token-file <P>   从文件读 GHCR token（无人值守）；缺省交互式输入
  --skip-login            匿名拉取失败时也不尝试登录（公开镜像用不到）
  --fs-roots <LIST>       文件管理根白名单，逗号分隔（留空 = 关闭文件模块）
  --compose-dirs <LIST>   Compose 白名单目录，逗号分隔（留空 = Compose 页为空）
  --self-container <NAME> 自身容器名，默认 dockdeck-agent（保护它不被面板误操作）
  --disk-mounts <LIST>    磁盘挂载点白名单，逗号分隔（留空 = 自动探测）
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
        --tag) TAG="$2"; shift 2 ;;
        --image) IMAGE="$2"; shift 2 ;;
        --dir) INSTALL_DIR="$2"; shift 2 ;;
        --pull-user) PULL_USER="$2"; shift 2 ;;
        --pull-token-file) PULL_TOKEN_FILE="$2"; shift 2 ;;
        --skip-login) DO_LOGIN=0; shift ;;
        --check) CHECK_ONLY=1; shift ;;
        --force) FORCE=1; shift ;;
        --fs-roots) FS_ROOTS="$2"; shift 2 ;;
        --compose-dirs) COMPOSE_DIRS="$2"; shift 2 ;;
        --self-container) SELF_CONTAINER="$2"; shift 2 ;;
        --disk-mounts) DISK_MOUNTS="$2"; shift 2 ;;
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
    DD_HUB_URL="${HUB_URL:-https://127.0.0.1:8443}" \
    DD_AGENT_JOIN_SECRET="${JOIN_SECRET:-selfcheck-placeholder}" \
    DD_AGENT_IMAGE="$IMAGE" \
        docker compose -p "$PROJECT_NAME" -f "$SCRIPT_DIR/$COMPOSE_FILE" config
    info "渲染通过"
    exit 0
fi

# ---------------------------------------------------------------- 守护进程、socket
docker info >/dev/null 2>&1 || die "无法连接 Docker 守护进程（需要 root 或 docker 组权限）"

[[ -S /var/run/docker.sock ]] \
    || die "找不到 /var/run/docker.sock；Agent 必须能访问本机 Docker Engine 的 socket"

# v0.2.6 起 Agent 不监听端口（反向长连接），原先的 9527 占用检查已无意义，删除。

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
    local src="$SCRIPT_DIR/$COMPOSE_FILE"
    local dst="$1/$COMPOSE_FILE"
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
DD_HUB_URL=${HUB_URL}
DD_AGENT_JOIN_SECRET=${JOIN_SECRET}
DD_AGENT_IMAGE=${IMAGE}
DD_AGENT_FS_ROOTS=${FS_ROOTS:-$INSTALL_DIR}
DD_AGENT_COMPOSE_DIRS=${COMPOSE_DIRS:-$INSTALL_DIR}
DD_AGENT_SELF_CONTAINER=${SELF_CONTAINER:-dockdeck-agent}
DD_AGENT_DISK_MOUNTS=${DISK_MOUNTS}
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
info "启动 Agent（反向长连接，project=$PROJECT_NAME）"
docker compose --project-directory "$INSTALL_DIR" -p "$PROJECT_NAME" \
    -f "$INSTALL_DIR/$COMPOSE_FILE" up -d

# ---------------------------------------------------------------- 就绪确认
# v0.2.6 起 Agent 不监听端口，没有可探的 HTTP 端点；且纳管是启动期同步步骤
# （Hub 不可达会无限重试，凭据/邀请类错误直接退出），所以「容器进入 running」
# 就是就绪信号，退出则被下面的等待抓获。
wait_running() {
    local deadline=$(( SECONDS + 60 )) state=""
    while (( SECONDS < deadline )); do
        state="$(docker inspect -f '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || true)"
        [[ "$state" == "running" ]] && return 0
        sleep 2
    done
    printf '%s' "${state:-无容器}"
    return 1
}

info "等待 Agent 容器就绪（无监听端口，以容器状态判活）"
if ! last_state="$(wait_running)"; then
    warn "60 秒内容器未进入 running（最后状态：${last_state}）"
    warn "v0.2.6 起纳管是启动期硬前提：Hub 地址不可达会一直重试，"
    warn "join_secret 无效/过期则直接退出（restart 策略会反复拉起）。"
    warn "容器已保留，排查：docker logs --tail 100 ${CONTAINER_NAME}"
    exit 1
fi

# ---------------------------------------------------------------- 纳管结果
# 纳管同步完成后 main 会打「已纳管，主动连接 Hub」，enroll 另有
# 「已纳管为 Hub 服务器 #id」一行；出现任一行即成功。
enroll_state="unknown"
deadline=$(( SECONDS + 20 ))
while (( SECONDS < deadline )); do
    logs="$(docker logs "$CONTAINER_NAME" 2>&1 || true)"
    if grep -q '已纳管' <<<"$logs"; then
        enroll_state="ok"
        break
    fi
    sleep 2
done

# ---------------------------------------------------------------- 汇报
printf '\n✅ Agent 已就绪\n\n'
printf '  模式       反向长连接（不监听端口，主动连 Hub）\n'
printf '  机器名     %s\n' "$SERVER_NAME"
printf '  Hub        %s\n' "$HUB_URL"
printf '  安装目录   %s\n\n' "$INSTALL_DIR"

case "$enroll_state" in
    ok)
        printf '  纳管状态   已自注册，Hub 服务器列表应能看到「%s」\n\n' "$SERVER_NAME"
        ;;
    *)
        printf '  纳管状态   20 秒内未见纳管日志，请到 Hub 服务器列表确认；\n'
        printf '             未出现则看 docker logs --tail 50 %s\n\n' "$CONTAINER_NAME"
        ;;
esac
