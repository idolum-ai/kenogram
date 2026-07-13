#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
if find .github/workflows -type f \( -name '*.yml' -o -name '*.yaml' \) -print0 | xargs -0 -r grep -n $'\t'; then
  echo "workflow files must not contain tabs" >&2
  exit 1
fi
if git grep -n -E '^(<<<<<<<|=======|>>>>>>>)' -- . ':!bin' >/dev/null; then
  echo "merge conflict marker found" >&2
  exit 1
fi
bash -n scripts/*.sh

for phrase in 'persist-credentials: false' './scripts/prepare-release-notes.sh' 'make test-race' 'make integration' 'make release-dist' 'candidate-version.txt'; do
  grep -F -- "${phrase}" .github/workflows/release-candidate.yml >/dev/null || {
    echo "candidate workflow is missing: ${phrase}" >&2; exit 1;
  }
done
for phrase in 'environment: release' 'contents: write' 'persist-credentials: false' \
  'merged release tree differs from the candidate-reviewed head' 'git push origin "${SOURCE_SHA}:refs/tags/${TAG}"' \
  '--verify-tag --draft' 'gh release upload' '--draft=false'; do
  grep -F -- "${phrase}" .github/workflows/release.yml >/dev/null || {
    echo "release workflow is missing: ${phrase}" >&2; exit 1;
  }
done
if grep -R -E 'uses:[[:space:]]+actions/(checkout|setup-go|upload-artifact|download-artifact)@v[0-9]+' .github/workflows >/dev/null; then
  echo "official actions must be pinned by full commit SHA" >&2
  exit 1
fi
echo "workflow sanity check passed"
