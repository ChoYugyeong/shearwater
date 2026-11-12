#!/usr/bin/env python3
# Copyright 2026 The Shearwater Authors.
# SPDX-License-Identifier: Apache-2.0
"""Copy/verify this alpha's reviewed notices using only existing local files.

Run in Linux/WSL after ". hack/env.sh". Default: verify the bundled documents.
--check-originals also compares the local cache; --write copies from it.
No downloads, dependency discovery, installation, source edits or deletions.
Unknown module versions, toolchains and license bytes require a new review.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
GO_VERSION = "go1.27.3"
ROOT_LICENSE_SHA256 = "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"

AUDITED_MODULES = {
    "github.com/cespare/xxhash/v2": ("v2.3.0", "MIT", "LICENSE.txt", "f566a9f97bacdaf00d9f21dd991e81dc11201c4e016c86b470799429a1c9a79c"),
    "github.com/containerd/containerd/api": ("v1.11.1", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/containerd/containerd/v2": ("v2.3.7", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/containerd/continuity": ("v0.5.0", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/containerd/errdefs": ("v1.0.0", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/containerd/errdefs/pkg": ("v0.3.0", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/containerd/fifo": ("v1.1.0", "Apache-2.0", "LICENSE", "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4"),
    "github.com/containerd/log": ("v0.1.0", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/containerd/platforms": ("v1.0.0-rc.5", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/containerd/plugin": ("v1.1.0", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/containerd/ttrpc": ("v1.2.8", "Apache-2.0", "LICENSE", "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4"),
    "github.com/containerd/typeurl/v2": ("v2.2.3", "Apache-2.0", "LICENSE", "4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4"),
    "github.com/distribution/reference": ("v0.6.0", "Apache-2.0", "LICENSE", "cb5e8e7e5f4a3988e1063c142c60dc2df75605f4c46515e776e3aca6df976e14"),
    "github.com/dustin/go-humanize": ("v1.1.0", "MIT", "LICENSE", "a973b4498c13eb74baa2a8e5c351426a6826f2fcdd909916dbe53ee2e755fd71"),
    "github.com/felixge/httpsnoop": ("v1.0.4", "MIT", "LICENSE.txt", "8108e14b5cb2b6bcd51583d3849ef0c2b5c4de61758256c20308bc70d1775dbb"),
    "github.com/go-logr/logr": ("v1.4.3", "Apache-2.0", "LICENSE", "b40930bbcf80744c86c46a12bc9da056641d722716c378f5659b9e555ef833e1"),
    "github.com/go-logr/stdr": ("v1.2.2", "Apache-2.0", "LICENSE", "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4"),
    "github.com/gogo/protobuf": ("v1.3.2", "BSD-3-Clause", "LICENSE", "3f571768c0ba2f330e5fee90de89bfe5141f48c1939be8008fa61461c30ca07f"),
    "github.com/google/uuid": ("v1.6.0", "BSD-3-Clause", "LICENSE", "0a8d61ed3cbfd5312326e8126c31ce9c627a283adc99131b56896d29ada04b2d"),
    "github.com/klauspost/compress": ("v1.18.5", "BSD-3-Clause; path-specific MIT/Apache-2.0 notices", "LICENSE", "0d9e582ee4bff57bf1189c9e514e6da7ce277f9cd3bc2d488b22fbb39a6d87cf"),
    "github.com/moby/locker": ("v1.0.1", "Apache-2.0", "LICENSE", "9b44ae21e39b0ec20e89984bfb437d20617236285c051c42d95672babe2dad62"),
    "github.com/moby/sys/mountinfo": ("v0.7.2", "Apache-2.0", "LICENSE", "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"),
    "github.com/moby/sys/signal": ("v0.7.1", "Apache-2.0", "LICENSE", "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"),
    "github.com/moby/sys/user": ("v0.4.0", "Apache-2.0", "LICENSE", "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"),
    "github.com/moby/sys/userns": ("v0.1.0", "Apache-2.0", "LICENSE", "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"),
    "github.com/opencontainers/go-digest": ("v1.0.0", "Apache-2.0", "LICENSE", "aceb55133c41c50c2e170c5918e301fe74f592d0f28724bf2f7a7bfdcff1fc5f"),
    "github.com/opencontainers/image-spec": ("v1.1.1", "Apache-2.0", "LICENSE", "b0a3f39513927db306adabea11d14c23f079d4febcea241d123a68d1a0d45418"),
    "github.com/opencontainers/runtime-spec": ("v1.3.0", "Apache-2.0", "LICENSE", "a964a27c435167bdd78827ff83e4bd4473eca53d583a39598d2f445894a9225d"),
    "github.com/remyoudompheng/bigfft": ("v0.0.0-20230129092748-24d4a6f8daec", "BSD-3-Clause", "LICENSE", "dd26a7abddd02e2d0aba97805b31f248ef7835d9e10da289b22e3b8ab78b324d"),
    "github.com/sirupsen/logrus": ("v1.9.4", "MIT", "LICENSE", "51a0c9ec7f8b7634181b8d4c03e5b5d204ac21d6e72f46c313973424664b2e6b"),
    "go.opentelemetry.io/auto/sdk": ("v1.2.1", "Apache-2.0", "LICENSE", "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4"),
    "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp": ("v0.68.0", "Apache-2.0; additional Go BSD-3-Clause notice", "LICENSE", "1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40"),
    "go.opentelemetry.io/otel": ("v1.44.0", "Apache-2.0; additional Go BSD-3-Clause notice", "LICENSE", "1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40"),
    "go.opentelemetry.io/otel/metric": ("v1.44.0", "Apache-2.0; additional Go BSD-3-Clause notice", "LICENSE", "1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40"),
    "go.opentelemetry.io/otel/trace": ("v1.44.0", "Apache-2.0; additional Go BSD-3-Clause notice", "LICENSE", "1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40"),
    "golang.org/x/net": ("v0.58.0", "BSD-3-Clause", "LICENSE", "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"),
    "golang.org/x/sync": ("v0.24.0", "BSD-3-Clause", "LICENSE", "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"),
    "golang.org/x/sys": ("v0.49.0", "BSD-3-Clause", "LICENSE", "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"),
    "golang.org/x/text": ("v0.41.0", "BSD-3-Clause", "LICENSE", "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"),
    "google.golang.org/genproto/googleapis/rpc": ("v0.0.0-20260526163538-3dc84a4a5aaa", "Apache-2.0", "LICENSE", "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"),
    "google.golang.org/grpc": ("v1.83.2", "Apache-2.0", "LICENSE", "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"),
    "google.golang.org/protobuf": ("v1.36.12-0.20260120151049-f2248ac996af", "BSD-3-Clause", "LICENSE", "4835612df0098ca95f8e7d9e3bffcb02358d435dbb38057c844c99d7f725eb20"),
    "modernc.org/libc": ("v1.80.1", "BSD-3-Clause; inherited MIT/BSD notices", "LICENSE", "95ff867eb55a56935fa7492406cfa953fb7c13ca73f4c0a86ae05756b4605600"),
    "modernc.org/mathutil": ("v1.7.1", "BSD-3-Clause", "LICENSE", "bfa9bf72a72ca009fd62a8f84fca3dca67e51d93af96352723646599898b6cf5"),
    "modernc.org/memory": ("v1.12.1", "BSD-3-Clause; inherited Go/mmap-go notices", "LICENSE", "59895e669f48f168b6b858358f6005779cdf40a265f7828813061b56af67b496"),
    "modernc.org/sqlite": ("v1.62.0", "BSD-3-Clause; SQLite public domain; inherited notices", "LICENSE", "c6fe05491a60ae13bcd223088d2705e36dede24e5587226231d2459ada5c4822"),
}

# Each supplemental document was reviewed separately; its scope is not inferred
# from the filename or from a keyword in its license text.
SUPPLEMENTAL = [
    ("module","github.com/containerd/containerd/v2","v2.3.7","NOTICE","03f0b4bcaecfdb491ef5a7d320e2c2adf886c732d34e3fff87d52dbaec64d8c3","Binary: inherited containerd/Docker notice, reproduced verbatim in NOTICE."),
    ("module","google.golang.org/grpc","v1.83.2","NOTICE.txt","693ff28ec216d5112ac1bbfe64ef539005867d1c7bd427b57d579683293b947f","Binary: gRPC notice, reproduced verbatim in NOTICE."),
    ("module","golang.org/x/sys","v0.49.0","PATENTS","96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc","Additional upstream patent grant retained with the binary notices."),
    ("module","golang.org/x/net","v0.58.0","PATENTS","96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc","Additional upstream patent grant retained with the binary notices."),
    ("module","golang.org/x/sync","v0.24.0","PATENTS","96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc","Additional upstream patent grant retained with the binary notices."),
    ("module","golang.org/x/text","v0.41.0","PATENTS","96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc","Additional upstream patent grant retained with the binary notices."),
    ("module","google.golang.org/protobuf","v1.36.12-0.20260120151049-f2248ac996af","PATENTS","96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc","Additional upstream patent grant retained with the binary notices."),
    ("module","github.com/klauspost/compress","v1.18.5","internal/snapref/LICENSE","f69f157b0be75da373605dbc8bbf142e8924ee82d8f44f11bcaf351335bf98cf","Binary: internal/snapref is used by the selected zstd packages; Snappy-Go BSD notice."),
    ("module","github.com/klauspost/compress","v1.18.5","zstd/internal/xxhash/LICENSE.txt","f566a9f97bacdaf00d9f21dd991e81dc11201c4e016c86b470799429a1c9a79c","Binary: selected zstd/internal/xxhash implementation; MIT notice."),
    ("module","github.com/opencontainers/go-digest","v1.0.0","LICENSE.docs","a1d1adec3ba691ef6e377a78bbe9e9e2501f2f4a1723d08857da1ac6564e4fed","Documentation-only CC-BY-SA-4.0 license retained as upstream context; not the Go code license. No upstream documentation is incorporated into the program by this entry."),
    ("module","modernc.org/libc","v1.80.1","LICENSE-3RD-PARTY.md","f597097efe3d97021f89170746bd3a0fb9a8b6fb26b82043ed68a4e0283bee6c","Binary/inherited: Go, musl (including its prior-work notices), go-netdb and NixOS/nixpkgs. This complete upstream document preserves the original scope."),
    ("module","modernc.org/mathutil","v1.7.1","mersenne/LICENSE","c05630f9ca899ea094620d37daea6ba66f99adbf9268ea5fea9db607e1102bd9","Supplemental module-source notice. The mersenne subpackage was not selected for this binary."),
    ("module","modernc.org/memory","v1.12.1","LICENSE-GO","2d36597f7117c38b006835ae7f537487207d8ec407aa9d9980794b2030cbc067","Binary/inherited Go copyright and BSD terms."),
    ("module","modernc.org/memory","v1.12.1","LICENSE-MMAP-GO","c2eba69f20d05414538c3a5df7694dde392e065ff70882e1625e90f5d6659fff","Binary/inherited mmap-go copyright and BSD terms."),
    ("module","modernc.org/memory","v1.12.1","LICENSE-LOGO","5ae5bee3072a841376451b48d8cfcec7188e10543926d5870828d36c8a750dc5","Upstream logo attribution URL only, retained as a reference. It is not a verified image license; no logo is bundled."),
    ("module","modernc.org/sqlite","v1.62.0","LICENSE-SQLITE","8438c9c89b849131ead81d5435cb97fcf052df5b0b286dda8a2d4c29e6cb3fd0","Binary: original SQLite public-domain dedication, separate from the Go driver's BSD license."),
    ("module","modernc.org/sqlite","v1.62.0","LICENSE-3RD-PARTY.md","f4a00ade8f4057ab0ea49097d9c459df8559fcf10ca78da0c950343fb7428d37","Complete upstream inventory, including runtime, test, graph-only and source-only sections. Its MPL/test entries are not claims that those components are linked into sw."),
    ("module","modernc.org/sqlite","v1.62.0","LICENSE-SQLITE_VEC","6ce72bbe12d975bd5286e5ab0a064c069693300c47bccbc57bec18485f1621ea","Supplemental MIT notice for optional sqlite-vec. Neither sqlite/vec nor sqlite/vfs was selected for this binary."),
    ("go","Go","go1.27.3","LICENSE","911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad","Binary: Go runtime and standard-library BSD-3-Clause license."),
    ("go","Go","go1.27.3","PATENTS","96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc","Go toolchain: additional upstream patent grant."),
    ("module","k8s.io/api","v0.36.6","LICENSE","cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30","Source-derived Kubernetes v1.36.6 schema/type metadata and fixture provenance only; not a linked module."),
    ("module","k8s.io/apimachinery","v0.36.6","LICENSE","cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30","Adapted strategic-merge behavior, protobuf envelope/schema and fixture oracle provenance only; not a linked module."),
]


NOTICE_PREFIX = """shearwater
Copyright 2026 The Shearwater Authors

Shearwater's original work and modifications are licensed under the Apache
License, Version 2.0. See LICENSE. This collective attribution does not replace
the notices of the original authors of the included or adapted material.

Kubernetes-derived material
---------------------------
Copyright The Kubernetes Authors.
Copyright 2014 The Kubernetes Authors.
Copyright 2015 The Kubernetes Authors.
Copyright 2017 The Kubernetes Authors.

internal/protobuf/tables.go contains field numbers manually transcribed from
Kubernetes v1.36.6 generated.proto files, with JSON/pointer metadata from the
adjacent types.go files, adapted to Shearwater's limited decoding tables.
internal/protobuf/protobuf.go uses the runtime.Unknown/TypeMeta envelope schema.
The source schema paths under staging/src/k8s.io/ are:
  api/core/v1/generated.proto
  api/apps/v1/generated.proto
  api/core/v1/types.go
  api/apps/v1/types.go
  apimachinery/pkg/apis/meta/v1/generated.proto
  apimachinery/pkg/runtime/generated.proto
  apimachinery/pkg/util/intstr/generated.proto
  apimachinery/pkg/api/resource/generated.proto

internal/patch/strategic.go adapts the strategic-merge behavior of
k8s.io/apimachinery v0.36.6 pkg/util/strategicpatch/patch.go. Shearwater uses its
own type-tag schema and returns errors for unknown directives and inputs that
panic in the reference implementation. The upstream file attributes earlier
code to github.com/openshift/origin/pkg/util/jsonmerge; that attribution is
retained here without claiming that the whole OpenShift project is included.

Protobuf fixtures were captured from kubectl v1.36.6 (Linux) and v1.36.0
(Windows). Their reference JSON, and expected/error values in the strategic
merge patch fixtures, were generated using Kubernetes apimachinery v0.36.6.
These are source/data provenance statements, not linked-module declarations.
See internal/protobuf/testdata/README.md and internal/patch/testdata/smp.json.

Original sources:
https://github.com/kubernetes/kubernetes/tree/v1.36.6/staging/src/k8s.io
https://github.com/kubernetes/apimachinery/blob/v0.36.6/pkg/util/strategicpatch/patch.go

The Kubernetes-derived material is licensed under Apache-2.0. The accompanying
THIRD_PARTY_NOTICES.md maps these sources and the linked Go modules to verbatim
license, notice and patent-grant documents in LICENSES/.

The following upstream notices are reproduced without editing their contents.
"""

THIRD_HEADER = """# Third-party notices for shearwater

Shearwater's original work and modifications are licensed under Apache-2.0;
see [LICENSE](LICENSE) and [NOTICE](NOTICE). Original third-party copyright,
license, patent and attribution notices remain applicable to their own work.

## Binary module inventory

This table describes the 46 external modules recorded by **go version -m
dist/sw** for the reviewed Linux/amd64, CGO_ENABLED=0, Go 1.27.3 alpha.
It is not a listing of go.sum, the complete Go module graph, or all downloaded
cache directories. Module identities and Go checksums are checked against
the binary and go.sum; replacement modules and unreviewed versions are refused.

The files below are copied byte-for-byte from the reviewed local module cache
and Go toolchain. Only identical SHA-256 contents share a copy. An Apache
document that also contains BSD or other attribution retains its entire text.
An upstream aggregate notice can describe additional tests, documentation or
optional components; their presence in the text does not declare them linked.

| Module | Version | Scope | License / original documents |
|---|---|---|---|
"""

THIRD_FOOTER = """
## Copied and adapted source/data

Kubernetes v1.36.6 generated.proto field numbers and adjacent type metadata
were transcribed/adapted for internal/protobuf/tables.go and the
runtime.Unknown/TypeMeta envelope in internal/protobuf/protobuf.go.
internal/patch/strategic.go adapts the behavior of apimachinery v0.36.6, with
Shearwater's type-tag schema and explicit error-handling differences.
Original Kubernetes copyright and modification notices appear in those files
and in NOTICE.

The 73 strategic merge patch fixture expectations and the reference JSON for
the captured protobuf bodies came from apimachinery v0.36.6. The 10 pairs of
protobuf bodies came from kubectl v1.36.6 Linux and v1.36.0 Windows.
The former oracle is recorded at Git commit 483e994 and was removed from the
working tree in 3598a0b. Using an oracle does not by itself make all of its
dependencies linked parts of the resulting program.

Official sources:
- [Kubernetes v1.36.6 schemas and types](https://github.com/kubernetes/kubernetes/tree/v1.36.6/staging/src/k8s.io)
- [apimachinery v0.36.6 strategic patch](https://github.com/kubernetes/apimachinery/blob/v0.36.6/pkg/util/strategicpatch/patch.go)

## Scope of inherited notices

- modernc.org/sqlite's LICENSE-3RD-PARTY.md is preserved in full. Its separate
  runtime/test/graph-only/source-only categories still apply. In particular,
  its MPL-2.0 golang-lru entry is graph-only for this reviewed binary.
- The optional sqlite/vec and sqlite/vfs packages and mathutil/mersenne were
  not selected. Supplemental notices for optional/source material are labeled
  separately above. The memory logo reference is not a verified image license,
  and no logo is included.
- modernc.org/libc's inherited notices retain Go, musl and its earlier works,
  go-netdb, and NixOS/nixpkgs attribution. Its GPL-licensed crlibm testdata is
  outside this binary's package selection and is not copied into this bundle.
- compress/internal/snapref and compress/zstd/internal/xxhash are selected
  packages; their BSD and MIT notices are included in addition to the complete
  root license. OpenTelemetry's additional Go BSD notice is not truncated.
- The go-digest documentation license is preserved as documentation context.
  It is distinct from the Apache-2.0 license of the Go code.
- Host-installed tools and separately obtained container images are outside
  this binary bundle. Adding their executables/images, a module cache, or new
  build targets requires a separate redistribution review.

## Verification and redistribution

[manifest.json](LICENSES/manifest.json) records each original component,
version, relative source path, scope, and SHA-256. It also records the binary's
module Go checksums (these are distinct from individual license-file hashes).
[SHA256SUMS](LICENSES/SHA256SUMS) covers every deduplicated original document.
The manifest contains no personal absolute cache paths.
LICENSES/.gitattributes disables Git text conversion for the original documents
so their SHA-256 values survive checkouts on Windows and Linux.

Keep LICENSE, NOTICE, this document, and the complete LICENSES directory with
a distributed binary. Upstream license links alone are not substitutes for
the included texts.

From a checkout with a Go tool and the built binary (no module cache required):

    . hack/env.sh
    python3 hack/notices.py
    (cd LICENSES && sha256sum -c SHA256SUMS)

The default check mode verifies the bundled bytes against the reviewed hashes,
binary module identities and go.sum, the manifest, and relative links. It writes
nothing and does not read original module/toolchain license documents. Public
users and CI do not need to obtain the historical Kubernetes oracle modules.

Maintainers with the reviewed Go toolchain and existing original cache can use
python3 hack/notices.py --check-originals to additionally compare every original
document, or python3 hack/notices.py --write to regenerate from those originals.
Both modes fail if an original is missing; neither mode downloads anything.

A changed binary module/version, binary Go version, build target, license
checksum, or replacement module requires a new review. The final release
binary must pass the default check again.
"""


def fail(message):
    raise ValueError(message)


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def escaped(path):
    return "".join("!" + c.lower() if c.isupper() else c for c in path)


def go_output(args, env):
    return subprocess.check_output(["go", *args], cwd=ROOT, env=env, text=True)


def parse_build(text):
    lines = text.splitlines()
    match = re.search(r": (go[^\s]+)$", lines[0] if lines else "")
    if not match:
        fail("Cannot read the binary's Go toolchain version.")
    result = {"go": match[1], "modules": {}, "settings": {}}
    for line in lines[1:]:
        fields = line.split()
        if not fields:
            continue
        if fields[0] == "=>":
            fail("Replacement modules require a separate provenance review.")
        if fields[0] == "dep":
            if len(fields) != 4 or fields[1] in result["modules"]:
                fail("Malformed or duplicate binary dependency.")
            result["modules"][fields[1]] = (fields[2], fields[3])
        elif fields[0] in ("path", "mod"):
            result[fields[0]] = fields[1]
        elif fields[0] == "build":
            key, _, value = fields[1].partition("=")
            result["settings"][key] = value
    return result


def validate_build(build, sums, toolchain=None):
    if build["go"] != GO_VERSION or (toolchain is not None and toolchain != GO_VERSION):
        fail("Go toolchain changed; review its license files before regenerating.")
    if build.get("path") != "shearwater/cmd/sw" or build.get("mod") != "shearwater":
        fail("This notice bundle is for shearwater/cmd/sw.")
    expected = {m: spec[0] for m, spec in AUDITED_MODULES.items()}
    actual = {m: spec[0] for m, spec in build["modules"].items()}
    if actual != expected:
        fail("Binary module/version inventory differs from the reviewed 46 modules.")
    for key, value in (("GOOS", "linux"), ("GOARCH", "amd64"),
                       ("CGO_ENABLED", "0"), ("-buildmode", "exe")):
        if build["settings"].get(key) != value:
            fail("Unreviewed build setting: " + key)
    if build["settings"].get("-tags") or build["settings"].get("GOAMD64", "v1") != "v1":
        fail("Build tags/CPU target changed; review selected package notices.")
    go_sums = {}
    for line in sums.splitlines():
        module, version, checksum = line.split()
        if not version.endswith("/go.mod"):
            go_sums[(module, version)] = checksum
    for module, (version, checksum) in build["modules"].items():
        if not checksum.startswith("h1:") or go_sums.get((module, version)) != checksum:
            fail("Binary checksum does not match go.sum: " + module)


def documents(build):
    result = []
    for module, (version, license_name, file, digest) in sorted(AUDITED_MODULES.items()):
        result.append(dict(kind="module", component=module, version=version,
                           file=file, sha256=digest, role="binary-root",
                           license=license_name,
                           scope="Linked module recorded in the reviewed binary."))
    for kind, component, version, file, digest, scope in SUPPLEMENTAL:
        result.append(dict(kind=kind, component=component, version=version,
                           file=file, sha256=digest, role="supplemental", scope=scope))
    for doc in result:
        doc["bundle"] = "LICENSES/" + doc["sha256"] + ".txt"
        doc["source"] = (
            "$GOROOT/" + doc["file"] if doc["kind"] == "go"
            else "$GOMODCACHE/" + escaped(doc["component"]) + "@" +
                 doc["version"] + "/" + doc["file"]
        )
    return result


def render_bundle(build, settings, *, originals=False):
    docs = documents(build)
    original_bytes = {}
    outputs = {}
    for doc in docs:
        if originals:
            parent = (Path(settings["GOROOT"]) if doc["kind"] == "go" else
                      Path(settings["GOMODCACHE"]) /
                      (escaped(doc["component"]) + "@" + doc["version"]))
            data = (parent / doc["file"]).read_bytes()
        else:
            data = (ROOT / doc["bundle"]).read_bytes()
        if sha256(data) != doc["sha256"]:
            where = doc["source"] if originals else doc["bundle"]
            fail(("Original" if originals else "Bundled") +
                 " license bytes differ from the review: " + where)
        if doc["bundle"] in outputs and outputs[doc["bundle"]] != data:
            fail("Different bytes share a SHA-256 filename.")
        outputs[doc["bundle"]] = data
        original_bytes[(doc["component"], doc["file"])] = data

    manifest = {
        "format": 1, "go_version": GO_VERSION, "package": "shearwater/cmd/sw",
        "project_license": {"path": "LICENSE", "sha256": ROOT_LICENSE_SHA256},
        "target": {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0", "GOAMD64": "v1"},
        "modules": [
            {"module": m, "version": v, "go_checksum": h,
             "documents": sorted({d["bundle"] for d in docs if d["component"] == m})}
            for m, (v, h) in sorted(build["modules"].items())
        ],
        "documents": docs,
    }
    outputs["LICENSES/manifest.json"] = (
        json.dumps(manifest, indent=2, ensure_ascii=False) + "\n"
    ).encode("utf-8")
    outputs["LICENSES/SHA256SUMS"] = "".join(
        name.split("/")[-1][:-4] + "  " + name.split("/")[-1] + "\n"
        for name in sorted(outputs) if name.endswith(".txt")
    ).encode("ascii")
    outputs["LICENSES/.gitattributes"] = (
        "# Preserve the exact upstream license/notice/patent document bytes.\n"
        "# Upstream trailing blank lines are part of the audited bytes, not our style.\n"
        "*.txt -text -whitespace\n"
        "manifest.json text eol=lf\n"
        "SHA256SUMS text eol=lf\n"
        ".gitattributes text eol=lf\n"
    ).encode("ascii")

    notice = NOTICE_PREFIX.encode("utf-8")
    for component, version, filename in (
        ("github.com/containerd/containerd/v2", "v2.3.7", "NOTICE"),
        ("google.golang.org/grpc", "v1.83.2", "NOTICE.txt"),
    ):
        marker = component + "@" + version + "/" + filename
        original = original_bytes[(component, filename)]
        notice += ("\n----- BEGIN " + marker + " -----\n").encode()
        notice += original
        if not original.endswith(b"\n"):
            notice += b"\n"
        notice += ("----- END " + marker + " -----\n").encode()
    outputs["NOTICE"] = notice

    md = THIRD_HEADER
    for module, (version, license_name, _, _) in sorted(AUDITED_MODULES.items()):
        links = "; ".join(
            "[" + d["file"] + "](" + d["bundle"] + ")"
            for d in docs if d["component"] == module
        )
        md += "| " + " | ".join((module, version, "Binary (reviewed Linux/amd64)",
                                 license_name + ". " + links)) + " |\n"
    md += "\n## Supplemental documents and their exact scope\n\n"
    md += "| Component | Version | Scope | Original document |\n|---|---|---|---|\n"
    for doc in docs:
        if doc["role"] != "supplemental":
            continue
        link = "[" + doc["file"] + "](" + doc["bundle"] + ")"
        md += "| " + " | ".join((doc["component"], doc["version"], doc["scope"], link)) + " |\n"
    outputs["THIRD_PARTY_NOTICES.md"] = (md + THIRD_FOOTER).encode("utf-8")
    return outputs, manifest


def compare_outputs(expected, actual):
    if set(expected) != set(actual):
        missing, extra = sorted(expected.keys() - actual.keys()), sorted(actual.keys() - expected.keys())
        fail("Bundle file inventory differs; missing=" + str(missing) + ", unexpected=" + str(extra))
    changed = [p for p in expected if expected[p] != actual[p]]
    if changed:
        fail("Bundle content/checksum mismatch: " + ", ".join(changed))


def check_links(markdown, available):
    links = re.findall(r"\[[^\]\n]+\]\(([^)\s]+)\)", markdown)
    local = {link.split("#", 1)[0] for link in links
             if not link.startswith(("https://", "http://", "#"))}
    missing = local - set(available)
    if missing:
        fail("Broken relative document links: " + ", ".join(sorted(missing)))
    return len(local)


def verify_outputs(expected):
    actual = {}
    for name in ("NOTICE", "THIRD_PARTY_NOTICES.md"):
        path = ROOT / name
        if path.is_file():
            actual[name] = path.read_bytes()
    directory = ROOT / "LICENSES"
    if directory.exists():
        for path in directory.rglob("*"):
            if path.is_file():
                actual[path.relative_to(ROOT).as_posix()] = path.read_bytes()
    compare_outputs(expected, actual)
    available = set(actual)
    if (ROOT / "LICENSE").is_file():
        if sha256((ROOT / "LICENSE").read_bytes()) != ROOT_LICENSE_SHA256:
            fail("Root LICENSE differs from the reviewed Apache-2.0 original; it was not rewritten.")
        available.add("LICENSE")
    count = check_links(actual["THIRD_PARTY_NOTICES.md"].decode("utf-8"), available)
    return count


def write_outputs(outputs):
    directory = ROOT / "LICENSES"
    if directory.is_symlink():
        fail("LICENSES must be a real directory.")
    if directory.exists():
        extra = {p.relative_to(ROOT).as_posix() for p in directory.rglob("*") if p.is_file()} - outputs.keys()
        if extra:
            fail("Unmanaged LICENSES files; nothing was deleted: " + ", ".join(sorted(extra)))
    for name in outputs:
        if (ROOT / name).is_symlink():
            fail("Refusing to overwrite a symlink: " + name)
    directory.mkdir(exist_ok=True)
    for name, data in outputs.items():
        path = ROOT / name
        if not path.exists() or path.read_bytes() != data:
            path.write_bytes(data)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--write", action="store_true", help="copy reviewed bytes and regenerate notices")
    mode.add_argument("--check", action="store_true", help="verify bundled files without needing original caches (default)")
    mode.add_argument("--check-originals", action="store_true", help="also compare existing original cache/toolchain files")
    parser.add_argument("--binary", type=Path, default=ROOT / "dist/sw")
    args = parser.parse_args()
    env = dict(os.environ, GOENV="off", GOWORK="off", GOTOOLCHAIN="local",
               GOPROXY="off", GOSUMDB="off")
    originals = args.write or args.check_originals
    settings = (json.loads(go_output(["env", "-json", "GOMODCACHE", "GOROOT", "GOVERSION"], env))
                if originals else {})
    build = parse_build(go_output(["version", "-m", str(args.binary.resolve())], env))
    validate_build(build, (ROOT / "go.sum").read_text(encoding="utf-8"), settings.get("GOVERSION"))
    outputs, manifest = render_bundle(build, settings, originals=originals)
    if args.write:
        write_outputs(outputs)
    links = verify_outputs(outputs)
    original_count = len(manifest["documents"])
    unique = sum(name.endswith(".txt") for name in outputs)
    binary_hash = sha256(args.binary.read_bytes())
    print(f"PASS: 46 binary module/version/checksum matches; {original_count} reviewed source documents; "
          f"{unique} byte-exact SHA-256 copies; {links} relative link targets.")
    print("PASS: generated notice/manifest/checksum inventory matches; no downloads.")
    print("Mode: " + ("existing original cache/toolchain compared" if originals else
                     "bundle-only; original cache/toolchain documents not read"))
    print("Binary SHA-256: " + binary_hash)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as exc:
        print("notices: " + str(exc), file=sys.stderr)
        sys.exit(1)
