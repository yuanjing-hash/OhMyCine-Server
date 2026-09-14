#!/usr/bin/env python3
"""Resolve and statically verify the Server-only beta release contract."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


VERSION_RE = re.compile(r"^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")
ROOT = Path(__file__).resolve().parents[2]
DEFAULT_WORKFLOW = ROOT / ".github" / "workflows" / "server-beta-release.yml"


def normalize_version(raw: str) -> tuple[str, str]:
    candidate = raw.strip()
    match = VERSION_RE.fullmatch(candidate)
    if match is None:
        raise ValueError("version must be strict semantic version X.Y.Z or vX.Y.Z")
    version = ".".join(match.groups())
    return version, f"server-v{version}"


def verify_workflow(path: Path = DEFAULT_WORKFLOW) -> list[str]:
    text = path.read_text(encoding="utf-8")
    required = {
        "manual and namespaced tag dispatch": "workflow_dispatch:" in text and "tags: ['server-v*']" in text,
        "normalized namespaced tag": "server_release_source.py" in text,
        "repository is Server-only": "player-beta-release.yml" not in text
        and "github.com/yuanjing-hash/OhMyCine-Server" in text,
        "concurrency uses normalized tag": "group: server-release-${{ needs.resolve.outputs.tag_name }}" in text,
        "develop source is checked twice": text.count("server_release_source.py") >= 2,
        "existing tags are commit checked": text.count('refs/tags/${TAG_NAME}^{commit}') >= 2,
        "missing release creates namespaced prerelease": 'gh release create "${create_args[@]}"' in text
        and "--prerelease" in text,
        "release identity and title are checked": text.count(
            "--json tagName,name,isPrerelease,isDraft"
        )
        >= 2
        and text.count("OhMyCine Server v${VERSION} Beta") >= 2,
        "official read token secret is injected once": text.count(
            "secrets.OHMYCINE_TMDB_READ_ACCESS_TOKEN"
        )
        == 1,
        "node signing secret is injected only for trust derivation and signing": text.count(
            "secrets.OHMYCINE_NODE_MANIFEST_SIGNING_PRIVATE_KEY"
        )
        == 2,
        "node signing secret is isolated to trust derivation and signing steps": (
            text.find("Derive Node release trust root")
            < text.find("secrets.OHMYCINE_NODE_MANIFEST_SIGNING_PRIVATE_KEY")
            < text.find("Build and package embedded-WebUI Server archives")
            < text.find("Sign Node manifest and render versioned installers")
            < text.rfind("secrets.OHMYCINE_NODE_MANIFEST_SIGNING_PRIVATE_KEY")
            < text.find("Revalidate latest develop and publish Server-only prerelease")
        ),
        "unsigned node release fails closed": "OHMYCINE_NODE_MANIFEST_SIGNING_PRIVATE_KEY is required; unsigned Node assets are forbidden."
        in text,
        "node signing key is RSA 3072 or stronger": "key_bits < 3072" in text
        and 'openssl dgst -sha256 -sign "$signing_key"' in text,
        "node manifest signature is verified before upload": 'openssl dgst -sha256 -verify "$public_key"'
        in text,
        "embedded webui is mandatory": text.count("go build -tags webui") == 3,
        "strict build version is injected": "internal/buildinfo.Version=${VERSION}" in text,
        "build commit is injected": "internal/buildinfo.Commit=${GITHUB_SHA}" in text,
        "windows archive is built": "windows-x64.zip" in text,
        "linux archive is built": "linux-x64.tar.gz" in text,
        "server arm64 archive is built and published": all(
            value in text for value in (
                'CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags webui',
                'linux_arm64_asset="OhMyCine-Server-v${VERSION}-linux-arm64.tar.gz"',
                'tar -czf "$linux_arm64_asset" "$(basename "$linux_arm64_dir")"',
                'sha256sum "$windows_asset" "$linux_asset" "$linux_arm64_asset"',
                'echo "LINUX_ARM64_ASSET=${release_root}/${linux_arm64_asset}"',
                'gh release upload "$TAG_NAME" "$WINDOWS_ASSET" "$LINUX_ASSET" "$LINUX_ARM64_ASSET"',
            )
        ),
        "checksum manifest is built": "SHA256SUMS.txt" in text and "sha256sum" in text,
        "node platform archives are built": all(
            asset in text
            for asset in (
                "windows-amd64.zip",
                "linux-amd64.tar.gz",
                "linux-arm64.tar.gz",
            )
        ),
        "node builds do not receive tmdb linker flags": text.count(
            'go build -trimpath -ldflags "${build_ldflags} -s -w"'
        )
        == 3,
        "node dependency boundary is release checked": "Node dependency graph contains Server WebUI or TMDB packages"
        in text,
        "versioned node installers are rendered": "install-ohmycine-node-v${VERSION}.sh" in text
        and "install-ohmycine-node-v${VERSION}.ps1" in text
        and "__OHMYCINE_NODE_RELEASE_PUBLIC_KEY_BASE64__" in text,
        "node installer contracts run before release": "bash -n scripts/install-node.sh" in text
        and "scripts/test-install-node.ps1" in text,
        "webui gates run": all(
            command in text
            for command in (
                "npm run permissions:check",
                "npm run test",
                "npm run typecheck",
                "npm run lint",
                "npm run build",
            )
        ),
        "go gates run": all(
            command in text
            for command in (
                "go mod verify",
                "go build ./...",
                "go vet ./...",
                "go test ./...",
                "golangci-lint-action",
            )
        ),
        "standalone timezone database is enforced": "go list -deps -tags webui ./cmd/server | grep -Fxq 'time/tzdata'"
        in text,
        "lint action supports v2 and version is pinned": "golangci/golangci-lint-action@v7" in text and "version: v2.4.0" in text and "version: latest" not in text,
        "idempotent asset upload": 'gh release upload "$TAG_NAME"' in text and "--clobber" in text,
        "signed node assets are uploaded": all(
            name in text
            for name in (
                '"$NODE_WINDOWS_ASSET"',
                '"$NODE_LINUX_AMD64_ASSET"',
                '"$NODE_LINUX_ARM64_ASSET"',
                '"$NODE_MANIFEST_ASSET"',
                '"$NODE_SIGNATURE_ASSET"',
                '"$NODE_INSTALLER_SH"',
                '"$NODE_INSTALLER_PS1"',
            )
        ),
        "publish follows packaging": text.find("Build and package embedded-WebUI Server archives")
        < text.find("Revalidate latest develop and publish Server-only prerelease"),
        "job write permission is scoped": "permissions:\n      contents: write" in text,
    }
    forbidden = {
        "Player release dependency": "Existing Player beta release",
        "Player tag namespace": "refs/tags/v${",
        "legacy API-key build secret": "OHMYCINE_TMDB_API_KEY",
        "development build identity in release": "internal/buildinfo.Version=dev",
        "Player asset path": "player/",
        "node signing key leaked into artifact environment": 'echo "OHMYCINE_NODE_MANIFEST_SIGNING_PRIVATE_KEY=',
    }

    failures = [label for label, passed in required.items() if not passed]
    failures.extend(label for label, token in forbidden.items() if token in text)
    return failures


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    version_parser = subparsers.add_parser("resolve-version")
    version_parser.add_argument("version")
    workflow_parser = subparsers.add_parser("check-workflow")
    workflow_parser.add_argument("path", nargs="?", type=Path, default=DEFAULT_WORKFLOW)
    args = parser.parse_args(argv)

    if args.command == "resolve-version":
        try:
            version, tag_name = normalize_version(args.version)
        except ValueError as exc:
            print(f"::error::{exc}", file=sys.stderr)
            return 2
        print(f"version={version}")
        print(f"tag_name={tag_name}")
        return 0

    failures = verify_workflow(args.path)
    if failures:
        for failure in failures:
            print(f"::error::Server beta release guard failed: {failure}", file=sys.stderr)
        return 1
    print("Server-only beta release workflow contract verified.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
