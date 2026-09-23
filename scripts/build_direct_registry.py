#!/usr/bin/env python3
"""生成使用 `direct` 安装类型的 registry.json。

用途
----
`github-release` 类型要求 CPA 调用
    GET /repos/{owner}/{repo}/releases/tags/{tag}
并解析其内嵌的 assets 字段。实测该端点在某些情况下会返回 0 个资产
（而 /releases/latest 与 /releases/{id}/assets 均正常），此时 CPA 报
    release asset <name> not found
且无法绕过。

`direct` 类型完全不走 GitHub API：registry 里直接列出每个平台的
下载 URL 与 SHA-256，CPA 拉取后校验即可。

权衡
----
* 优点：不依赖 GitHub API，不受索引异常影响
* 缺点：URL 指向具体 tag，发新版本需要重新生成 registry（CI 已自动化）

用法
----
  python3 build_direct_registry.py \
      --id aigw-reverse-proxy \
      --name "AIGW 反向代理" \
      --version 0.8.1 \
      --repo https://github.com/<owner>/<repo> \
      --tag v0.8.1 \
      --checksums checksums.txt \
      --out registry.json
"""

import argparse
import json
import re
import sys

SCHEMA_VERSION_V2 = 2


def parse_checksums(path: str) -> dict[str, str]:
    """读取 sha256sum 格式的校验和文件。"""
    out: dict[str, str] = {}
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            parts = line.split()
            if len(parts) >= 2:
                # 兼容 "hash  name" 与 "hash *name"
                out[parts[-1].lstrip("*")] = parts[0].lower()
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--id", required=True)
    ap.add_argument("--name", required=True)
    ap.add_argument("--version", required=True)
    ap.add_argument("--repo", required=True,
                    help="https://github.com/{owner}/{repo}")
    ap.add_argument("--tag", required=True, help="例如 v0.8.1")
    ap.add_argument("--checksums", required=True, help="checksums.txt 路径")
    ap.add_argument("--description", default="")
    ap.add_argument("--author", default="")
    ap.add_argument("--platforms", default="linux/amd64,linux/arm64")
    ap.add_argument("--out", default="registry.json")
    args = ap.parse_args()

    if not args.repo.startswith("https://github.com/"):
        raise SystemExit("--repo 必须是 https://github.com/{owner}/{repo}")
    parts = args.repo[len("https://github.com/"):].strip("/").split("/")
    if len(parts) != 2 or not all(parts):
        raise SystemExit("--repo 必须是 https://github.com/{owner}/{repo}")

    checksums = parse_checksums(args.checksums)
    if not checksums:
        raise SystemExit(f"校验和文件为空：{args.checksums}")

    artifacts = []
    for platform in args.platforms.split(","):
        platform = platform.strip()
        if not platform or "/" not in platform:
            continue
        goos, goarch = platform.split("/", 1)
        asset = f"{args.id}_{args.version}_{goos}_{goarch}.zip"
        digest = checksums.get(asset)
        if not digest:
            raise SystemExit(f"校验和文件里没有 {asset}")
        if not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise SystemExit(f"{asset} 的 sha256 非法：{digest}")
        artifacts.append({
            "goos": goos,
            "goarch": goarch,
            "url": f"{args.repo}/releases/download/{args.tag}/{asset}",
            "sha256": digest,
        })

    plugin = {
        "id": args.id,
        "name": args.name,
        "description": args.description,
        "author": args.author,
        "version": args.version,
        "repository": args.repo,
        "license": "MIT",
        # direct 类型必须显式列出每个平台的下载地址与校验和。
        "install": {"type": "direct", "artifacts": artifacts},
    }

    registry = {"schema_version": SCHEMA_VERSION_V2, "plugins": [plugin]}
    with open(args.out, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(registry, fh, ensure_ascii=False, indent=2)
        fh.write("\n")

    print(f"  ✓ {args.out}（direct 类型，{len(artifacts)} 个平台）")
    for a in artifacts:
        print(f"    {a['goos']}/{a['goarch']}  {a['sha256'][:16]}…  {a['url']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
