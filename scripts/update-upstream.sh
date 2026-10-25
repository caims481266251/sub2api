#!/usr/bin/env bash
set -euo pipefail
exec python3 "$(cd -- "$(dirname -- "$0")" && pwd)/update_upstream.py" "$@"
