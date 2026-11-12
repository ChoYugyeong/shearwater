# Source this from the repository before building/testing: . hack/env.sh
# Respect the caller's PATH (including CI). The local directories are only a
# fallback for developers who unpacked tools without changing their profile.
if ! command -v go >/dev/null 2>&1 && [ -x "$HOME/.local/go/bin/go" ]; then
  PATH="$HOME/.local/go/bin:$PATH"
fi
if ! command -v kubectl >/dev/null 2>&1 && [ -x "$HOME/.local/bin/kubectl" ]; then
  PATH="$HOME/.local/bin:$PATH"
fi
export PATH
# The normal binary needs no C compiler. A developer can explicitly opt into
# CGO_ENABLED=1 for the Go race detector; sourcing this file preserves it.
export CGO_ENABLED="${CGO_ENABLED:-0}"
# Windows-owned checkouts on /mnt/c may fail Git's ownership check. Do not
# globally disable provenance: ordinary Linux checkouts retain Go's VCS stamp.
# Also preserve caller-provided GOFLAGS, even an explicitly empty value.
if [ -z "${GOFLAGS+x}" ] && command -v git >/dev/null 2>&1 &&
   ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  export GOFLAGS=-buildvcs=false
fi
