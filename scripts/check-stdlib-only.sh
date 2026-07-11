#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
module="$(go list -m)"
extra="$(go list -m all | awk -v module="$module" '$1 != module { print }')"
[[ -z "$extra" ]] || { echo "Kenogram must remain Go stdlib-only:" >&2; echo "$extra" >&2; exit 1; }
echo "stdlib-only check passed"
