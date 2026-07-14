#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

required=(
  README.md CHANGELOG.md docs/design.md docs/getting-started.md docs/kenogrammatics.md
  docs/apple-container-machine.md docs/release-strategy.md docs/world-pattern-proposal.md
  docs/compositions/README.md docs/compositions/engram.md
  docs/compositions/openclaw.md docs/compositions/hermes-agent.md
  docs/compositions/ssh.md images/ssh-world/Containerfile images/ssh-world/README.md
  images/reference-world/Containerfile images/reference-world/README.md
  scripts/prepare-first-world.sh requirements/INDEX.md
  requirements/declaration.md requirements/plan.md requirements/operations.md
  requirements/security.md requirements/network.md requirements/lifecycle.md
  requirements/history.md
)
for file in "${required[@]}"; do
  [[ -s "$file" ]] || { echo "missing required file: $file" >&2; exit 1; }
done

if rg -n 'github.com/idolum-ai/kenogram/internal/(app|backend|proxy|worldfs|history)' internal/decl internal/plan >/dev/null 2>&1; then
  echo "pure declaration and plan packages import a stateful package" >&2
  exit 1
fi

echo "architecture check passed"
