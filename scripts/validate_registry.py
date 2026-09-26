#!/usr/bin/env python3
"""Validate registry.json before committing.

Written after a placeholder sha256 ("PENDING") was pushed and broke the plugin
store with "invalid sha256 length". A placeholder is worse than a stale value:
the store rejects it outright, whereas a stale value merely fails the checksum
of one artifact.

Checks:
  * schema_version and the plugins array exist
  * each artifact has a 64-character lowercase hex sha256
  * each artifact URL points at the version the plugin declares
"""
import json
import re
import sys

HEX64 = re.compile(r"^[0-9a-f]{64}$")


def main(path: str = "registry.json") -> int:
    with open(path, encoding="utf-8") as handle:
        doc = json.load(handle)

    problems: list[str] = []
    if not doc.get("schema_version"):
        problems.append("schema_version is missing")

    plugins = doc.get("plugins") or []
    if not plugins:
        problems.append("plugins array is empty")

    for index, plugin in enumerate(plugins):
        version = str(plugin.get("version") or "")
        if not version:
            problems.append(f"plugins[{index}]: version is missing")
        for a_index, artifact in enumerate(plugin.get("install", {}).get("artifacts") or []):
            where = f"plugins[{index}]: artifacts[{a_index}]"
            digest = str(artifact.get("sha256") or "")
            if not HEX64.match(digest):
                # Distinguish a placeholder from a malformed value so the
                # message points at the fix.
                if digest.isalpha() and digest.isupper():
                    problems.append(f"{where}: sha256 is a placeholder ({digest!r}); "
                                    f"publish only after the CI artifact exists")
                else:
                    problems.append(f"{where}: invalid sha256 length ({len(digest)}), want 64 hex")
            url = str(artifact.get("url") or "")
            if version and version not in url:
                problems.append(f"{where}: url does not mention version {version}")

    if problems:
        print("registry.json is not publishable:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        return 1

    print("registry.json OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(*sys.argv[1:]))
