"""Release guard regressions: old binaries and inconsistent displays must fail."""

import unittest

from check_version import validate


class VersionCheckTest(unittest.TestCase):
    def setUp(self):
        self.output = "sw 0.1.0-alpha.2 (Kubernetes API v1.36.0+shearwater.0.1.0-alpha.2)\n"
        self.help = "sw - Shearwater 0.1.0-alpha.2\n\n  sw up ...\n"

    def test_release_matches(self):
        self.assertEqual(validate(self.output, self.output, self.help,
                                  "v0.1.0-alpha.2"), "0.1.0-alpha.2")

    def test_old_binary_under_new_tag_fails(self):
        old = self.output.replace("0.1.0-alpha.2", "0.0.1")
        with self.assertRaisesRegex(ValueError, "release tag"):
            validate(old, old, self.help.replace("0.1.0-alpha.2", "0.0.1"),
                     "v0.1.0-alpha.2")

    def test_wrong_tag_fails(self):
        for tag in ("v0.1.0-alpha.1", "0.1.0-alpha.2", "v0.01.0-alpha.2", ""):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                validate(self.output, self.output, self.help, tag)

    def test_stale_flag_or_help_fails(self):
        for flag, help_text in (("sw 0.0.1\n", self.help), (self.output, "sw - Shearwater 0.0.1\n")):
            with self.subTest(flag=flag, help=help_text), self.assertRaises(ValueError):
                validate(self.output, flag, help_text)

    def test_stale_api_metadata_fails(self):
        wrong = self.output.replace("shearwater.0.1.0-alpha.2", "shearwater.0.0.1")
        with self.assertRaisesRegex(ValueError, "metadata"):
            validate(wrong, wrong, self.help)

    def test_invalid_product_version_fails(self):
        for version in ("garbage", "0.01.0", "0.1.0-alpha.02"):
            wrong = self.output.replace("0.1.0-alpha.2", version)
            with self.subTest(version=version), self.assertRaises(ValueError):
                validate(wrong, wrong, self.help)

    def test_ordinary_branch_can_check_without_tag(self):
        self.assertEqual(validate(self.output, self.output, self.help), "0.1.0-alpha.2")


if __name__ == "__main__":
    unittest.main()
