#!/usr/bin/env python3
"""校验 THIRD-PARTY-NOTICES.md 与依赖清单一致。

规则：go.mod 里的每个模块、web/package.json 的每个 runtime 依赖，都必须在
THIRD-PARTY-NOTICES.md 中出现；任何一项缺失即失败（exit 1）。
仅用标准库，任何环境可跑。
"""
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
NOTICE = os.path.join(ROOT, "THIRD-PARTY-NOTICES.md")


def go_modules():
    path = os.path.join(ROOT, "go.mod")
    text = open(path, encoding="utf-8").read()
    return sorted(set(re.findall(r"^\t([a-z0-9.\-/]+)\s+v[0-9]", text, re.M)))


def js_deps():
    path = os.path.join(ROOT, "web", "package.json")
    data = json.load(open(path, encoding="utf-8"))
    return sorted(data.get("dependencies", {}).keys())


def main():
    if not os.path.exists(NOTICE):
        print("缺少 THIRD-PARTY-NOTICES.md", file=sys.stderr)
        return 1
    notice = open(NOTICE, encoding="utf-8").read()
    missing = []
    for mod in go_modules():
        if ("`%s`" % mod) not in notice and mod not in notice:
            missing.append("go: " + mod)
    for dep in js_deps():
        if ("`%s`" % dep) not in notice and dep not in notice:
            missing.append("npm: " + dep)
    if missing:
        print("THIRD-PARTY-NOTICES.md 未登记以下依赖：", file=sys.stderr)
        for m in missing:
            print("  - " + m, file=sys.stderr)
        print("新增依赖须在同一提交内更新该文件。", file=sys.stderr)
        return 1
    print("third-party notices OK（go %d / npm %d）" % (len(go_modules()), len(js_deps())))
    return 0


if __name__ == "__main__":
    sys.exit(main())
