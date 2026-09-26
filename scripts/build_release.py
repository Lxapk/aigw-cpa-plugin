#!/usr/bin/env python3
"""打包 CLIProxyAPI 插件商店所需的 Release 资产。

CPA 的 plugin store 对 `github-release` 安装类型有严格约定：

  资产名必须是   <id>_<version>_<goos>_<goarch>.zip
  必须附带       checksums.txt            （sha256，两列：hash + 文件名）
  zip 内必须含   <id>.so（或 <id>-v<version>.so），且位于 zip 根目录
  zip 内不能含   其它动态库（否则报 "multiple target dynamic libraries"）

用法：
  python3 build_release.py --id aigw-reverse-proxy --version 0.1.0 \
      --arm64 dist/aigw-reverse-proxy.so \
      --amd64 dist/aigw-reverse-proxy-amd64.so \
      --out release
"""

import argparse
import hashlib
import os
import sys
import zipfile


def build_zip(plugin_id: str, so_path: str, out_path: str, goarch: str) -> str:
    """把单个 .so 打进 zip。

    包内同时提供两种布局，对应宿主的两条搜索路径
    （docs/examples/plugin/simple/README_CN.md「发现规则」）：

        plugins/linux/<GOARCH>/<id>.so   —— 官方推荐的位置
        <id>.so                          —— 放在 plugins 根目录时可用

    包含子目录条目是为了让「解压到 plugins/ 即可」这条路径直接得到官方
    推荐的布局，而不必手工创建 linux/<GOARCH> 目录。
    """
    if not os.path.isfile(so_path):
        raise SystemExit(f"找不到 .so: {so_path}")

    with open(so_path, "rb") as fh:
        data = fh.read()

    entries = [
        f"{plugin_id}.so",
        f"linux/{goarch}/{plugin_id}.so",
    ]
    with zipfile.ZipFile(out_path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
        for inner_name in entries:
            # 固定时间戳让产物可复现；writestr 保证条目名不被进一步改写。
            info = zipfile.ZipInfo(inner_name, date_time=(1980, 1, 1, 0, 0, 0))
            info.external_attr = 0o755 << 16  # 保留可执行权限
            info.compress_type = zipfile.ZIP_DEFLATED
            zf.writestr(info, data)

    return sha256_file(out_path)


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--id", required=True, help="插件 id（须与 .so 的注册 id 一致）")
    ap.add_argument("--version", required=True, help="版本号，如 0.1.0")
    ap.add_argument("--arm64", help="linux/arm64 的 .so 路径")
    ap.add_argument("--amd64", help="linux/amd64 的 .so 路径")
    ap.add_argument("--out", default="release", help="输出目录")
    args = ap.parse_args()

    if not args.arm64 and not args.amd64:
        raise SystemExit("至少需要 --arm64 或 --amd64 之一")

    os.makedirs(args.out, exist_ok=True)

    targets = []
    if args.arm64:
        targets.append(("arm64", args.arm64))
    if args.amd64:
        targets.append(("amd64", args.amd64))

    checksums = []
    for goarch, so_path in targets:
        asset_name = f"{args.id}_{args.version}_linux_{goarch}.zip"
        out_path = os.path.join(args.out, asset_name)
        digest = build_zip(args.id, so_path, out_path, goarch)
        size = os.path.getsize(out_path)
        checksums.append((digest, asset_name))
        print(f"  ✓ {asset_name}  ({size:,} bytes)")

    checksum_path = os.path.join(args.out, "checksums.txt")
    with open(checksum_path, "w", encoding="utf-8", newline="\n") as fh:
        for digest, name in checksums:
            fh.write(f"{digest}  {name}\n")
    print(f"  ✓ checksums.txt")

    print()
    print("Release 资产（全部上传到 GitHub Release）：")
    for name in sorted(os.listdir(args.out)):
        print(f"  {name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
