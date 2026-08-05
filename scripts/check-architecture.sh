#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

required=(
  README.md CHANGELOG.md docs/design.md docs/getting-started.md docs/kenogrammatics.md
  docs/apple-container-machine.md docs/release-strategy.md docs/world-pattern-proposal.md
  docs/compositions/README.md docs/compositions/engram.md
  docs/compositions/openclaw.md docs/compositions/hermes-agent.md
  docs/compositions/ssh.md docs/compositions/readiness-wrapper.md
  images/ssh-world/Containerfile images/ssh-world/README.md
  images/reference-world/Containerfile images/reference-world/README.md
  scripts/prepare-first-world.sh requirements/INDEX.md
  requirements/declaration.md requirements/plan.md requirements/operations.md
  requirements/security.md requirements/network.md requirements/lifecycle.md
  requirements/history.md requirements/jobs.md requirements/provenance.md
  schemas/kenogram.job-request.v1.schema.json
  schemas/kenogram.job-result.v1.schema.json
  schemas/kenogram.job-evidence-manifest.v1.schema.json
  schemas/kenogram.executable-provenance.v1.schema.json
  schemas/kenogram.podman-runtime-observation.v1.schema.json
  schemas/kenogram.job-egress-evidence.v1.schema.json
)
for file in "${required[@]}"; do
  [[ -s "$file" ]] || { echo "missing required file: $file" >&2; exit 1; }
done

if rg -n 'github.com/idolum-ai/kenogram/internal/(app|backend|proxy|worldfs|history)' internal/decl internal/plan internal/jobcontract >/dev/null 2>&1; then
  echo "pure declaration, plan, or job-contract package imports a stateful package" >&2
  exit 1
fi

if rg -n 'github.com/idolum-ai/kenogram/internal/(backend|proxy|worldfs|history|netns)' internal/job >/dev/null 2>&1; then
  echo "provider-independent job core imports runtime or persistent-world implementation" >&2
  exit 1
fi

echo "architecture check passed"
