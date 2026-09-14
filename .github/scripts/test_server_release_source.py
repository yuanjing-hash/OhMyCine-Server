import unittest
from server_release_source import classify


class SourceTests(unittest.TestCase):
    def test_tag_channels(self):
        self.assertEqual(classify("push", "refs/tags/server-v1.2.3", True, True), "stable")
        self.assertEqual(classify("push", "refs/tags/server-v1.2.3", False, True), "beta")

    def test_manual_develop_only(self):
        self.assertEqual(classify("workflow_dispatch", "refs/heads/develop", False, True), "beta")
        for ref, valid in (("refs/heads/main", True), ("refs/heads/develop", False)):
            with self.assertRaises(ValueError):
                classify("workflow_dispatch", ref, False, valid)

    def test_rejects_other_sources(self):
        for ref in ("refs/heads/develop", "refs/tags/v1.2.3", "refs/tags/server-v01.2.3"):
            with self.assertRaises(ValueError):
                classify("push", ref, True, True)
        with self.assertRaises(ValueError):
            classify("push", "refs/tags/server-v1.2.3", False, False)


if __name__ == "__main__":
    unittest.main()
