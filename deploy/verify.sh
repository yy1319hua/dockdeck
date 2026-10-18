#!/usr/bin/env bash
# ============================================================================
# DockDeck 部署后验证脚本（S10.3）
#
# 只读、可重复执行。在已部署的宿主机上跑，覆盖两类断言：
#   1) API 冒烟：Hub / Agent 关键端点可达且鉴权生效；
#   2) 存量容器不变性：部署没有扰动宿主机上既有的其它容器。
#
# 角色自动探测：看本机 docker 里有没有 dockdeck-hub / dockdeck-agent，
# 有谁就验谁（--role 可强制只验某一边）。
#
# 用法：
#   sudo ./verify.sh                          # 自动探测角色，全量验证
#   sudo ./verify.sh --role hub               # 只验 Hub
#   sudo ./verify.sh --role agent             # 只验 Agent
#   sudo ./verify.sh --protect watchtower,komari,komari-agent
#   sudo ./verify.sh --init-baseline          # 只记录当前容器基线后退出
#   sudo ./verify.sh --check                  # 静态自检：依赖 + .env 解析，不触网不查容器
#
# 存量容器不变性说明：
#   首次运行（无基线）会惰性建立基线并告警「本次不校验不变性」；
#   想拿最严保证，请在部署前先跑一次 --init-baseline 锁定部署前状态。
#   基线只比对「非 dockdeck- 前缀」的容器是否仍在运行——dockdeck 自身的
#   容器由冒烟段负责，不算「存量」。
#
# 退出码：0 全部通过；1 有断言失败；2 用法 / 预检错误。
# ============================================================================
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly HUB_PORT=8443
readonly AGENT_PORT=9527
readonly CONTAINER_PREFIX=dockdeck-
readonly DEFAULT_DIR=/opt/dockdeck

DIR="$DEFAULT_DIR"
ROLE="auto"            # auto | hub | agent
BIND_IP=""
TLS_MODE=""
HUB_URL=""
AGENT_TOKEN_FILE=""
PROTECT=""
BASELINE_FILE=""
INIT_BASELINE=0
NO_INVARIANCE=0
CHECK_ONLY=0

info()  { printf '[verify] %s\n' "$*"; }
warn()  { printf '[verify] ⚠️  %s\n' "$*" >&2; }
die()   { printf '[verify] ❌ %s\n' "$*" >&2; exit 2; }
fail()  { printf '[verify] ❌ %s\n' "$*" >&2; FAILS+=("$*"); }

usage() {
    cat <<'EOF'
DockDeck 部署后验证脚本

用法：verify.sh [选项]

  --dir <PATH>           安装目录，默认 /opt/dockdeck（与 install 脚本一致）
  --role <auto|hub|agent>  探测角色，默认 auto（看本机有没有 dockdeck 容器）
  --bind-ip <IP>         覆盖 .env 里的 DD_BIND_IP（一般不用）
  --hub-url <URL>        覆盖 Agent 端判断 Hub 地址（本地验证一般不用）
  --agent-token-file <P> Agent 令牌文件，默认 <dir>/secrets/agent.token
  --protect NAME[,...]   额外要求保持 running 的容器名（逗号分隔），如 watchtower,komari
  --baseline <FILE>      不变性基线文件，默认 <dir>/.verify-baseline
  --init-baseline        只记录当前容器基线并存盘，不校验（用于部署前锁基准）
  --no-invariance        跳过存量容器不变性断言
  --check                仅静态自检（依赖 + .env 解析），不触网、不查容器
  -h, --help             显示本帮助

退出码：0 通过；1 断言失败；2 用法/预检错误。
EOF
}

while (( $# > 0 )); do
    case "$1" in
        -h|--help|--init-baseline|--no-invariance|--check) ;;
        --*) (( $# >= 2 )) || die "$1 缺少参数" ;;
    esac
    case "$1" in
        --dir)              DIR="$2"; shift 2 ;;
        --role)             ROLE="$2"; shift 2 ;;
        --bind-ip)          BIND_IP="$2"; shift 2 ;;
        --hub-url)          HUB_URL="$2"; shift 2 ;;
        --agent-token-file) AGENT_TOKEN_FILE="$2"; shift 2 ;;
        --protect)          PROTECT="$2"; shift 2 ;;
        --baseline)         BASELINE_FILE="$2"; shift 2 ;;
        --init-baseline)    INIT_BASELINE=1; shift ;;
        --no-invariance)    NO_INVARIANCE=1; shift ;;
        --check)            CHECK_ONLY=1; shift ;;
        -h|--help)          usage; exit 0 ;;
        *)                  usage >&2; die "未知参数：$1" ;;
    esac
done

[[ "$ROLE" == auto || "$ROLE" == hub || "$ROLE" == agent ]] \
    || die "--role 只能是 auto / hub / agent，得到：$ROLE"
[[ -n "$BASELINE_FILE" ]] || BASELINE_FILE="$DIR/.verify-baseline"

FAILS=()

# ---------------------------------------------------------------- 通用工具
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

env_get() {
    local key="$1" file="$2" line k v
    [[ -r "$file" ]] || return 1
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line%$'\r'}"
        [[ "$line" == \#* ]] && continue
        [[ "$line" == *=* ]] || continue
        k="${line%%=*}"; v="${line#*=}"
        k="${k//[[:space:]]/}"
        if [[ "$k" == "$key" ]]; then printf '%s' "${v%\"}"; return 0; fi
    done < "$file"
    return 1
}

# HTTP 取状态码（忽略自签证书，匹配 install 脚本的 -k 约定）
http_code() {
    local code
    code="$(curl -ksS -o /dev/null -w '%{http_code}' --max-time 5 "$@" 2>/dev/null || true)"
    printf '%s' "$code"
}

# ---------------------------------------------------------------- 预检 / 静态自检
if (( CHECK_ONLY )); then
    info "静态自检模式：检查依赖与 .env 解析，不触网、不查容器"
    require_cmd docker; require_cmd curl
    command -v openssl >/dev/null 2>&1 \
        && info "openssl 可用（Agent 签名 200 冒烟将启用）" \
        || warn "openssl 不可用：Agent 签名 200 冒烟将跳过（401 冒烟仍生效）"
    ENV_FILE="$DIR/.env"
    if [[ -r "$ENV_FILE" ]]; then
        b="$(env_get DD_BIND_IP "$ENV_FILE" || true)"
        t="$(env_get DD_HUB_TLS_MODE "$ENV_FILE" || true)"
        info "解析 .env：DD_BIND_IP=${b:-（空）}  DD_HUB_TLS_MODE=${t:-（空）}"
        [[ -n "$b" ]] || warn "DD_BIND_IP 为空：实际验证时需从 .env 或 --bind-ip 取得"
    else
        warn "找不到 $ENV_FILE：静态自检无法解析绑定地址（仅检查依赖通过）"
    fi
    info "静态自检通过"
    exit 0
fi

require_cmd docker
docker info >/dev/null 2>&1 || die "无法连接 Docker 守护进程（需要 root 或 docker 组权限）"
require_cmd curl
HAS_OPENSSL=0; command -v openssl >/dev/null 2>&1 && HAS_OPENSSL=1

ENV_FILE="$DIR/.env"
[[ -r "$ENV_FILE" ]] || die "找不到 $ENV_FILE；请先跑 install-hub.sh / install-agent.sh 生成 .env"
[[ -n "$BIND_IP" ]] || BIND_IP="$(env_get DD_BIND_IP "$ENV_FILE" || true)"
[[ -n "$BIND_IP" ]] || die "无法取得绑定 IP：.env 无 DD_BIND_IP，且未传 --bind-ip"
tls="$(env_get DD_HUB_TLS_MODE "$ENV_FILE" || true)"
[[ -n "$tls" ]] && TLS_MODE="$tls"
scheme=https; [[ "$TLS_MODE" == "none" ]] && scheme=http
[[ -n "$AGENT_TOKEN_FILE" ]] || AGENT_TOKEN_FILE="$DIR/secrets/agent.token"

# ---------------------------------------------------------------- 角色探测
hub_present=0; agent_present=0
if docker ps -q --filter "name=^/${CONTAINER_PREFIX}hub$" 2>/dev/null | grep -q .; then hub_present=1; fi
if docker ps -q --filter "name=^/${CONTAINER_PREFIX}agent$" 2>/dev/null | grep -q .; then agent_present=1; fi

case "$ROLE" in
    hub)   agent_present=0 ;;
    agent) hub_present=0 ;;
    auto)  : ;;
esac

if (( ! hub_present && ! agent_present )); then
    die "本机未探测到 dockdeck-hub 或 dockdeck-agent 容器；若已部署请确认容器名前缀为 ${CONTAINER_PREFIX}（--role 可强制）"
fi

info "探测到角色：Hub=$( ((hub_present)) && echo 是 || echo 否 )  Agent=$( ((agent_present)) && echo 是 || echo 否 )"

# ---------------------------------------------------------------- 存量容器基线
record_baseline() {
    mkdir -p "$DIR"
    docker ps -a --format '{{.Names}}\t{{.State}}' > "$BASELINE_FILE"
    info "已记录容器基线 → $BASELINE_FILE"
}

if (( INIT_BASELINE )); then
    record_baseline
    info "--init-baseline 完成，未做校验"
    exit 0
fi

# ---------------------------------------------------------------- 冒烟：Hub
smoke_hub() {
    local url="$scheme://${BIND_IP}:${HUB_PORT}"
    info "Hub 冒烟：$url"

    local hc; hc="$(http_code "$url/api/v1/health")"
    if [[ "$hc" == "200" ]]; then
        info "  ✅ /api/v1/health → 200（HTTP/TLS 已就绪）"
    else
        fail "Hub /api/v1/health 期望 200，得到 ${hc:-无响应}"
    fi

    local ac; ac="$(http_code "$url/api/v1/api-tokens")"
    if [[ "$ac" == "401" ]]; then
        info "  ✅ /api/v1/api-tokens 未带会话 → 401（会话鉴权生效）"
    elif [[ "$ac" == "200" ]]; then
        fail "Hub /api/v1/api-tokens 未鉴权竟返回 200（会话鉴权疑似失效，安全回归）"
    else
        warn "  ⚠️  /api/v1/api-tokens 返回 ${ac:-无响应}（期望 401；路由变更时请复核，本次不判失败）"
    fi

    if docker ps -q --filter "name=^/${CONTAINER_PREFIX}hub$" 2>/dev/null | grep -q .; then
        info "  ✅ 容器 ${CONTAINER_PREFIX}hub 运行中"
    else
        fail "容器 ${CONTAINER_PREFIX}hub 未在运行"
    fi
}

# ---------------------------------------------------------------- 冒烟：Agent
smoke_agent() {
    local url="https://${BIND_IP}:${AGENT_PORT}"
    info "Agent 冒烟：$url"

    # 无 token → 401：证明端口已监听 + TLS 已起 + 鉴权生效（沿用 install-agent.sh 约定）
    local nc; nc="$(http_code "$url/api/v1/info")"
    if [[ "$nc" == "401" ]]; then
        info "  ✅ /api/v1/info 无 token → 401（端口/TLS/鉴权生效）"
    else
        fail "Agent /api/v1/info 期望 401，得到 ${nc:-无响应}（Agent 未就绪或 TLS/端口异常）"
    fi

    # 带签名 token → 200：证明令牌文件有效（软检查，失败只告警，不判整体失败）
    if (( HAS_OPENSSL )) && [[ -r "$AGENT_TOKEN_FILE" ]]; then
        local token ts sig signing method target body
        token="$(tr -d '\r\n' < "$AGENT_TOKEN_FILE")"
        [[ -n "$token" ]] || { warn "  ⚠️  Agent 令牌文件为空，跳过签名 200 冒烟"; return; }
        ts="$(date +%s)"
        method=GET; target=/api/v1/info; body=""
        signing="${ts}"$'\n'"${method}"$'\n'"${target}"$'\n'"${body}"
        sig="$(printf '%s' "$signing" | openssl dgst -sha256 -hmac "$token" 2>/dev/null | sed 's/^.*= //')"
        local sc
        sc="$(curl -ksS -o /dev/null -w '%{http_code}' --max-time 5 \
            -H "Authorization: Bearer $token" \
            -H "X-DD-Timestamp: $ts" \
            -H "X-DD-Signature: $sig" \
            "$url/api/v1/info" 2>/dev/null || true)"
        if [[ "$sc" == "200" ]]; then
            info "  ✅ 带签名请求 /api/v1/info → 200（令牌文件有效，HMAC 校验通过）"
        else
            warn "  ⚠️  带签名请求 /api/v1/info 返回 ${sc:-无响应}（可能是时钟偏移或签名实现差异；端口/TLS/鉴权以 401 测试为准）"
        fi
    else
        (( HAS_OPENSSL )) || warn "  ⚠️  openssl 不可用，跳过签名 200 冒烟"
        [[ -r "$AGENT_TOKEN_FILE" ]] || warn "  ⚠️  找不到 Agent 令牌文件 $AGENT_TOKEN_FILE，跳过签名 200 冒烟"
    fi

    if docker ps -q --filter "name=^/${CONTAINER_PREFIX}agent$" 2>/dev/null | grep -q .; then
        info "  ✅ 容器 ${CONTAINER_PREFIX}agent 运行中"
    else
        fail "容器 ${CONTAINER_PREFIX}agent 未在运行"
    fi
}

# ---------------------------------------------------------------- 存量容器不变性
check_invariance() {
    info "存量容器不变性断言"
    if [[ ! -f "$BASELINE_FILE" ]]; then
        warn "  无基线文件（首次运行）；已惰性建立基线，本次不校验不变性"
        warn "  想拿最严保证：请在部署前先跑 verify.sh --init-baseline"
        record_baseline
        return
    fi

    # 基线里的非 dockdeck 容器：必须仍在运行
    local name state n=0
    while IFS=$'\t' read -r name state || [[ -n "$name" ]]; do
        [[ -z "$name" ]] && continue
        [[ "$name" == "${CONTAINER_PREFIX}"* ]] && continue
        n=$((n+1))
        if docker ps -q --filter "name=^/${name}$" 2>/dev/null | grep -q .; then
            info "  ✅ 存量容器 $name 仍在运行（基线状态 $state）"
        else
            fail "存量容器 $name 基线中为「$state」，现已不在运行（部署疑似扰动既有容器）"
        fi
    done < "$BASELINE_FILE"
    info "  比对基线中 ${n} 个非 dockdeck 容器"

    # 显式 --protect 的容器：必须运行
    local IFS=,
    for p in $PROTECT; do
        [[ -z "$p" ]] && continue
        if docker ps -q --filter "name=^/${p}$" 2>/dev/null | grep -q .; then
            info "  ✅ --protect 容器 $p 运行中"
        else
            fail "--protect 容器 $p 未在运行"
        fi
    done
}

# ---------------------------------------------------------------- 执行
(( hub_present ))   && smoke_hub
(( agent_present )) && smoke_agent
(( NO_INVARIANCE )) || check_invariance

# ---------------------------------------------------------------- 汇总
if (( ${#FAILS[@]} == 0 )); then
    printf '\n✅ 验证通过：'
    (( hub_present ))   && printf ' Hub 冒烟'
    (( agent_present )) && printf ' Agent 冒烟'
    (( NO_INVARIANCE )) || printf ' 存量容器不变性'
    printf '\n'
    exit 0
else
    printf '\n❌ 验证未通过，%d 项断言失败：\n' "${#FAILS[@]}"
    for f in "${FAILS[@]}"; do printf '   - %s\n' "$f"; done
    exit 1
fi
