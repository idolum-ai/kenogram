#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
rg -q 'Status: implemented at M1' requirements/declaration.md
rg -q 'no networking is implemented at M1' requirements/network.md
rg -q 'does \*\*not\*\* yet create containers' README.md
echo "docs freshness check passed"
