"""The release path of the Compose quick start check must not skip checks the release has."""

import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    'compose_quickstart_check', Path(__file__).resolve().parent / 'compose-quickstart-check.py')
check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check)


class FailureExplanationGateTest(unittest.TestCase):
    def test_releases_with_explanations_get_every_check(self):
        for release in ('v0.1.0-alpha.15', 'v0.1.0-alpha.16', 'v0.1.0-alpha.100',
                        'v0.1.0', 'v0.1.1-alpha.1', 'v0.2.0', 'v1.0.0'):
            with self.subTest(release=release):
                self.assertTrue(check.explains_failures(release))

    def test_older_releases_skip_only_those_checks(self):
        for release in ('v0.1.0-alpha.14', 'v0.1.0-alpha.2', 'v0.0.9'):
            with self.subTest(release=release):
                self.assertFalse(check.explains_failures(release))

    def test_unparsed_version_does_not_quietly_skip(self):
        for release in ('', None, '0.1.0-alpha.14', 'v0.1.0-beta.1', 'latest'):
            with self.subTest(release=release):
                self.assertTrue(check.explains_failures(release))


if __name__ == '__main__':
    unittest.main()
