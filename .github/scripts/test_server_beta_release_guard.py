from __future__ import annotations

import importlib.util
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("server_beta_release_guard.py")
SPEC = importlib.util.spec_from_file_location("server_beta_release_guard", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
guard = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(guard)


class VersionTests(unittest.TestCase):
    def test_accepts_plain_and_prefixed_semver(self) -> None:
        self.assertEqual(guard.normalize_version("1.2.3"), ("1.2.3", "server-v1.2.3"))
        self.assertEqual(guard.normalize_version(" v10.20.30 "), ("10.20.30", "server-v10.20.30"))

    def test_rejects_player_tag_prerelease_and_leading_zero(self) -> None:
        for value in ("server-v1.2.3", "1.2", "1.2.3-beta", "01.2.3", "v1.02.3", ""):
            with self.subTest(value=value), self.assertRaises(ValueError):
                guard.normalize_version(value)


class WorkflowTests(unittest.TestCase):
    def test_repository_workflow_obeys_server_only_contract(self) -> None:
        self.assertEqual(guard.verify_workflow(), [])

    def test_detects_player_asset_regression(self) -> None:
        source = guard.DEFAULT_WORKFLOW.read_text(encoding="utf-8")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "workflow.yml"
            path.write_text(source + "\n# player/dist/forbidden.zip\n", encoding="utf-8")
            self.assertIn("Player asset path", guard.verify_workflow(path))

    def test_detects_unpinned_linter_and_missing_release_title_check(self) -> None:
        source = guard.DEFAULT_WORKFLOW.read_text(encoding="utf-8")
        source = source.replace("version: v2.4.0", "version: latest")
        source = source.replace("--json tagName,name,isPrerelease,isDraft", "--json tagName,isPrerelease,isDraft")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "workflow.yml"
            path.write_text(source, encoding="utf-8")
            failures = guard.verify_workflow(path)
            self.assertIn("lint action supports v2 and version is pinned", failures)
            self.assertIn("release identity and title are checked", failures)

    def test_detects_lint_action_without_v2_support(self) -> None:
        source = guard.DEFAULT_WORKFLOW.read_text(encoding="utf-8")
        source = source.replace("golangci/golangci-lint-action@v7", "golangci/golangci-lint-action@v6")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "workflow.yml"
            path.write_text(source, encoding="utf-8")
            self.assertIn("lint action supports v2 and version is pinned", guard.verify_workflow(path))

    def test_detects_missing_standalone_timezone_guard(self) -> None:
        source = guard.DEFAULT_WORKFLOW.read_text(encoding="utf-8")
        source = source.replace("go list -deps -tags webui ./cmd/server | grep -Fxq 'time/tzdata'", "")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "workflow.yml"
            path.write_text(source, encoding="utf-8")
            self.assertIn("standalone timezone database is enforced", guard.verify_workflow(path))

    def test_detects_missing_linker_build_identity(self) -> None:
        source = guard.DEFAULT_WORKFLOW.read_text(encoding="utf-8")
        source = source.replace(
            "-X=github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo.Version=${VERSION}",
            "-X=github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo.Version=dev",
        )
        source = source.replace(
            "-X=github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo.Commit=${GITHUB_SHA}",
            "",
        )
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "workflow.yml"
            path.write_text(source, encoding="utf-8")
            failures = guard.verify_workflow(path)
            self.assertIn("strict build version is injected", failures)
            self.assertIn("build commit is injected", failures)
            self.assertIn("development build identity in release", failures)

    def test_detects_old_monorepo_module_path(self) -> None:
        source = guard.DEFAULT_WORKFLOW.read_text(encoding="utf-8")
        source = source.replace(
            "github.com/yuanjing-hash/OhMyCine-Server",
            "github.com/yuanjing-hash/ohmycine/server",
        )
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "workflow.yml"
            path.write_text(source, encoding="utf-8")
            self.assertIn("repository is Server-only", guard.verify_workflow(path))


if __name__ == "__main__":
    unittest.main()
