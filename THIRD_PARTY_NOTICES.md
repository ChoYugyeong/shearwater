# Third-party notices for shearwater

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
| github.com/cespare/xxhash/v2 | v2.3.0 | Binary (reviewed Linux/amd64) | MIT. [LICENSE.txt](LICENSES/f566a9f97bacdaf00d9f21dd991e81dc11201c4e016c86b470799429a1c9a79c.txt) |
| github.com/containerd/containerd/api | v1.11.1 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt) |
| github.com/containerd/containerd/v2 | v2.3.7 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt); [NOTICE](LICENSES/03f0b4bcaecfdb491ef5a7d320e2c2adf886c732d34e3fff87d52dbaec64d8c3.txt) |
| github.com/containerd/continuity | v0.5.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt) |
| github.com/containerd/errdefs | v1.0.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt) |
| github.com/containerd/errdefs/pkg | v0.3.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt) |
| github.com/containerd/fifo | v1.1.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4.txt) |
| github.com/containerd/log | v0.1.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt) |
| github.com/containerd/platforms | v1.0.0-rc.5 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt) |
| github.com/containerd/plugin | v1.1.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt) |
| github.com/containerd/ttrpc | v1.2.8 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4.txt) |
| github.com/containerd/typeurl/v2 | v2.2.3 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/4bbe3b885e8cd1907ab4cf9a41e862e74e24b5422297a4f2fe524e6a30ada2b4.txt) |
| github.com/distribution/reference | v0.6.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/cb5e8e7e5f4a3988e1063c142c60dc2df75605f4c46515e776e3aca6df976e14.txt) |
| github.com/dustin/go-humanize | v1.1.0 | Binary (reviewed Linux/amd64) | MIT. [LICENSE](LICENSES/a973b4498c13eb74baa2a8e5c351426a6826f2fcdd909916dbe53ee2e755fd71.txt) |
| github.com/felixge/httpsnoop | v1.0.4 | Binary (reviewed Linux/amd64) | MIT. [LICENSE.txt](LICENSES/8108e14b5cb2b6bcd51583d3849ef0c2b5c4de61758256c20308bc70d1775dbb.txt) |
| github.com/go-logr/logr | v1.4.3 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/b40930bbcf80744c86c46a12bc9da056641d722716c378f5659b9e555ef833e1.txt) |
| github.com/go-logr/stdr | v1.2.2 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4.txt) |
| github.com/gogo/protobuf | v1.3.2 | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/3f571768c0ba2f330e5fee90de89bfe5141f48c1939be8008fa61461c30ca07f.txt) |
| github.com/google/uuid | v1.6.0 | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/0a8d61ed3cbfd5312326e8126c31ce9c627a283adc99131b56896d29ada04b2d.txt) |
| github.com/klauspost/compress | v1.18.5 | Binary (reviewed Linux/amd64) | BSD-3-Clause; path-specific MIT/Apache-2.0 notices. [LICENSE](LICENSES/0d9e582ee4bff57bf1189c9e514e6da7ce277f9cd3bc2d488b22fbb39a6d87cf.txt); [internal/snapref/LICENSE](LICENSES/f69f157b0be75da373605dbc8bbf142e8924ee82d8f44f11bcaf351335bf98cf.txt); [zstd/internal/xxhash/LICENSE.txt](LICENSES/f566a9f97bacdaf00d9f21dd991e81dc11201c4e016c86b470799429a1c9a79c.txt) |
| github.com/moby/locker | v1.0.1 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/9b44ae21e39b0ec20e89984bfb437d20617236285c051c42d95672babe2dad62.txt) |
| github.com/moby/sys/mountinfo | v0.7.2 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30.txt) |
| github.com/moby/sys/signal | v0.7.1 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30.txt) |
| github.com/moby/sys/user | v0.4.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30.txt) |
| github.com/moby/sys/userns | v0.1.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30.txt) |
| github.com/opencontainers/go-digest | v1.0.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/aceb55133c41c50c2e170c5918e301fe74f592d0f28724bf2f7a7bfdcff1fc5f.txt); [LICENSE.docs](LICENSES/a1d1adec3ba691ef6e377a78bbe9e9e2501f2f4a1723d08857da1ac6564e4fed.txt) |
| github.com/opencontainers/image-spec | v1.1.1 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/b0a3f39513927db306adabea11d14c23f079d4febcea241d123a68d1a0d45418.txt) |
| github.com/opencontainers/runtime-spec | v1.3.0 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/a964a27c435167bdd78827ff83e4bd4473eca53d583a39598d2f445894a9225d.txt) |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748-24d4a6f8daec | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/dd26a7abddd02e2d0aba97805b31f248ef7835d9e10da289b22e3b8ab78b324d.txt) |
| github.com/sirupsen/logrus | v1.9.4 | Binary (reviewed Linux/amd64) | MIT. [LICENSE](LICENSES/51a0c9ec7f8b7634181b8d4c03e5b5d204ac21d6e72f46c313973424664b2e6b.txt) |
| go.opentelemetry.io/auto/sdk | v1.2.1 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4.txt) |
| go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp | v0.68.0 | Binary (reviewed Linux/amd64) | Apache-2.0; additional Go BSD-3-Clause notice. [LICENSE](LICENSES/1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40.txt) |
| go.opentelemetry.io/otel | v1.44.0 | Binary (reviewed Linux/amd64) | Apache-2.0; additional Go BSD-3-Clause notice. [LICENSE](LICENSES/1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40.txt) |
| go.opentelemetry.io/otel/metric | v1.44.0 | Binary (reviewed Linux/amd64) | Apache-2.0; additional Go BSD-3-Clause notice. [LICENSE](LICENSES/1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40.txt) |
| go.opentelemetry.io/otel/trace | v1.44.0 | Binary (reviewed Linux/amd64) | Apache-2.0; additional Go BSD-3-Clause notice. [LICENSE](LICENSES/1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40.txt) |
| golang.org/x/net | v0.58.0 | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad.txt); [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| golang.org/x/sync | v0.24.0 | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad.txt); [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| golang.org/x/sys | v0.49.0 | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad.txt); [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| golang.org/x/text | v0.41.0 | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad.txt); [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| google.golang.org/genproto/googleapis/rpc | v0.0.0-20260526163538-3dc84a4a5aaa | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30.txt) |
| google.golang.org/grpc | v1.83.2 | Binary (reviewed Linux/amd64) | Apache-2.0. [LICENSE](LICENSES/cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30.txt); [NOTICE.txt](LICENSES/693ff28ec216d5112ac1bbfe64ef539005867d1c7bd427b57d579683293b947f.txt) |
| google.golang.org/protobuf | v1.36.12-0.20260120151049-f2248ac996af | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/4835612df0098ca95f8e7d9e3bffcb02358d435dbb38057c844c99d7f725eb20.txt); [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| modernc.org/libc | v1.80.1 | Binary (reviewed Linux/amd64) | BSD-3-Clause; inherited MIT/BSD notices. [LICENSE](LICENSES/95ff867eb55a56935fa7492406cfa953fb7c13ca73f4c0a86ae05756b4605600.txt); [LICENSE-3RD-PARTY.md](LICENSES/f597097efe3d97021f89170746bd3a0fb9a8b6fb26b82043ed68a4e0283bee6c.txt) |
| modernc.org/mathutil | v1.7.1 | Binary (reviewed Linux/amd64) | BSD-3-Clause. [LICENSE](LICENSES/bfa9bf72a72ca009fd62a8f84fca3dca67e51d93af96352723646599898b6cf5.txt); [mersenne/LICENSE](LICENSES/c05630f9ca899ea094620d37daea6ba66f99adbf9268ea5fea9db607e1102bd9.txt) |
| modernc.org/memory | v1.12.1 | Binary (reviewed Linux/amd64) | BSD-3-Clause; inherited Go/mmap-go notices. [LICENSE](LICENSES/59895e669f48f168b6b858358f6005779cdf40a265f7828813061b56af67b496.txt); [LICENSE-GO](LICENSES/2d36597f7117c38b006835ae7f537487207d8ec407aa9d9980794b2030cbc067.txt); [LICENSE-MMAP-GO](LICENSES/c2eba69f20d05414538c3a5df7694dde392e065ff70882e1625e90f5d6659fff.txt); [LICENSE-LOGO](LICENSES/5ae5bee3072a841376451b48d8cfcec7188e10543926d5870828d36c8a750dc5.txt) |
| modernc.org/sqlite | v1.62.0 | Binary (reviewed Linux/amd64) | BSD-3-Clause; SQLite public domain; inherited notices. [LICENSE](LICENSES/c6fe05491a60ae13bcd223088d2705e36dede24e5587226231d2459ada5c4822.txt); [LICENSE-SQLITE](LICENSES/8438c9c89b849131ead81d5435cb97fcf052df5b0b286dda8a2d4c29e6cb3fd0.txt); [LICENSE-3RD-PARTY.md](LICENSES/f4a00ade8f4057ab0ea49097d9c459df8559fcf10ca78da0c950343fb7428d37.txt); [LICENSE-SQLITE_VEC](LICENSES/6ce72bbe12d975bd5286e5ab0a064c069693300c47bccbc57bec18485f1621ea.txt) |

## Supplemental documents and their exact scope

| Component | Version | Scope | Original document |
|---|---|---|---|
| github.com/containerd/containerd/v2 | v2.3.7 | Binary: inherited containerd/Docker notice, reproduced verbatim in NOTICE. | [NOTICE](LICENSES/03f0b4bcaecfdb491ef5a7d320e2c2adf886c732d34e3fff87d52dbaec64d8c3.txt) |
| google.golang.org/grpc | v1.83.2 | Binary: gRPC notice, reproduced verbatim in NOTICE. | [NOTICE.txt](LICENSES/693ff28ec216d5112ac1bbfe64ef539005867d1c7bd427b57d579683293b947f.txt) |
| golang.org/x/sys | v0.49.0 | Additional upstream patent grant retained with the binary notices. | [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| golang.org/x/net | v0.58.0 | Additional upstream patent grant retained with the binary notices. | [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| golang.org/x/sync | v0.24.0 | Additional upstream patent grant retained with the binary notices. | [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| golang.org/x/text | v0.41.0 | Additional upstream patent grant retained with the binary notices. | [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| google.golang.org/protobuf | v1.36.12-0.20260120151049-f2248ac996af | Additional upstream patent grant retained with the binary notices. | [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| github.com/klauspost/compress | v1.18.5 | Binary: internal/snapref is used by the selected zstd packages; Snappy-Go BSD notice. | [internal/snapref/LICENSE](LICENSES/f69f157b0be75da373605dbc8bbf142e8924ee82d8f44f11bcaf351335bf98cf.txt) |
| github.com/klauspost/compress | v1.18.5 | Binary: selected zstd/internal/xxhash implementation; MIT notice. | [zstd/internal/xxhash/LICENSE.txt](LICENSES/f566a9f97bacdaf00d9f21dd991e81dc11201c4e016c86b470799429a1c9a79c.txt) |
| github.com/opencontainers/go-digest | v1.0.0 | Documentation-only CC-BY-SA-4.0 license retained as upstream context; not the Go code license. No upstream documentation is incorporated into the program by this entry. | [LICENSE.docs](LICENSES/a1d1adec3ba691ef6e377a78bbe9e9e2501f2f4a1723d08857da1ac6564e4fed.txt) |
| modernc.org/libc | v1.80.1 | Binary/inherited: Go, musl (including its prior-work notices), go-netdb and NixOS/nixpkgs. This complete upstream document preserves the original scope. | [LICENSE-3RD-PARTY.md](LICENSES/f597097efe3d97021f89170746bd3a0fb9a8b6fb26b82043ed68a4e0283bee6c.txt) |
| modernc.org/mathutil | v1.7.1 | Supplemental module-source notice. The mersenne subpackage was not selected for this binary. | [mersenne/LICENSE](LICENSES/c05630f9ca899ea094620d37daea6ba66f99adbf9268ea5fea9db607e1102bd9.txt) |
| modernc.org/memory | v1.12.1 | Binary/inherited Go copyright and BSD terms. | [LICENSE-GO](LICENSES/2d36597f7117c38b006835ae7f537487207d8ec407aa9d9980794b2030cbc067.txt) |
| modernc.org/memory | v1.12.1 | Binary/inherited mmap-go copyright and BSD terms. | [LICENSE-MMAP-GO](LICENSES/c2eba69f20d05414538c3a5df7694dde392e065ff70882e1625e90f5d6659fff.txt) |
| modernc.org/memory | v1.12.1 | Upstream logo attribution URL only, retained as a reference. It is not a verified image license; no logo is bundled. | [LICENSE-LOGO](LICENSES/5ae5bee3072a841376451b48d8cfcec7188e10543926d5870828d36c8a750dc5.txt) |
| modernc.org/sqlite | v1.62.0 | Binary: original SQLite public-domain dedication, separate from the Go driver's BSD license. | [LICENSE-SQLITE](LICENSES/8438c9c89b849131ead81d5435cb97fcf052df5b0b286dda8a2d4c29e6cb3fd0.txt) |
| modernc.org/sqlite | v1.62.0 | Complete upstream inventory, including runtime, test, graph-only and source-only sections. Its MPL/test entries are not claims that those components are linked into sw. | [LICENSE-3RD-PARTY.md](LICENSES/f4a00ade8f4057ab0ea49097d9c459df8559fcf10ca78da0c950343fb7428d37.txt) |
| modernc.org/sqlite | v1.62.0 | Supplemental MIT notice for optional sqlite-vec. Neither sqlite/vec nor sqlite/vfs was selected for this binary. | [LICENSE-SQLITE_VEC](LICENSES/6ce72bbe12d975bd5286e5ab0a064c069693300c47bccbc57bec18485f1621ea.txt) |
| Go | go1.27.3 | Binary: Go runtime and standard-library BSD-3-Clause license. | [LICENSE](LICENSES/911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad.txt) |
| Go | go1.27.3 | Go toolchain: additional upstream patent grant. | [PATENTS](LICENSES/96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc.txt) |
| k8s.io/api | v0.36.6 | Source-derived Kubernetes v1.36.6 schema/type metadata and fixture provenance only; not a linked module. | [LICENSE](LICENSES/cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30.txt) |
| k8s.io/apimachinery | v0.36.6 | Adapted strategic-merge behavior, protobuf envelope/schema and fixture oracle provenance only; not a linked module. | [LICENSE](LICENSES/cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30.txt) |

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
