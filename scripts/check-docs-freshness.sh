#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
rg -q 'Status: binding contract' requirements/declaration.md
rg -q 'Evidence and known limits' requirements/INDEX.md
for posture in 'Accepted for v0.x' 'Before stable' 'Experimental'; do
  rg -Fq "${posture}" requirements/INDEX.md
done
rg -q 'make integration' README.md requirements/INDEX.md
rg -q 'make e2e' README.md requirements/INDEX.md
rg -q 'first-world guide' README.md
rg -q 'kenogram doctor' README.md docs/getting-started.md requirements/INDEX.md
rg -q 'network-diagnostics' README.md docs/getting-started.md requirements/operations.md requirements/network.md requirements/security.md requirements/INDEX.md
rg -q 'sensitive operator metadata' README.md requirements/operations.md requirements/security.md
rg -q 'prepare-first-world.sh' README.md docs/getting-started.md docs/release-strategy.md
rg -q 'exact local image ID' docs/getting-started.md requirements/declaration.md
rg -q 'not a morphogrammatic calculus' docs/kenogrammatics.md
rg -Fq 'Natural Numbers in Trans-Classic' docs/kenogrammatics.md
rg -Fq 'Morphogrammatik: Eine Einführung' docs/kenogrammatics.md
rg -Fq 'Morphogrammatics for' docs/kenogrammatics.md
rg -q 'Status: design proposal' docs/world-pattern-proposal.md
rg -q 'candidate-reviewed tree' docs/release-strategy.md
rg -q 'scripts/install-release.sh' README.md docs/release-strategy.md
rg -q 'macos-26' .github/workflows/ci.yml docs/apple-container-machine.md
rg -q 'do not support nested virtualization' docs/apple-container-machine.md
rg -q 'shell-inert argv' README.md CONTRIBUTING.md docs/apple-container-machine.md requirements/INDEX.md
rg -q 'signal forwarding' CONTRIBUTING.md docs/apple-container-machine.md requirements/INDEX.md
rg -q 'testRunCommandInShell' docs/apple-container-machine.md
for evidence in \
  'make e2e-ssh' \
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
for guide in docs/compositions/README.md docs/compositions/ssh.md docs/compositions/engram.md docs/compositions/openclaw.md docs/compositions/hermes-agent.md; do
  test -s "${guide}"
done
e2e_count="$(awk '/^e2e:/ { print NF - 1; exit }' Makefile)"
test "$e2e_count" -eq 6
rg -Fq '`make e2e` runs all six' requirements/INDEX.md
rg -Fq 'KENOGRAM_E2E_VFS_MIN_FREE_GIB' CONTRIBUTING.md
rg -Fq 'vfsMinimumFreeHermesGiB = uint64(96)' internal/e2e/container_storage_test.go
rg -Fq 'Hermes lanes require 96 GiB free' CONTRIBUTING.md
rg -Fq 'SSH, Engram, and OpenClaw do not yet have' CONTRIBUTING.md
rg -Fq 'never force image removal' requirements/INDEX.md
rg -q 'cleanupOverallTimeout[[:space:]]*=[[:space:]]*2 \* time.Minute' internal/e2e/container_storage_test.go
rg -q 'imageRemove:[[:space:]]*90 \* time.Second' internal/e2e/container_storage_test.go
rg -Fq 'inside a two-minute overall cleanup budget' CONTRIBUTING.md
container_e2e_inventory=(
  'engram_release_test.go:e2eLaneEngram'
  'openclaw_test.go:e2eLaneOpenClaw'
  'engram_openclaw_test.go:e2eLaneOpenClaw'
  'telegram_canary_test.go:e2eLaneOpenClaw'
  'hermes_test.go:e2eLaneHermes'
  'engram_hermes_test.go:e2eLaneHermes'
  'ssh_test.go:e2eLaneSSH'
)
test "$(rg -l 'prepareContainerE2E\(t, ctx,' internal/e2e/*_test.go | wc -l)" -eq "${#container_e2e_inventory[@]}"
for entry in "${container_e2e_inventory[@]}"; do
  source="internal/e2e/${entry%%:*}"
  lane="${entry##*:}"
  test "$(rg -c "prepareContainerE2E\\(t, ctx, $lane\\)" "$source")" -eq 1
  test "$(rg -c 'runImageAcquisition\(t, ctx, resources,' "$source")" -eq 1
  tmp_line="$(rg -n -m1 'tmp := t.TempDir\(\)' "$source" | cut -d: -f1)"
  prepare_line="$(rg -n -m1 'prepareContainerE2E\(t, ctx,' "$source" | cut -d: -f1)"
  test "$tmp_line" -lt "$prepare_line"
done
lifecycle_checkpoint_count="$(sed -n '/var lifecycleCrashCheckpoints = \[\]string{/,/^}/p' internal/app/lifecycle_crash_test.go | rg -o '"[^"]+"' | wc -l)"
test "$lifecycle_checkpoint_count" -eq 15
rg -Fq 'fifteen lifecycle boundaries' requirements/lifecycle.md
if rg -iq 'make e2e.{0,40}(three proofs|runs all three|runs all five)' requirements/INDEX.md README.md; then
  echo "stale make e2e proof count" >&2
  exit 1
fi
echo "docs freshness check passed"
