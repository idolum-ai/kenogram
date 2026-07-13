#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
rg -q 'Status: binding contract' requirements/declaration.md
rg -q 'Evidence and open boundaries' requirements/INDEX.md
rg -q 'make integration' README.md requirements/INDEX.md
rg -q 'make e2e' README.md requirements/INDEX.md
rg -q 'not a morphogrammatic calculus' docs/kenogrammatics.md
for evidence in \
  'make e2e-release' \
  'make e2e-openclaw' \
  'make e2e-composition' \
  'make e2e-hermes' \
  'make e2e-hermes-composition' \
  'v0.3.0' \
  '2026.6.11' \
  'v2026.7.7.2'; do
  rg -Fq "$evidence" requirements/INDEX.md
done
echo "docs freshness check passed"
