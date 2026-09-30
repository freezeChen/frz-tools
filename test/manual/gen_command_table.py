#!/usr/bin/env python3
"""从 `opsctl --help` 生成 docs/manual.md 附录 A.1 / A.2 的命令速查表。

用法（在仓库根执行）：

    make build
    python3 test/manual/gen_command_table.py            # 打印到标准输出
    python3 test/manual/gen_command_table.py --write    # 就地更新 docs/manual.md

`--write` 只替换 `### A.1` 到 `### A.3` 之间的内容，A.3 全局旗标是手写的、不在这里生成。
默认读 output/opsctl，可用 --binary 指到别处。
"""

import argparse
import re
import subprocess
import sys

SKIP = {"help", "completion"}  # cobra 自带，不是本工具的领域命令


def run(binary, path):
    proc = subprocess.run([binary, *path, "--help"], capture_output=True, text=True)
    if proc.returncode != 0 and not proc.stdout:
        raise SystemExit("命令 %s 的 --help 执行失败：%s" % (" ".join(path), proc.stderr.strip()))
    return proc.stdout


def section(text, header):
    """抓取 "header:" 之后、下一个空行之前的内容。"""
    out, on = [], False
    for line in text.splitlines():
        if line.strip() == header:
            on = True
            continue
        if on:
            if not line.strip():
                break
            out.append(line)
    return out


def children(text):
    result = []
    for line in section(text, "可用命令:"):
        match = re.match(r"^  ([a-z][a-z0-9-]*)\s+(.*)$", line)
        if match and match.group(1) not in SKIP:
            result.append((match.group(1), match.group(2).strip()))
    return result


def use_line(text):
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("opsctl ") and not stripped.startswith("opsctl [command]"):
            return stripped[len("opsctl "):].strip()
    return ""


def flag_names(text):
    names = []
    for line in section(text, "选项:"):
        stripped = line.strip()
        if not stripped.startswith("-"):
            continue
        match = re.match(r"^(-[a-zA-Z], )?(--[a-zA-Z0-9-]+)", stripped)
        if match and match.group(2) != "--help":
            names.append(match.group(2))
    return names


def walk(binary, path):
    rows = []
    for name, short in children(run(binary, path)):
        child = path + [name]
        text = run(binary, child)
        rows.append({
            "path": " ".join(child),
            "use": use_line(text),
            "short": short,
            "flags": flag_names(text),
            "has_children": bool(children(text)),
        })
        rows += walk(binary, child)
    return rows


def render(rows):
    leaves = [r for r in rows if not r["has_children"]]
    groups = [r for r in rows if r["has_children"]]
    out = ["### A.1 全部命令", "", "| 命令 | 说明 |", "| --- | --- |"]
    for r in groups:
        out.append("| `opsctl %s` | **分组**：%s |" % (r["path"], r["short"]))
    out.append("| | |")
    for r in leaves:
        out.append("| `opsctl %s` | %s |" % (r["use"] or r["path"], r["short"]))
    out += [
        "",
        "另有 cobra 提供的 `opsctl completion <bash\\|zsh\\|fish\\|powershell>`（生成补全脚本）"
        "与 `opsctl help [命令]`（查看帮助），不在上表。",
        "",
        "### A.2 各命令自有旗标",
        "",
        "只列旗标名，取值与语义看 `opsctl <命令> --help`；全局旗标见 A.3。",
        "",
        "| 命令 | 自有旗标 |",
        "| --- | --- |",
    ]
    for r in leaves:
        flags = " ".join("`%s`" % f for f in r["flags"]) or "—"
        out.append("| `opsctl %s` | %s |" % (r["path"], flags))
    out += [
        "",
        "`opsctl app`、`opsctl backup policy`、`opsctl app slot`、`opsctl app target` 等分组命令自身没有旗标。",
    ]
    return "\n".join(out) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="output/opsctl", help="opsctl 二进制路径（默认 output/opsctl）")
    parser.add_argument("--doc", default="docs/manual.md", help="要更新的手册路径（默认 docs/manual.md）")
    parser.add_argument("--write", action="store_true", help="就地替换手册里 A.1–A.2 之间的内容")
    args = parser.parse_args()

    table = render(walk(args.binary, []))
    if not args.write:
        sys.stdout.write(table)
        return

    with open(args.doc, encoding="utf-8") as handle:
        text = handle.read()
    start = text.index("### A.1")
    end = text.index("### A.3")
    with open(args.doc, "w", encoding="utf-8") as handle:
        handle.write(text[:start] + table + "\n" + text[end:])
    print("已更新 %s（A.1 / A.2）" % args.doc)


if __name__ == "__main__":
    main()
