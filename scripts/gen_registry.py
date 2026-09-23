#!/usr/bin/env python3
"""生成 CLIProxyAPI 插件商店用的 registry.json。

CPA 读 `plugins.store-sources` 里列的 registry.json 来发现可安装插件。
本脚本根据插件元信息生成符合 SchemaVersion 2 的清单（支持 `github-release`
与 `direct` 两种安装类型）。

用法：
  python3 scripts/gen_registry.py \
      --id aigw-reverse-proxy \
      --name "AIGW 反向代理" \
      --version 0.1.0 \
      --repo https://github.com/<owner>/<repo> \
      --author TaiXu \
      --description "从 AI 聚合网关 0.1.18 移植的反向代理网关" \
      --out registry.json
"""

import argparse
import json
import sys

SCHEMA_VERSION_V2 = 2


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--id", required=True)
    ap.add_argument("--name", required=True)
    ap.add_argument("--version", required=True)
    ap.add_argument("--repo", required=True,
                    help="必须是完整 URL：https://github.com/{owner}/{repo}")
    ap.add_argument("--author", default="")
    ap.add_argument("--description", default="")
    ap.add_argument("--license", default="MIT")
    ap.add_argument("--homepage", default="")
    ap.add_argument("--out", default="registry.json")
    args = ap.parse_args()

    # CPA 的 GitHubRepositoryParts 严格要求这种形态，提前拦住写错的输入。
    if not args.repo.startswith("https://github.com/"):
        raise SystemExit("--repo 必须是 https://github.com/{owner}/{repo}")
    parts = args.repo[len("https://github.com/"):].strip("/").split("/")
    if len(parts) != 2 or not all(parts):
        raise SystemExit("--repo 必须是 https://github.com/{owner}/{repo}")
    if parts[1].endswith(".git"):
        raise SystemExit("--repo 不能带 .git 后缀")

    plugin = {
        "id": args.id,
        "name": args.name,
        "description": args.description,
        "author": args.author,
        "version": args.version,
        "repository": args.repo,
        "license": args.license,
        # github-release 类型：artifact 从仓库 Release 里按
        # <id>_<version>_<goos>_<goarch>.zip 自动匹配，无需在此声明。
        "install": {"type": "github-release"},
    }
    if args.homepage:
        plugin["homepage"] = args.homepage

    registry = {"schema_version": SCHEMA_VERSION_V2, "plugins": [plugin]}

    with open(args.out, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(registry, fh, ensure_ascii=False, indent=2)
        fh.write("\n")

    print(f"  ✓ {args.out}")
    print(json.dumps(registry, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
