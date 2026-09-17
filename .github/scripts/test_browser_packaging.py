"""Static packaging contract: these checks do not install or launch a browser."""
from pathlib import Path
import json
import unittest

ROOT = Path(__file__).resolve().parents[2]


class BrowserPackagingTests(unittest.TestCase):
    def test_docker_packages_wrapper_without_browser_install(self):
        docker = (ROOT / "Dockerfile").read_text(encoding="utf-8")
        self.assertIn("FROM node:22-bookworm-slim AS runtime", docker)
        self.assertIn("npm ci --omit=dev --ignore-scripts", docker)
        self.assertIn("USER 65532:65532", docker)
        self.assertIn("OMC_CLOAK_COMPANION=/opt/ohmycine/browser-companion/src/main.mjs", docker)
        for forbidden in ("playwright install", "cloakbrowser install", "--no-sandbox", "licenseAccepted"):
            self.assertNotIn(forbidden, docker)

    def test_compose_keeps_browser_private(self):
        compose = (ROOT / "deploy/compose.server.yml").read_text(encoding="utf-8")
        self.assertIn("init: true", compose)
        self.assertIn('shm_size: "256mb"', compose)
        self.assertNotIn("19876:", compose)
        self.assertNotIn("privileged:", compose)

    def test_archives_preserve_source_with_honest_prerequisite(self):
        workflow = (ROOT / ".github/workflows/server-beta-release.yml").read_text(encoding="utf-8")
        self.assertIn('for server_dir in "$windows_dir" "$linux_dir" "$linux_arm64_dir"', workflow)
        self.assertIn('cp -R browser-companion/src "${server_dir}/browser-companion/"', workflow)
        self.assertIn("BROWSER-SETUP.md", workflow)
        self.assertNotIn("cp -R browser-companion/node_modules", workflow)
        doc = (ROOT / "docs/deployment/browser-companion.md").read_text(encoding="utf-8")
        self.assertIn("不会同步 companion", doc)
        self.assertIn("不含 Node.js", doc)

    def test_windows_start_does_not_download_browser(self):
        launcher = (ROOT / "start.ps1").read_text(encoding="utf-8")
        self.assertIn("@('ci', '--omit=dev', '--ignore-scripts')", launcher)
        self.assertIn("browser-companion\\src\\main.mjs", launcher)
        self.assertNotIn("licenseAccepted", launcher)

    def test_wrapper_lock_is_cross_architecture_and_archive_stays_bounded(self):
        lock = json.loads((ROOT / "browser-companion/package-lock.json").read_text(encoding="utf-8"))
        for name, package in lock["packages"].items():
            self.assertFalse(package.get("hasInstallScript"), name)
            self.assertFalse(package.get("os"), name)
            self.assertFalse(package.get("cpu"), name)
        source_entries = list((ROOT / "browser-companion/src").rglob("*"))
        # Existing updater accepts at most 128 archive entries, including dirs.
        self.assertLess(len(source_entries) + 10, 128)
        self.assertFalse(any(entry.is_symlink() for entry in source_entries))


if __name__ == "__main__":
    unittest.main()
