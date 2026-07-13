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
e2e_count="$(awk '/^e2e:/ { print NF - 1; exit }' Makefile)"
test "$e2e_count" -eq 5
rg -Fq '`make e2e` runs all five' requirements/INDEX.md
rg -Fq 'KENOGRAM_E2E_VFS_MIN_FREE_GIB' CONTRIBUTING.md
for floor in \
  'vfsMinimumFreeEngramGiB   = uint64(24)' \
  'vfsMinimumFreeOpenClawGiB = uint64(64)' \
  'vfsMinimumFreeHermesGiB   = uint64(96)'; do
  rg -Fq "$floor" internal/e2e/container_storage_test.go
done
rg -Fq '24 GiB for the Engram release proof, 64 GiB for' CONTRIBUTING.md
rg -Fq 'OpenClaw proofs, and 96 GiB for Hermes proofs' CONTRIBUTING.md
rg -Fq 'never force image removal' requirements/INDEX.md
container_e2e_sources="$(rg -l 'prepareContainerE2E\(t, ctx,' internal/e2e/*_test.go)"
test "$(wc -l <<<"$container_e2e_sources")" -eq 6
while IFS= read -r source; do
  rg -q 'claimImage\(t, ctx,' "$source"
  tmp_line="$(rg -n -m1 'tmp := t.TempDir\(\)' "$source" | cut -d: -f1)"
  prepare_line="$(rg -n -m1 'prepareContainerE2E\(t, ctx,' "$source" | cut -d: -f1)"
  test "$tmp_line" -lt "$prepare_line"
done <<<"$container_e2e_sources"
lifecycle_checkpoint_count="$(sed -n '/var lifecycleCrashCheckpoints = \[\]string{/,/^}/p' internal/app/lifecycle_crash_test.go | rg -o '"[^"]+"' | wc -l)"
test "$lifecycle_checkpoint_count" -eq 14
rg -Fq 'fourteen lifecycle boundaries' requirements/lifecycle.md
if rg -iq 'make e2e.{0,40}(three proofs|runs all three)' requirements/INDEX.md README.md; then
  echo "stale make e2e proof count" >&2
  exit 1
fi
echo "docs freshness check passed"
