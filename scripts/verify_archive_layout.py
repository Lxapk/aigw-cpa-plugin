#!/usr/bin/env python3
"""校验插件 zip 的内部结构是否符合 CPA 插件商店的要求。

为什么需要这个脚本
==================

商店在安装前会逐条校验归档内容
（internal/pluginstore/install.go readTargetLibrary）：

    if cleanedName != targetName && cleanedName != versionedTargetName {
        if path.Base(cleanedName) == targetName {
            return "target dynamic library must be at zip root"
        }
        return "dynamic library filename must be ..."
    }
    if target != nil {
        return "zip contains multiple target dynamic libraries"
    }

归纳起来是三条硬约束：

1. 归档里的动态库必须位于**根**，条目名不得带目录前缀；
2. 文件名必须是 ``<id>.so`` 或 ``<id>-v<版本>.so``；
3. **只能有一个**动态库条目。

违反第 1 条就是用户看到的 "target dynamic library must be at zip root"。
这类问题只在安装时暴露，所以放在构建流水线里提前拦截。

注意：宿主推荐的 ``plugins/<GOOS>/<GOARCH>/`` 目录布局是**安装之后**
由宿主落盘时决定的（installTargetPath），与 zip 内部结构无关，
因此归档必须保持根目录下的单一文件。
"""
from __future__ import annotations

import argparse
import sys
import zipfile
from pathlib import Path

# 与 internal/pluginstore 的 pluginExtension 对应。
DYNAMIC_LIBRARY_SUFFIXES = (".so", ".dylib", ".dll")


def verify(archive: Path, plugin_id: str, version: str) -> list[str]:
    """返回问题列表；空列表表示通过。"""
    problems: list[str] = []

    if not archive.is_file():
        return [f"归档不存在: {archive}"]

    with zipfile.ZipFile(archive) as zf:
        entries = [info for info in zf.infolist() if not info.is_dir()]

    library_names = [
        info.filename
        for info in entries
        if info.filename.endswith(DYNAMIC_LIBRARY_SUFFIXES)
    ]

    # 约束 3：只能有一个动态库。
    if len(library_names) > 1:
        problems.append(
            f"归档包含 {len(library_names)} 个动态库 {library_names!r}；"
            "商店会报 \"zip contains multiple target dynamic libraries\""
        )

    # 约束 2：文件名必须受支持。
    accepted = {f"{plugin_id}.so", f"{plugin_id}-v{version}.so"}
    for name in library_names:
        if "/" in name:
            # 约束 1：带目录前缀。
            if Path(name).name in accepted:
                problems.append(
                    f"动态库位于子目录: {name!r}；"
                    "商店会报 \"target dynamic library must be at zip root\""
                )
            else:
                problems.append(f"动态库位于子目录且文件名不符: {name!r}")
        elif name not in accepted:
            problems.append(
                f"动态库文件名 {name!r} 不被接受；应为 {sorted(accepted)!r}"
            )

    # 约束 1 的补充：即使文件名正确，也不允许存在任何目录前缀。
    for info in entries:
        if "/" in info.filename:
            problems.append(f"归档条目带目录前缀: {info.filename!r}")

    if not library_names:
        problems.append(
            f"归档中没有动态库（期望 {plugin_id}.so 或 {plugin_id}-v{version}.so）"
        )

    return problems


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dir", default="release", help="归档所在目录")
    parser.add_argument("--id", default="workbuddy", help="插件 id")
    parser.add_argument("--version", required=True, help="版本号，如 0.13.25")
    parser.add_argument("--platform", default="linux_amd64", help="归档名中的平台段")
    args = parser.parse_args()

    archive = Path(args.dir) / f"{args.id}_{args.version}_{args.platform}.zip"
    problems = verify(archive, args.id, args.version)

    if problems:
        print(f"{archive} 的归档结构不符合商店要求:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        return 1

    with zipfile.ZipFile(archive) as zf:
        names = [i.filename for i in zf.infolist() if not i.is_dir()]
    print(f"{archive}: 结构 OK ({names})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
