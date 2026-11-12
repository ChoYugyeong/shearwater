#!/usr/bin/env bash
# WSL root, using sw built as the normal user after sourcing hack/env.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
exec python3 hack/measure.py "$@"
