#!/usr/bin/env bash
# The same non-mutating check runs locally and in CI. Formatting differences
# fail with filenames; an unexpected test failure must never edit source code.
#   bash hack/t.sh [./internal/patch/ ...]
set -euo pipefail
cd "$(dirname "$0")/.."
. hack/env.sh
[ $# -gt 0 ] || set -- ./...
bad=$(gofmt -l cmd internal)
if [ -n "$bad" ]; then
  printf 'gofmt needed (run gofmt -w on these files):\n%s\n' "$bad" >&2
  exit 1
fi
go vet "$@"
# Keep the complete failure output and the real exit status. Piping through
# head used to truncate diagnostics and could send SIGPIPE to the test runner.
go test "$@"
