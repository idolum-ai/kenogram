#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
rg -q 'Status: binding contract' requirements/declaration.md
rg -q 'Evidence and open boundaries' requirements/INDEX.md
rg -q 'make integration' README.md requirements/INDEX.md
rg -q 'make e2e' README.md requirements/INDEX.md
rg -q 'not a morphogrammatic calculus' docs/kenogrammatics.md
rg -q 'candidate-reviewed tree' docs/release-strategy.md
rg -q 'scripts/install-release.sh' README.md docs/release-strategy.md
echo "docs freshness check passed"
