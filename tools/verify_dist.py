#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""dockdeck 产物校验器（make verify-dist）：架构 / 大小 / 静态性 / 版本注入。

背景：`make dist` 出 hub/agent × linux/amd64、linux/arm64 四件套
（bin/dockdeck-<cmd>-<os>-<arch>）。本校验器在推送/发布前对产物做机检，
stdlib-only、无第三方依赖；直接解析 ELF 头，不依赖 file/binutils
（构建沙箱里两者皆无，elfcheck 的历史做法照搬至此并产品化）。

检查项（每个产物 5 项）：
  1. ELF 头     ：magic + ELFCLASS64（64 位）——不是 ELF 或 32 位即失败；
  2. 架构对账   ：e_machine 与文件名架构一致（0x3E x86-64 ↔ amd64；
                  0xB7 aarch64 ↔ arm64）——交叉编译串台最常见的静默失败；
  3. 静态性     ：无 PT_INTERP 程序头 → 无动态链接器 → 纯静态
                  （构建侧 CGO_ENABLED=0 + netgo/osusergo 的二进制侧印证；
                  静态 PIE（e_type=ET_DYN）同样无 PT_INTERP，予以放行）；
  4. 大小预算   ：hub ≤ 24MiB、agent ≤ 16MiB——当前实际约 2 倍余量
                  （hub 11.1–11.7MiB / agent 7.0–7.5MiB），挡依赖回归膨胀；
  5. 版本注入   ：COMMIT 短串（ldflags -X internal/version.Commit）在产物
                  字节内可检——漏注入时 `version` 包回退 unknown/dev，
                  运行期无法回嘴自己是哪版。commit 取不到（无 .git）时
                  跳过本项并警告，不算失败。

用法：
    python3 tools/verify_dist.py [--bin DIR] [--commit SHORT_SHA] [--quiet]

    --bin     产物目录，默认取仓库根 bin/（本脚本位于 tools/ 下）；
    --commit  期望注入的短 commit，默认 `git rev-parse --short HEAD`；
    --quiet   只输出失败项与汇总行。

退出码：0 全过；1 有失败项；2 用法/环境错（如产物目录不存在）。
"""

import argparse
import struct
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent

# --- ELF 常量（64 位小端，Go 产出均为 LE）---
ELFCLASS64 = 2
ET_EXEC, ET_DYN = 2, 3
PT_INTERP = 3
EM_X86_64, EM_AARCH64 = 0x3E, 0xB7

MIB = 1024 * 1024

# 期望矩阵：(cmd, os, arch, e_machine, 大小预算字节)
MATRIX = [
    ("hub", "linux", "amd64", EM_X86_64, 24 * MIB),
    ("hub", "linux", "arm64", EM_AARCH64, 24 * MIB),
    ("agent", "linux", "amd64", EM_X86_64, 16 * MIB),
    ("agent", "linux", "arm64", EM_AARCH64, 16 * MIB),
]

ARCH_NAME = {EM_X86_64: "x86-64", EM_AARCH64: "aarch64"}
ETYPE_NAME = {ET_EXEC: "exec", ET_DYN: "dyn(static-pie)"}


def parse_elf(data: bytes) -> dict:
    """解析 64 位 ELF 头与程序头表入口，失败抛 ValueError（文案即失败原因）。"""
    if len(data) < 64:
        raise ValueError("文件过小，不是合法 ELF")
    if data[:4] != b"\x7fELF":
        raise ValueError("ELF magic 不符")
    ei_class = data[4]
    if ei_class != ELFCLASS64:
        raise ValueError("ELFCLASS=%d，期望 %d（64 位）" % (ei_class, ELFCLASS64))
    e_type, e_machine = struct.unpack_from("<HH", data, 16)
    e_phoff = struct.unpack_from("<Q", data, 32)[0]
    e_phentsize, e_phnum = struct.unpack_from("<HH", data, 54)
    if e_phentsize < 4 or e_phnum > 4096:
        raise ValueError("程序头表参数异常（phentsize=%d phnum=%d）" % (e_phentsize, e_phnum))
    return {
        "e_type": e_type,
        "e_machine": e_machine,
        "e_phoff": e_phoff,
        "e_phentsize": e_phentsize,
        "e_phnum": e_phnum,
    }


def has_pt_interp(data: bytes, hdr: dict) -> bool:
    off = hdr["e_phoff"]
    for i in range(hdr["e_phnum"]):
        p_type = struct.unpack_from("<I", data, off + i * hdr["e_phentsize"])[0]
        if p_type == PT_INTERP:
            return True
    return False


def resolve_commit(arg: str | None) -> str | None:
    if arg:
        return arg
    try:
        out = subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            cwd=REPO_ROOT, capture_output=True, text=True, timeout=15,
        )
        if out.returncode == 0 and out.stdout.strip():
            return out.stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        pass
    return None


def main() -> int:
    ap = argparse.ArgumentParser(description="dockdeck 产物校验：架构/大小/静态性/版本注入")
    ap.add_argument("--bin", default=str(REPO_ROOT / "bin"), help="产物目录（默认仓库根 bin/）")
    ap.add_argument("--commit", default=None, help="期望注入的短 commit（默认 git rev-parse --short HEAD）")
    ap.add_argument("--quiet", action="store_true", help="只输出失败项与汇总")
    args = ap.parse_args()

    bindir = Path(args.bin)
    if not bindir.is_dir():
        print("FAIL: 产物目录不存在：%s（先 make dist）" % bindir, file=sys.stderr)
        return 2

    commit = resolve_commit(args.commit)
    if commit is None:
        print("WARN: 取不到 commit（无 .git？），跳过「版本注入」检查项", file=sys.stderr)
    elif commit in ("dev", "unknown"):
        print("WARN: commit=%r 是构建兜底值，跳过「版本注入」检查项" % commit, file=sys.stderr)
        commit = None

    print("产物校验：目录=%s commit=%s" % (bindir, commit or "(跳过注入检查)"))

    failures = []   # (产物, 检查项, 原因)
    rows = []       # (产物, elf, 架构, 静态, 大小, 注入)
    total_checks = 0

    for cmd, os_, arch, machine, budget in MATRIX:
        name = "dockdeck-%s-%s-%s" % (cmd, os_, arch)
        path = bindir / name
        row = [name, "-", "-", "-", "-", "-"]
        rows.append(row)

        if not path.is_file():
            failures.append((name, "存在性", "文件缺失（先 make dist）"))
            continue

        data = path.read_bytes()
        size = len(data)
        total_checks += 1

        # 1. ELF 头
        try:
            hdr = parse_elf(data)
        except ValueError as e:
            failures.append((name, "ELF 头", str(e)))
            continue
        row[1] = "elf64"
        total_checks += 1

        # 2. 架构对账
        total_checks += 1
        if hdr["e_machine"] != machine:
            failures.append((name, "架构对账",
                             "e_machine=0x%X（%s），与文件名 arch=%s 不符"
                             % (hdr["e_machine"], ARCH_NAME.get(hdr["e_machine"], "?"), arch)))
            row[2] = "✗"
        else:
            row[2] = ARCH_NAME[machine] + "✓"

        # 3. 静态性（无 PT_INTERP）
        total_checks += 1
        if has_pt_interp(data, hdr):
            failures.append((name, "静态性", "检出 PT_INTERP：产物非纯静态（CGO_ENABLED=0 漏了？）"))
            row[3] = "✗动态"
        else:
            row[3] = "静态✓(%s)" % ETYPE_NAME.get(hdr["e_type"], "?")

        # 4. 大小预算
        total_checks += 1
        if size > budget:
            failures.append((name, "大小预算",
                             "%.2fMiB 超预算 %.0fMiB（依赖回归膨胀？）" % (size / MIB, budget / MIB)))
            row[4] = "%.1fMiB✗/%.0f" % (size / MIB, budget / MIB)
        else:
            row[4] = "%.1fMiB✓/%.0f" % (size / MIB, budget / MIB)

        # 5. 版本注入
        if commit is not None:
            total_checks += 1
            if commit.encode() in data:
                row[5] = "commit✓"
            else:
                failures.append((name, "版本注入", "产物内检不出 commit 串 %r（ldflags 漏注入？）" % commit))
                row[5] = "✗"
        else:
            row[5] = "-"

    if not args.quiet:
        print("%-30s %-6s %-16s %-18s %-16s %s" % ("产物", "ELF", "架构", "静态性", "大小/预算", "注入"))
        for r in rows:
            print("%-30s %-6s %-16s %-18s %-16s %s" % tuple(r))

    if failures:
        print("\n失败 %d 项：" % len(failures))
        for name, item, why in failures:
            print("  [%s] %s：%s" % (name, item, why))
        print("FAIL：%d 产物共 %d 项检查，%d 项失败" % (len(MATRIX), total_checks, len(failures)))
        return 1

    print("PASS：%d 产物共 %d 项检查全部通过" % (len(MATRIX), total_checks))
    return 0


if __name__ == "__main__":
    sys.exit(main())
