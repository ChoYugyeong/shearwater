#!/usr/bin/env bash
# Build a Linux binary without installing it or starting a privileged process.
# Usage: bash hack/build.sh [output-file]; default output is dist/sw.
set -euo pipefail
cd "$(dirname "$0")/.."
. hack/env.sh
[[ $# -le 1 ]] || { echo 'usage: bash hack/build.sh [output-file]' >&2; exit 2; }
output=${1:-dist/sw}
[[ $(go env GOOS) == linux ]] || {
  echo 'Build in Linux/WSL, or explicitly set GOOS=linux for a cross build.' >&2
  exit 1
}
mkdir -p -- "$(dirname -- "$output")"
# trimpath removes developer-specific absolute source paths from the binary.
# Do not strip symbols here: the alpha should remain straightforward to debug.
CGO_ENABLED=0 go build -trimpath -o "$output" ./cmd/sw
printf 'Built %s\n' "$output"
sha256sum "$output"
go version -m "$output"
