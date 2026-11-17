#!/usr/bin/env python3
"""Download one release into a NEW directory; never alter a running server.

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
import stat
import tarfile
import tempfile
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True, help='exact published version, e.g. v0.1.0-alpha.11')
    parser.add_argument('--destination', type=Path, help='new version directory; never overwritten')
    parser.add_argument('--apply', action='store_true', help='download and install after reviewing the default preview')
    args = parser.parse_args()
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?', args.version):
        parser.error('an exact version is required, not latest or a URL')
    if platform.system() != 'Linux' or platform.machine() not in ('x86_64', 'amd64'):
        parser.error('this release installer supports Linux x86_64 (including WSL)')
    destination = args.destination or Path.home() / '.local/share/shearwater' / args.version
    if not destination.is_absolute() or destination.exists() or destination.is_symlink():
        parser.error('destination must be an absolute NEW directory')
    name = f'shearwater-{args.version}-linux-amd64.tar.gz'
    base = f'https://github.com/ChoYugyeong/shearwater/releases/download/{args.version}'
    if not args.apply:
        print(json.dumps(dict(applied=False, source=base, destination=str(destination),
                             next='repeat with --apply; no packages, server, PATH or AI profiles will be changed')))
        return
    download_dir = Path(tempfile.mkdtemp(prefix='shearwater-download-'))
    print('Download evidence: ' + str(download_dir), flush=True)
    download(base + '/' + name, download_dir / name, MAX_ARCHIVE)
    download(base + '/SHA256SUMS', download_dir / 'SHA256SUMS', 1024 * 1024)
    check_hash(download_dir / name, expected_hash((download_dir / 'SHA256SUMS').read_text(), name))
    extract_verified(download_dir / name, destination)
    print(json.dumps(dict(installed=True, executable=str(destination / 'sw'),
                         checksumVerified=True, serverChanged=False,
                         next='run this sw version; alpha.11 and later include ONBOARDING-ko.md for setup and AI connection; older releases use INSTALL-ko.md')))


if __name__ == '__main__':
    main()
