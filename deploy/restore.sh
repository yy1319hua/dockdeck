#!/usr/bin/env bash
# ============================================================================
# DockDeck Hub 整机恢复脚本（S10.2）——backup.sh 的逆操作
#
# 流程：归档 + sha256 校验 → 恢复前快照（调 backup.sh，可 --skip-pre）→
#   交互确认 → 停 Hub 容器 → 清空数据卷 → 解包归档（保 uid 10000 属主）→
#   起容器。探活留给人工 / verify.sh（S10.3），本脚本只保证容器起来。
#
# 危险操作：清空卷不可逆。确认提示默认开启（--force 跳过）。
#
# 用法：bash restore.sh <archive.tar.gz> [--dir /opt/dockdeck] [--volume 名字]
#                      [--force] [--skip-pre] [--keep N]
# ============================================================================
set -euo pipefail

BACKUP_DIR="/opt/dockdeck/backups"
VOLUME="dockdeck_dockdeck-hub-data"
FORCE=0
SKIP_PRE=0
KEEP=14
CONTAINER="dockdeck-hub"
ARCHIVE=""

log() { echo "[restore] $*"; }
die() { echo "[restore] ❌ $*" >&2; exit 1; }
usage() { sed -n '3,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dir)     BACKUP_DIR="$2"; shift 2 ;;
    --volume)  VOLUME="$2"; CONTAINER=""; shift 2 ;;  # 自定义卷=自定义部署，不动容器
    --force)   FORCE=1; shift ;;
    --skip-pre) SKIP_PRE=1; shift ;;
    --keep)    KEEP="$2"; shift 2 ;;
    --help|-h) usage ;;
    -*)        die "未知参数：$1（--help 看用法）" ;;
    *)         [ -z "$ARCHIVE" ] || die "归档文件只能给一个"; ARCHIVE="$1"; shift ;;
  esac
done

[ -n "$ARCHIVE" ] || { usage; }
[ -f "$ARCHIVE" ] || die "归档不存在：$ARCHIVE"
command -v docker >/dev/null 2>&1 || die "宿主机没有 docker 命令"
docker volume inspect "$VOLUME" >/dev/null 2>&1 || die "找不到数据卷 $VOLUME"
command -v sha256sum >/dev/null 2>&1 || die "宿主机没有 sha256sum"

# ── 1. 归档完整性校验 ────────────────────────────────────────────────────────
log "校验归档：$ARCHIVE"
EXPECT="$(awk 'NR==1{print $1}' "${ARCHIVE}.sha256" 2>/dev/null || true)"
[ -n "$EXPECT" ] || die "缺 sha256 清单：${ARCHIVE}.sha256（备份脚本产物必带，缺档=来源可疑，拒恢复）"
ACTUAL="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
[ "$ACTUAL" = "$EXPECT" ] || die "sha256 不匹配：期望 $EXPECT 实际 $ACTUAL"
LIST="$(docker run --rm -v "$ARCHIVE:/src/archive.tar.gz:ro" alpine:3 tar tzf /src/archive.tar.gz)" \
  && echo "$LIST" | grep -qE "^\./?dockdeck\.db$" \
  || die "归档里没有 dockdeck.db——不像 backup.sh 的产物，拒恢复"

# ── 2. 恢复前快照（后悔药）──────────────────────────────────────────────────
if [ "$SKIP_PRE" != 1 ]; then
  log "恢复前先把当前卷备份一份（--skip-pre 跳过）"
  bash "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"/backup.sh --dir "$BACKUP_DIR" --volume "$VOLUME" --keep "$KEEP" --quiet \
    || die "恢复前快照失败——已中止，当前数据未被改动"
fi

# ── 3. 确认 ────────────────────────────────────────────────────────────────
if [ "$FORCE" != 1 ]; then
  echo "──────────────────────────────────────────────"
  echo " 即将用 $(basename "$ARCHIVE") 覆盖数据卷 $VOLUME 的全部内容"
  echo " 卷内现有数据将被清空（恢复前快照见 $BACKUP_DIR）"
  echo "──────────────────────────────────────────────"
  read -r -p "确认恢复？输入 yes 继续：" ans
  [ "$ans" = "yes" ] || die "已取消（输入了「$ans」而非 yes）"
fi

# ── 4. 停容器（若在跑）─────────────────────────────────────────────────────
WAS_RUNNING=0
if [ -n "$CONTAINER" ] && docker container inspect "$CONTAINER" >/dev/null 2>&1; then
  if [ "$(docker container inspect -f '{{.State.Running}}' "$CONTAINER")" = "true" ]; then
    log "停止容器 $CONTAINER"
    docker stop "$CONTAINER" >/dev/null
    WAS_RUNNING=1
  fi
fi

# ── 5. 清卷 → 解包 → 属主兜底 ──────────────────────────────────────────────
log "清空卷并还原归档内容"
docker run --rm \
  -v "${VOLUME}:/data" \
  -v "$(cd "$(dirname "$ARCHIVE")" && pwd):/src:ro" \
  -e ARCHIVE_NAME="$(basename "$ARCHIVE")" \
  alpine:3 sh -eu -c '
    find /data -mindepth 1 -delete
    tar xzf "/src/${ARCHIVE_NAME}" -C /data --numeric-owner
    chown -R 10000:10000 /data          # 兜底：归档属主异常时也不至于让 Hub 读不了
    chmod 600 /data/master.key 2>/dev/null || true
    ls /data >/dev/null                  # 解包结果非空的最低断言
  '

# ── 6. 起容器 ──────────────────────────────────────────────────────────────
if [ "$WAS_RUNNING" = 1 ]; then
  log "重新启动 $CONTAINER"
  docker start "$CONTAINER" >/dev/null
  log "容器已启动；请目验探活：curl -k https://<DD_BIND_IP>:8443/api/v1/health 期望 200"
else
  log "Hub 容器原未运行；数据已就位，自行 compose up -d 后按上方方式探活"
fi
log "✅ 恢复完成"
