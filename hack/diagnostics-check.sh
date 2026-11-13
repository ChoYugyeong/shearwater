#!/usr/bin/env bash
# Real failures are created only in an EMPTY, ISOLATED containerd/network.
# Do not run this on a server that owns an application you want to keep.
# Usage: sudo bash hack/diagnostics-check.sh /absolute/sw /absolute/kubectl
# SW_CONTAINERD_SOCKET selects an isolated daemon; a socket alone does not
# isolate the network. See docs/troubleshooting.md for the full precautions.
set -euo pipefail
[[ $# == 2 && $EUID == 0 && $1 == /* && $2 == /* && -x $1 && -x $2 ]] || {
  echo 'usage (root, isolated host): diagnostics-check.sh /absolute/sw /absolute/kubectl' >&2
  exit 2
}
# Isolated Python ignores PYTHONOPTIMIZE/PYTHONPATH so environment variables
# cannot disable the guard assertions or substitute imported modules.
exec python3 -I "$(dirname "$0")/diagnostics_check.py" "$1" "$2"
