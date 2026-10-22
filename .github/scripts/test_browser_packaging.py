"""Static packaging contract: these checks do not install or launch a browser."""
from pathlib import Path
import json
import hashlib
import importlib.util
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
        self.assertIn("seccomp=./seccomp/browser.json", compose)
        for forbidden in ("SYS_ADMIN", "seccomp=unconfined", "ipc: host", "pid: host", "network_mode: host"):
            self.assertNotIn(forbidden, compose)

    def test_archives_preserve_source_with_honest_prerequisite(self):
        workflow = (ROOT / ".github/workflows/server-beta-release.yml").read_text(encoding="utf-8")
        self.assertIn('for server_dir in "$windows_dir" "$linux_dir" "$linux_arm64_dir"', workflow)
        self.assertIn('cp -R browser-companion/src "${server_dir}/browser-companion/"', workflow)
        self.assertIn("BROWSER-SETUP.md", workflow)
        self.assertIn('cp deploy/compose.server.yml "${server_dir}/deploy/"', workflow)
        self.assertIn('deploy/seccomp/browser.json deploy/seccomp/moby-default.json deploy/seccomp/upstream.json deploy/seccomp/LICENSE.moby "${server_dir}/deploy/seccomp/"', workflow)
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
        deploy_entries = list((ROOT / "deploy/seccomp").rglob("*"))
        self.assertLess(len(source_entries) + len(deploy_entries) + 16, 128)
        self.assertFalse(any(entry.is_symlink() for entry in source_entries))

    def test_pinned_complete_baseline_and_license(self):
        directory = ROOT / "deploy/seccomp"
        source = json.loads((directory / "upstream.json").read_text())
        self.assertEqual(source["commit"], "2ceae35d351c156cb5a8efc0fdc4a08cf94569d8")
        self.assertEqual(source["sha256"], "6416b47770785a41ac59073cdc77d9fe98517df2799dc83ef207e622de3053f6")
        self.assertEqual(hashlib.sha256((directory / "moby-default.json").read_bytes()).hexdigest(), source["sha256"])
        self.assertEqual(hashlib.sha256((directory / "LICENSE.moby").read_bytes()).hexdigest(), source["license_sha256"])
        self.assertEqual(source["license"], "Apache-2.0")
        baseline = json.loads((directory / "moby-default.json").read_bytes())
        profile = json.loads((directory / "browser.json").read_bytes())
        self.assertEqual(profile["defaultAction"], "SCMP_ACT_ERRNO")
        self.assertEqual(profile["defaultErrnoRet"], 1)
        self.assertEqual(profile["archMap"], baseline["archMap"])
        self.assertGreater(len(baseline["syscalls"]), 30)
        self.assertEqual(profile["syscalls"][:-2], baseline["syscalls"])
        self.assertEqual({**profile, "syscalls": baseline["syscalls"]}, baseline)

    def test_profile_is_reproducible_and_namespaces_are_constrained(self):
        spec = importlib.util.spec_from_file_location("browser_seccomp", ROOT / "scripts/build-browser-seccomp.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        profile = json.loads((ROOT / "deploy/seccomp/browser.json").read_bytes())
        self.assertEqual(profile, module.profile())
        self.assertEqual((ROOT / "deploy/seccomp/browser.json").read_bytes(), (json.dumps(module.profile(), indent=2) + "\n").encode("utf-8"))
        clone, unshare = profile["syscalls"][-2:]
        self.assertEqual(clone["names"], ["clone"])
        self.assertEqual(clone["includes"]["arches"], ["amd64", "arm64"])
        arg = clone["args"][0]
        self.assertEqual(arg["index"], 0)
        self.assertEqual(arg["op"], "SCMP_CMP_MASKED_EQ")
        for flags in (0x10000000, 0x30000000, 0x70000000, 0x20000000):
            self.assertEqual(flags & arg["value"], arg["valueTwo"])
        for forbidden in (0x00020000, 0x02000000, 0x04000000, 0x08000000):
            self.assertNotEqual((0x10000000 | forbidden) & arg["value"], arg["valueTwo"])
        self.assertEqual(unshare["names"], ["unshare"])
        self.assertEqual(unshare["args"], [{"index": 0, "value": 0x10000000, "op": "SCMP_CMP_EQ"}])
        # All baseline AF_ALG/AF_VSOCK, io_uring, bpf, ptrace and clone3 rules
        # remain byte-for-byte equivalent; only the above two additions differ.
        self.assertFalse(any("clone3" in r["names"] or "setns" in r["names"] or "socket" in r["names"] for r in module.ADDITIONS))


if __name__ == "__main__":
    unittest.main()
