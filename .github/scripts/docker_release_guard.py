"""Do not let a delayed/retried Docker workflow promote an older Beta."""
from __future__ import annotations

import json
import re
import sys


def may_promote(version: str, pages: list[list[dict]], channel: str = "beta") -> bool:
    if channel not in ("beta", "stable"):
        raise ValueError("invalid release channel")
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version):
        raise ValueError("invalid Server version")
    candidate = tuple(map(int, version.split(".")))
    versions = []
    for page in pages:
        for release in page:
            tag = release.get("tag_name", "")
            match = re.fullmatch(r"server-v((0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))", tag)
            if match and release.get("prerelease") is (channel == "beta") and release.get("draft") is False:
                versions.append(tuple(map(int, match[1].split("."))))
    return bool(versions) and candidate == max(versions)


if __name__ == "__main__":
    print(str(may_promote(sys.argv[1], json.load(sys.stdin), sys.argv[2] if len(sys.argv) > 2 else "beta")).lower())
