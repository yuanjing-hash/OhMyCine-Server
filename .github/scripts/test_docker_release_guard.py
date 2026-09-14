import unittest
from pathlib import Path

from docker_release_guard import may_promote


class WorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        root = Path(__file__).resolve().parents[1]
        cls.workflow = (root / "workflows/docker-publish.yml").read_text(encoding="utf-8")
        cls.caller = (root / "workflows/server-beta-release.yml").read_text(encoding="utf-8")
        cls.smoke = (root / "scripts/docker_smoke.sh").read_text(encoding="utf-8")

    def test_secrets_declared_forwarded_and_not_interpolated_in_shell(self):
        for name in ("DOCKERHUB_USERNAME", "DOCKERHUB_TOKEN"):
            self.assertIn(f"{name}: {{ required: true }}", self.workflow)
            self.assertIn(name + ": ${{ secrets." + name + " }}", self.caller)
        self.assertIn('namespace="${DOCKERHUB_USERNAME,,}"', self.workflow)
        self.assertIn('[[ ! "$namespace" =~ ^[a-z0-9][a-z0-9_-]{0,127}$ ]]', self.workflow)
        self.assertIn('[[ -z "$DOCKERHUB_TOKEN" ]]', self.workflow)
        self.assertLess(self.workflow.index("name: Validate Docker Hub configuration"),
                        self.workflow.index("name: Fetch verified Release binaries"))
        for block in self.workflow.split("run: |")[1:]:
            shell = block.split("\n      - ", 1)[0]
            self.assertNotIn("${{ secrets.", shell)

    def test_both_registries_and_architectures_are_published_and_smoked(self):
        for component in ("server", "node"):
            self.assertIn("ghcr.io/${{ env.IMAGE_OWNER }}/ohmycine-" + component, self.workflow)
            self.assertIn("docker.io/${{ env.DOCKERHUB_NAMESPACE }}/ohmycine-" + component, self.workflow)
        self.assertEqual(self.workflow.count("platforms: linux/amd64,linux/arm64"), 2)
        self.assertEqual(self.workflow.count("IMAGE_REGISTRIES: ghcr.io/${{ env.IMAGE_OWNER }},docker.io/${{ env.DOCKERHUB_NAMESPACE }}"), 2)
        self.assertIn('for registry in "${registries[@]}"; do', self.smoke)
        self.assertIn("for arch in amd64 arm64; do", self.smoke)
        self.assertIn('image="${registry}/ohmycine-${component}:v${VERSION}"', self.smoke)

    def test_promotion_remains_after_smoke_and_gated(self):
        self.assertLess(self.workflow.index("run: bash .github/scripts/docker_smoke.sh"),
                        self.workflow.index("name: Promote only the newest published channel release"))
        self.assertIn('python3 .github/scripts/docker_release_guard.py "$VERSION"', self.workflow)
        self.assertIn('for registry in "${registries[@]}"; do', self.workflow)
        self.assertIn("cancel-in-progress: false", self.workflow)
        self.assertNotIn(":latest", self.workflow)

    def test_release_and_manual_triggers_check_source_branches(self):
        self.assertNotIn("types: [published]", self.workflow)
        self.assertIn("workflow_dispatch:", self.workflow)
        self.assertIn("branch=main", self.workflow)
        self.assertIn("channel=beta; branch=develop", self.workflow)
        self.assertIn('git merge-base --is-ancestor "$commit"', self.workflow)
        self.assertIn('aliases=(stable latest)', self.workflow)


class PromotionTests(unittest.TestCase):
    def test_channels_promote_independently(self):
        pages = [[{"tag_name": "server-v1.0.0", "prerelease": False, "draft": False},
                  {"tag_name": "server-v2.0.0", "prerelease": True, "draft": False}]]
        self.assertTrue(may_promote("1.0.0", pages, "stable"))
        self.assertFalse(may_promote("2.0.0", pages, "stable"))
        self.assertTrue(may_promote("2.0.0", pages, "beta"))

    def test_older_run_cannot_replace_beta_even_when_returned_first(self):
        pages = [[{"tag_name": "server-v1.9.0", "prerelease": True, "draft": False}],
                 [{"tag_name": "server-v1.10.0", "prerelease": True, "draft": False}]]
        self.assertFalse(may_promote("1.9.0", pages))
        self.assertTrue(may_promote("1.10.0", pages))

    def test_requires_published_server_beta_and_supports_idempotent_retry(self):
        pages = [[{"tag_name": "v99.0.0", "prerelease": True, "draft": False},
                  {"tag_name": "server-v9.0.0", "prerelease": True, "draft": True},
                  {"tag_name": "server-v1.0.0", "prerelease": True, "draft": False}]]
        self.assertTrue(may_promote("1.0.0", pages))
        self.assertFalse(may_promote("9.0.0", pages))
        self.assertFalse(may_promote("1.0.0", []))


if __name__ == "__main__":
    unittest.main()
