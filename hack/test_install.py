"""Installer boundary tests: hostile archives cannot escape a new destination."""
import io
from pathlib import Path
import tarfile
import tempfile
import unittest
import install


class InstallerTests(unittest.TestCase):
    def member(self, name, kind=tarfile.REGTYPE):
        m = tarfile.TarInfo(name)
        m.type = kind
        return m

    def valid(self):
        return [self.member('release/' + name) for name in ('sw', 'sw.sha256', 'LICENSE', 'NOTICE')]

    def test_archive_boundaries(self):
        install.validate_members(self.valid())
        for extra in [self.member('../outside'), self.member('/outside'),
                      self.member('release/../outside'), self.member('release/link', tarfile.SYMTYPE),
                      self.member('release/link', tarfile.LNKTYPE), self.member('other/file'),
                      self.member('release/sw'), self.member('release/device', tarfile.CHRTYPE)]:
            with self.subTest(name=extra.name, kind=extra.type), self.assertRaises(ValueError):
                install.validate_members(self.valid() + [extra])

    def test_checksum_is_exact_and_unique(self):
        digest = 'a' * 64
        self.assertEqual(install.expected_hash(digest + '  sw\n', 'sw'), digest)
        for text in [digest + '  other', (digest + '  sw\n') * 2, 'bad  sw']:
            with self.assertRaises(ValueError):
                install.expected_hash(text, 'sw')

    def test_no_overwrite_or_early_extraction(self):
        with tempfile.TemporaryDirectory() as root:
            archive = Path(root) / 'bad.tar.gz'
            with tarfile.open(archive, 'w:gz') as out:
                for member in self.valid() + [self.member('../outside')]:
                    out.addfile(member, io.BytesIO())
            destination = Path(root) / 'new'
            with self.assertRaises(ValueError):
                install.extract_verified(archive, destination)
            self.assertFalse(destination.exists())
            destination.mkdir()
            marker = destination / 'keep'
            marker.write_text('data')
            with self.assertRaises(ValueError):
                install.extract_verified(archive, destination)
            self.assertEqual(marker.read_text(), 'data')


if __name__ == '__main__':
    unittest.main()
