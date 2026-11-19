#!/usr/bin/env python3
"""Download one release into a NEW directory; never alter a running server.

With --host (as root on Ubuntu/Debian) it also prepares a fresh server: it
installs containerd/runc only when no runtime exists, persists IPv4
forwarding, installs under /opt/shearwater/releases and starts the systemd
service. It prints the exact plan first and changes nothing without --apply.

Python's standard library is enough. We verify the whole archive before opening
it, then validate every archive path before extracting regular files ourselves.
No archive-provided program or shell command runs during installation. A checksum
detects corruption; it is not an independent signature of the release publisher.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import socket
import stat
import subprocess
import tarfile
import tempfile
import time
import urllib.request

MAX_ARCHIVE = 256 * 1024 * 1024
MAX_EXPANDED = 512 * 1024 * 1024


def download(url, destination, limit):
    # Only this program constructs URLs. HTTPS validation stays enabled, including
    # GitHub's release-asset redirects. Never accept a file/HTTP URL from a model.
    with urllib.request.urlopen(url, timeout=60) as response, destination.open('xb') as out:
        total = 0
        while chunk := response.read(1024 * 1024):
            total += len(chunk)
            if total > limit:
                raise ValueError('download exceeds size limit')
            out.write(chunk)


def expected_hash(sums, filename):
    matches = []
    for line in sums.splitlines():
        m = re.fullmatch(r'([0-9a-f]{64}) [ *](.+)', line)
        if m and m[2] == filename:
            matches.append(m[1])
    if len(matches) != 1:
        raise ValueError('exactly one checksum is required for ' + filename)
    return matches[0]


def check_hash(path, expected):
    with path.open('rb') as stream:
        actual = hashlib.file_digest(stream, 'sha256').hexdigest()
    if actual != expected:
        raise ValueError('SHA-256 mismatch: ' + path.name)


def validate_members(members):
    if not members or len(members) > 4096:
        raise ValueError('invalid archive member count')
    root, seen, size = None, set(), 0
    for member in members:
        path = PurePosixPath(member.name)
        if path.is_absolute() or '..' in path.parts or '\\' in member.name or not path.parts:
            raise ValueError('unsafe archive path')
        if root is None:
            root = path.parts[0]
        if path.parts[0] != root or not (member.isdir() or member.isfile()):
            raise ValueError('archive must contain one root and only directories/regular files')
        relative = PurePosixPath(*path.parts[1:])
        if str(relative) in seen or (str(relative) == '.' and not member.isdir()):
            raise ValueError('duplicate/invalid archive path')
        seen.add(str(relative))
        size += member.size
        if size > MAX_EXPANDED or member.size < 0:
            raise ValueError('archive exceeds expanded size limit')
    if not {'sw', 'sw.sha256', 'LICENSE', 'NOTICE'}.issubset(seen):
        raise ValueError('release is missing executable/checksum/license files')


def extract_verified(archive, destination):
    if destination.exists() or destination.is_symlink():
        raise ValueError('destination exists; choose a new version directory')
    # Refuse symlink parents instead of installing somewhere the caller cannot
    # identify. Failed downloads/extractions are retained for inspection.
    for parent in (destination.parent, *destination.parents):
        if parent.is_symlink():
            raise ValueError('destination ancestors must not be symlinks')
    with tarfile.open(archive, 'r:gz') as bundle:
        members = bundle.getmembers()
        validate_members(members)
        destination.mkdir(parents=True, mode=0o700, exist_ok=False)
        for member in members:
            relative = PurePosixPath(*PurePosixPath(member.name).parts[1:])
            target = destination.joinpath(*relative.parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with bundle.extractfile(member) as source, target.open('xb') as out:
                    shutil.copyfileobj(source, out)
                # Do not copy setuid, group-writable or device permissions.
                target.chmod(0o755 if member.mode & stat.S_IXUSR else 0o644)
    check_hash(destination / 'sw', expected_hash((destination / 'sw.sha256').read_text(), 'sw'))
    (destination / 'sw').chmod(0o755)


# ---- --host: prepare a fresh Ubuntu/Debian server and start Shearwater ----
#
# Everything is planned first and printed; nothing changes without --apply.
# An existing containerd (for example Docker's) is reused and never replaced:
# Docker's containerd.io package conflicts with the distribution packages.

SOCKET = '/run/containerd/containerd.sock'
RELEASES = '/opt/shearwater/releases'
LINK = '/usr/local/bin/sw'
SYSCTL_FILE = '/etc/sysctl.d/90-shearwater.conf'
UNIT = '/etc/systemd/system/shearwater.service'
TOOLS = ('ca-certificates', 'iproute2', 'iptables')  # always wanted
RUNTIME = ('containerd', 'runc')                    # only when no runtime exists


def parse_os_release(text):
    info = {}
    for line in text.splitlines():
        key, sep, value = line.partition('=')
        if sep and re.fullmatch(r'[A-Z_]+', key):
            info[key] = value.strip().strip('"\'')
    return info


def apt_family(info):
    return info.get('ID') in ('ubuntu', 'debian') or 'debian' in info.get('ID_LIKE', '').split()


def node_name(hostname):
    name = re.sub(r'[^a-z0-9-]+', '-', hostname.lower()).strip('-')[:63].strip('-')
    return name or 'shearwater'


class System:
    """The few host operations --host needs; tests replace them."""

    def run(self, args, check=True):
        return subprocess.run(args, check=check, text=True, capture_output=True,
                              env=dict(os.environ, DEBIAN_FRONTEND='noninteractive', LC_ALL='C'))

    def which(self, name):
        return shutil.which(name)

    def exists(self, path):
        return os.path.lexists(path)

    def read(self, path):
        return Path(path).read_text()

    def euid(self):
        return os.geteuid()

    def installed(self, package):
        p = self.run(['dpkg-query', '-W', '-f=${Status}', package], check=False)
        return p.returncode == 0 and p.stdout.strip() == 'install ok installed'


def plan_host(sys_, version, user, node):
    """Return the ordered steps --host would take on this machine."""
    problems, steps = [], []
    if sys_.euid() != 0:
        problems.append('run --host as root, for example with sudo')
    info = parse_os_release(sys_.read('/etc/os-release')) if sys_.exists('/etc/os-release') else {}
    if not apt_family(info) or not sys_.which('apt-get'):
        problems.append('--host supports Ubuntu and Debian with apt-get; prepare other systems by hand (docs/install.md)')
    if not sys_.exists('/run/systemd/system'):
        problems.append('--host needs systemd as the init system')
    reuse = sys_.exists(SOCKET) or bool(sys_.which('containerd'))
    wanted = [p for p in TOOLS if not sys_.installed(p)]
    if not reuse:
        wanted += [p for p in RUNTIME if not sys_.installed(p)]
    steps.append(dict(step='packages', install=wanted,
                      note='an existing containerd/runc is reused, never replaced' if reuse else 'from the signed distribution archive'))
    steps.append(dict(step='containerd', action='reuse ' + SOCKET if sys_.exists(SOCKET) else 'systemctl enable --now containerd'))
    forwarding = sys_.read('/proc/sys/net/ipv4/ip_forward').strip() if sys_.exists('/proc/sys/net/ipv4/ip_forward') else '0'
    persisted = sys_.exists(SYSCTL_FILE)
    steps.append(dict(step='ipv4-forwarding',
                      action='keep' if persisted else 'write ' + SYSCTL_FILE + ' and apply it',
                      current=forwarding, note='host-wide setting needed for Pod networking'))
    destination = RELEASES + '/' + version
    steps.append(dict(step='release', destination=destination, link=LINK,
                      action='keep existing' if sys_.exists(destination) else 'download, verify and extract'))
    if sys_.exists(LINK) and not os.path.realpath(LINK).startswith(RELEASES + '/'):
        problems.append(LINK + ' exists and is not a Shearwater release link; remove or rename it first')
    steps.append(dict(step='service', node=node,
                      action='keep the existing unit (update by the documented procedure)' if sys_.exists(UNIT)
                      else 'install and start shearwater.service'))
    steps.append(dict(step='user', action=f'sw setup --user {user} --apply' if user else 'skip (no non-root sudo user)'))
    return problems, steps


def apply_host(sys_, steps, version, archive, sums, user, node, log):
    by = {s['step']: s for s in steps}
    if by['packages']['install']:
        log('apt-get update')
        sys_.run(['apt-get', 'update', '-q'])
        log('apt-get install ' + ' '.join(by['packages']['install']))
        sys_.run(['apt-get', 'install', '-qy', '--no-install-recommends', *by['packages']['install']])
    if not by['containerd']['action'].startswith('reuse'):
        sys_.run(['systemctl', 'enable', '--now', 'containerd'])
    for _ in range(60):
        if sys_.exists(SOCKET):
            break
        time.sleep(1)
    else:
        raise RuntimeError('containerd did not create ' + SOCKET)
    if by['ipv4-forwarding']['action'] != 'keep':
        Path(SYSCTL_FILE).write_text('# Written by the Shearwater installer: Pod networking forwards IPv4.\nnet.ipv4.ip_forward = 1\n')
        sys_.run(['sysctl', '-q', '-p', SYSCTL_FILE])
    destination = Path(RELEASES) / version
    if not destination.exists():
        extract_verified(archive, destination)
        destination.chmod(0o755)  # other users run client commands such as sw task
    sw = str(destination / 'sw')
    if os.path.lexists(LINK):
        os.remove(LINK)  # plan_host allowed only an earlier release link here
    os.symlink(sw, LINK)
    if not sys_.exists(UNIT):
        sys_.run([sw, 'service', 'install', '--binary', sw, '--node-name', node])
        sys_.run([sw, 'service', 'start'])
    log('waiting for the node to become Ready')
    deadline = time.time() + 120
    while True:
        doctor = sys_.run([sw, 'doctor', '--node-name', node], check=False)
        if f'OK   node: {node} Ready' in doctor.stdout:
            break
        if time.time() > deadline:
            raise RuntimeError('the node did not become Ready:\n' + doctor.stdout[-2000:])
        time.sleep(2)
    if user:
        sys_.run([sw, 'setup', '--user', user, '--apply'])
    return dict(executable=sw, doctor=doctor.stdout.strip().splitlines())


def fetch_release(version, archive, sums):
    """Return a verified archive path: the given local files or a download."""
    name = f'shearwater-{version}-linux-amd64.tar.gz'
    if archive:
        if archive.name != name or not sums:
            raise ValueError(f'a local archive must be named {name} and come with --sums')
        check_hash(archive, expected_hash(sums.read_text(), name))
        return archive, sums
    base = f'https://github.com/ChoYugyeong/shearwater/releases/download/{version}'
    download_dir = Path(tempfile.mkdtemp(prefix='shearwater-download-'))
    print('Download evidence: ' + str(download_dir), flush=True)
    download(base + '/' + name, download_dir / name, MAX_ARCHIVE)
    download(base + '/SHA256SUMS', download_dir / 'SHA256SUMS', 1024 * 1024)
    check_hash(download_dir / name, expected_hash((download_dir / 'SHA256SUMS').read_text(), name))
    return download_dir / name, download_dir / 'SHA256SUMS'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True, help='exact published version, e.g. v0.1.0-alpha.13')
    parser.add_argument('--destination', type=Path, help='new version directory; never overwritten')
    parser.add_argument('--apply', action='store_true', help='download and install after reviewing the default preview')
    parser.add_argument('--host', action='store_true',
                        help='as root on Ubuntu/Debian: also install containerd/runc if missing, enable IPv4 forwarding, '
                             'install under /opt/shearwater/releases and start the shearwater systemd service')
    parser.add_argument('--node-name', help='node name for --host (default: the host name as a DNS label)')
    parser.add_argument('--archive', type=Path, help='verified local release archive instead of downloading')
    parser.add_argument('--sums', type=Path, help='SHA256SUMS that lists --archive')
    args = parser.parse_args()
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?', args.version):
        parser.error('an exact version is required, not latest or a URL')
    if platform.system() != 'Linux' or platform.machine() not in ('x86_64', 'amd64'):
        parser.error('this release installer supports Linux x86_64 (including WSL)')
    if args.host:
        if args.destination:
            parser.error('--host always installs under ' + RELEASES)
        user = os.environ.get('SUDO_USER') if os.environ.get('SUDO_USER') not in (None, '', 'root') else None
        node = args.node_name or node_name(socket.gethostname())
        if node_name(node) != node:
            parser.error('--node-name must be a lowercase DNS label')
        sys_ = System()
        problems, steps = plan_host(sys_, args.version, user, node)
        if problems or not args.apply:
            print(json.dumps(dict(applied=False, problems=problems, steps=steps,
                                  next='fix the problems first' if problems else 'repeat with --apply to make exactly these changes'), indent=2))
            if problems:
                raise SystemExit(1)
            return
        started = time.time()
        archive, sums = fetch_release(args.version, args.archive, args.sums)
        result = apply_host(sys_, steps, args.version, archive, sums, user, node,
                            lambda m: print('==> ' + m, flush=True))
        print(json.dumps(dict(applied=True, seconds=round(time.time() - started, 1), steps=steps, **result,
                              next='deploy an app: sudo kubectl --kubeconfig /etc/shearwater/kubeconfig apply -f '
                                   + RELEASES + '/' + args.version + '/examples/web.yaml'), indent=2))
        return
    destination = args.destination or Path.home() / '.local/share/shearwater' / args.version
    if not destination.is_absolute() or destination.exists() or destination.is_symlink():
        parser.error('destination must be an absolute NEW directory')
    name = f'shearwater-{args.version}-linux-amd64.tar.gz'
    base = f'https://github.com/ChoYugyeong/shearwater/releases/download/{args.version}'
    if not args.apply:
        print(json.dumps(dict(applied=False, source=base, destination=str(destination),
                             next='repeat with --apply; no packages, server, PATH or AI profiles will be changed')))
        return
    archive, _ = fetch_release(args.version, args.archive, args.sums)
    extract_verified(archive, destination)
    print(json.dumps(dict(installed=True, executable=str(destination / 'sw'),
                         checksumVerified=True, serverChanged=False,
                         next='run this sw version; alpha.11 and later include ONBOARDING-ko.md for setup and AI connection; older releases use INSTALL-ko.md')))


if __name__ == '__main__':
    main()
