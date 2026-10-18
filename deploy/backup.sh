#!/usr/bin/env bash
# ============================================================================
# DockDeck Hub 整机备份脚本（S10.2）
#
# 备份对象：命名卷 dockdeck_dockdeck-hub-data（SQLite 库 + 主密钥 + 自签证书）
# 产物：    <dir>/backups/dockdeck-backup-<时间戳>.tar.gz + 同名 .sha256
# 轮转：    默认保留 14 份（--keep N 可调），超出删最旧
#
# 一致性口径：不停机。一次性 alpine:3 容器（apk add sqlite）对在线库执行
#   VACUUM INTO —— 产出自包含的一致性快照（等价于面板 /api/v1/admin/backup
#   的落盘版，但不依赖 token / 面板可用）。WAL/热库文件不进归档，归档里的
#   dockdeck.db 一律来自快照。
#
# 前提：docker 可用；Hub 数据卷存在（Hub 容器停着也能备份）。
# 需要：宿主可拉 alpine:3 镜像 + 容器内可 apk 联网装 sqlite（二者失败即停）。
#
# 用法：bash backup.sh [--dir /opt/dockdeck] [--keep 14] [--volume 名字] [--quiet]
# 恢复：用同目录 restore.sh；归档先过 sha256 校验再整体还原卷内容。
# ============================================================================
set -euo pipefail

BACKUP_DIR="/opt/dockdeck/backups"
KEEP=14
VOLUME="dockdeck_dockdeck-hub-data"
QUIET=0

log() { [ "$QUIET" = 1 ] || echo "[backup] $*"; }
die() { echo "[backup] ❌ $*" >&2; exit 1; }
usage() { sed -n '3,21p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dir)     BACKUP_DIR="$2"; shift 2 ;;
    --keep)    KEEP="$2"; shift 2 ;;
    --volume)  VOLUME="$2"; shift 2 ;;
    --quiet)   QUIET=1; shift ;;
    --help|-h) usage ;;
    -*)        die "未知参数：$1（--help 看用法）" ;;
    *)         die "不接受位置参数：$1" ;;
  esac
done

case "$KEEP" in ''|*[!0-9]*) die "--keep 必须是正整数" ;; esac
[ "$KEEP" -ge 1 ] || die "--keep 必须 ≥ 1"
command -v docker >/dev/null 2>&1 || die "宿主机没有 docker 命令"
docker volume inspect "$VOLUME" >/dev/null 2>&1 \
  || die "找不到数据卷 $VOLUME（若改过项目名用 --volume 指定；docker volume ls 查看）"

STAMP="$(date +%Y%m%d-%H%M%S)"
FINAL_TAR="${BACKUP_DIR}/dockdeck-backup-${STAMP}.tar.gz"
FINAL_SHA="${FINAL_TAR}.sha256"
WORK_TAR="${BACKUP_DIR}/.work-${STAMP}.tar.gz"
MARK="$(mktemp)"
mkdir -p "$BACKUP_DIR"

log "备份数据卷 ${VOLUME} → ${FINAL_TAR}"

# 一次性容器：装 sqlite → VACUUM INTO 快照 → staging（快照顶替热库文件、
# 排除卷内 backups/ 与 WAL 文件）→ tar.gz → sha256。
# 失败退出码 30/31/32 供排障区分；成功向 stdout 打「OK <大小>」标记。
docker run --rm \
  -v "${VOLUME}:/data" \
  -v "${BACKUP_DIR}:/dest" \
  -e STAMP="$STAMP" \
  alpine:3 sh -eu -c '
    apk add --no-cache sqlite >/dev/null 2>&1 \
      || { echo "apk add sqlite 失败（容器内无法联网？）" >&2; exit 30; }
    [ -f /data/dockdeck.db ] \
      || { echo "卷里没有 /data/dockdeck.db——这不像 Hub 数据卷" >&2; exit 31; }
    # SQL 字符串字面量需要引号；本 payload 整体是单引号串，不能出现裸引号，
    # 用 printf 动态取引号符拼 SQL
    SQLQ="$(printf "\047")"
    sqlite3 /data/dockdeck.db "VACUUM INTO ${SQLQ}/data/.dd-bak-${STAMP}${SQLQ}" \
      || { echo "VACUUM INTO 失败（库损坏或权限不足？）" >&2; exit 32; }
    STAGE="$(mktemp -d)"
    for f in /data/* /data/.[!.]*; do
      [ -e "$f" ] || continue
      b=$(basename "$f")
      case "$b" in
        dockdeck.db|dockdeck.db-wal|dockdeck.db-shm|backups) continue ;;
        .dd-bak-*) continue ;;
      esac
      cp -a "$f" "$STAGE/"
    done
    cp "/data/.dd-bak-${STAMP}" "$STAGE/dockdeck.db"
    rm -f "/data/.dd-bak-${STAMP}"
    tar czf "/dest/.work-${STAMP}.tar.gz" -C "$STAGE" .
    sha256sum "/dest/.work-${STAMP}.tar.gz" | sed "s|/dest/||" > "/dest/.work-${STAMP}.tar.gz.sha256"
    echo "OK $(du -h "/dest/.work-${STAMP}.tar.gz" | awk "{print \$1}")"
  ' | tee "$MARK" || { rm -f "$MARK"; die "备份容器执行失败（上方有原因）；未产出任何归档"; }

grep -q '^OK ' "$MARK" || { rm -f "$MARK"; die "容器输出异常，未见成功标记"; }
rm -f "$MARK"

mv "$WORK_TAR" "$FINAL_TAR"
mv "${WORK_TAR}.sha256" "${FINAL_SHA}"
log "归档完成：${FINAL_TAR}（sha256 清单为同名 .sha256）"

# 轮转：只匹配本脚本命名，按时间排序，超出 KEEP 删最旧（连带 .sha256）
ls -1t "${BACKUP_DIR}"/dockdeck-backup-*.tar.gz 2>/dev/null | tail -n +$((KEEP + 1)) | while read -r old; do
  rm -f "$old" "${old}.sha256"
  log "轮转删除超限旧档：$(basename "$old")"
done
log "✅ 备份完成"
