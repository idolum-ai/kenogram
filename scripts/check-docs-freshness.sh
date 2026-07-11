#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
rg -q 'Status: implemented' requirements/declaration.md
rg -q 'Status: implemented' requirements/network.md
rg -q 'make integration' README.md requirements/INDEX.md
echo "docs freshness check passed"
