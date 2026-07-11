#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
bin="${KENOGRAM_SMOKE_BIN:-bin/kenogram}"
[[ -x "$bin" ]] || { echo "missing smoke binary: $bin" >&2; exit 1; }
"$bin" version | rg -q '^kenogram '
"$bin" help | rg -q 'up --dry-run'
echo "smoke check passed"
