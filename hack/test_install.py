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


class FakeSystem(install.System):
    def __init__(self, files=None, tools=(), packages=(), euid=0):
        self.files = {'/etc/os-release': 'ID=ubuntu\nVERSION_ID="24.04"\n', '/run/systemd/system': '',
                      '/proc/sys/net/ipv4/ip_forward': '0\n'}
        self.files.update(files or {})
        self.tools, self.packages, self.uid = set(tools) | {'apt-get'}, set(packages), euid

    def which(self, name):
        return '/usr/bin/' + name if name in self.tools else None

    def exists(self, path):
        return path in self.files

    def read(self, path):
        return self.files[path]

    def installed(self, package):
        return package in self.packages

    def euid(self):
        return self.uid

    def run(self, args, check=True):
        raise AssertionError('planning must not run commands: ' + ' '.join(args))


class HostPlanTests(unittest.TestCase):
    def plan(self, sys_, user='ops'):
        problems, steps = install.plan_host(sys_, 'v0.1.0-alpha.14', user, 'web-1')
        return problems, {s['step']: s for s in steps}

    def test_fresh_ubuntu_installs_runtime_and_starts_service(self):
        problems, steps = self.plan(FakeSystem(packages={'ca-certificates'}))
        self.assertEqual(problems, [])
        self.assertEqual(steps['packages']['install'], ['iproute2', 'iptables', 'containerd', 'runc'])
        self.assertEqual(steps['containerd']['action'], 'systemctl enable --now containerd')
        self.assertIn(install.SYSCTL_FILE, steps['ipv4-forwarding']['action'])
        self.assertEqual(steps['release']['destination'], '/opt/shearwater/releases/v0.1.0-alpha.14')
        self.assertEqual(steps['service']['action'], 'install and start shearwater.service')
        self.assertEqual(steps['user']['action'], 'sw setup --user ops --apply')

    def test_existing_containerd_is_reused_never_replaced(self):
        # Docker's containerd.io conflicts with the distribution packages.
        sys_ = FakeSystem(files={install.SOCKET: ''}, tools={'containerd', 'runc'})
        _, steps = self.plan(sys_)
        self.assertNotIn('containerd', steps['packages']['install'])
        self.assertNotIn('runc', steps['packages']['install'])
        self.assertTrue(steps['containerd']['action'].startswith('reuse'))

    def test_unsupported_hosts_are_refused_with_reasons(self):
        no_systemd = FakeSystem()
        del no_systemd.files['/run/systemd/system']
        cases = [
            (FakeSystem(euid=1000), 'as root'),
            (FakeSystem(files={'/etc/os-release': 'ID=fedora\n'}), 'Ubuntu and Debian'),
            (no_systemd, 'systemd'),
        ]
        for sys_, want in cases:
            problems, _ = self.plan(sys_)
            self.assertTrue(any(want in p for p in problems), problems)
        problems, _ = self.plan(FakeSystem(files={'/etc/os-release': 'ID=linuxmint\nID_LIKE="ubuntu debian"\n'}))
        self.assertEqual(problems, [], 'an ID_LIKE=debian system is supported')

    def test_existing_installation_is_kept(self):
        sys_ = FakeSystem(files={install.UNIT: '', install.SYSCTL_FILE: '', '/opt/shearwater/releases/v0.1.0-alpha.14': ''},
                          packages=set(install.TOOLS) | set(install.RUNTIME))
        _, steps = self.plan(sys_, user=None)
        self.assertEqual(steps['packages']['install'], [])
        self.assertEqual(steps['ipv4-forwarding']['action'], 'keep')
        self.assertEqual(steps['release']['action'], 'keep existing')
        self.assertTrue(steps['service']['action'].startswith('keep'))
        self.assertTrue(steps['user']['action'].startswith('skip'))

    def test_foreign_sw_link_is_a_problem(self):
        sys_ = FakeSystem(files={install.LINK: ''})
        problems, _ = self.plan(sys_)
        self.assertTrue(any(install.LINK in p for p in problems), problems)

    def test_node_names_are_dns_labels(self):
        self.assertEqual(install.node_name('VPS_Seoul.01'), 'vps-seoul-01')
        self.assertEqual(install.node_name('---'), 'shearwater')
        self.assertEqual(len(install.node_name('a' * 80)), 63)

    def test_os_release_parsing(self):
        info = install.parse_os_release('ID=debian\nPRETTY_NAME="Debian GNU/Linux 12"\n# comment\n')
        self.assertTrue(install.apt_family(info))
        self.assertEqual(info['PRETTY_NAME'], 'Debian GNU/Linux 12')


if __name__ == '__main__':
    unittest.main()
