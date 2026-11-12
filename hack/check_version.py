#!/usr/bin/env python3
"""Check the executable we will ship, optionally against its release tag.

Source builds deliberately keep the version in internal/api/server.go: a GitHub
source archive and a Git checkout must produce the same version without needing
Git, linker flags or a network lookup. A release must supply --tag; otherwise
this checks CLI/help consistency only, which is useful on ordinary branch CI.
"""

import argparse
import re
import subprocess
import sys
from pathlib import Path


def semver(value):
    """SemVer core/prerelease validation, including numeric leading-zero rules."""
    number = r"(?:0|[1-9][0-9]*)"
    match = re.fullmatch(
        rf"{number}\.{number}\.{number}"
        r"(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?"
        r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?", value)
    return bool(match) and all(
        not (part.isdigit() and len(part) > 1 and part.startswith("0"))
        for part in (match[1] or "").split("."))


def validate(version_output, flag_output, help_output, tag=None):
    """Raise ValueError rather than allowing a mismatched release to continue."""
    match = re.fullmatch(r"sw (\S+) \(Kubernetes API (\S+)\)\n", version_output)
    if not match or not semver(match[1]):
        raise ValueError("sw version must contain a valid product version")
    version, api = match.groups()
    # A product prerelease is metadata on the Kubernetes compatibility version.
    # Do not replace the API prefix with 0.1.0: kubectl would interpret it as an
    # unsupported Kubernetes version. Escape punctuation before using a regex.
    if not semver(api.removeprefix("v")) or not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+\+shearwater\." +
                        re.escape(version), api):
        raise ValueError("Kubernetes API metadata differs from the product version")
    if flag_output != version_output:
        raise ValueError("sw --version differs from sw version")
    if not help_output.startswith("sw - Shearwater " + version + "\n"):
        raise ValueError("sw --help contains a different product version")
    if tag is not None and (not tag.startswith("v") or
                            not semver(tag[1:]) or tag[1:] != version):
        raise ValueError(f"release tag {tag!r} does not match sw version {version!r}")
    return version


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path, help="the built Linux sw executable")
    parser.add_argument("--tag", help="expected release tag, e.g. v0.1.0-alpha.2")
    args = parser.parse_args()
    try:
        binary = str(args.binary.resolve(strict=True))
        outputs = [subprocess.run([binary, arg], check=True, capture_output=True,
                                  text=True, timeout=10).stdout
                   for arg in ("version", "--version", "--help")]
        version = validate(*outputs, tag=args.tag)
    except (OSError, subprocess.SubprocessError, ValueError) as error:
        print("Version check failed: " + str(error), file=sys.stderr)
        return 1
    print(f"PASS: CLI/help/API metadata agree on {version}" +
          (f" and release tag {args.tag}" if args.tag else " (no tag checked)"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
