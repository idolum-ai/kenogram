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

toolchain="$(awk '$1 == "toolchain" { print $2 }' go.mod)"
if [[ ! "${toolchain}" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "go.mod must pin one exact Go toolchain patch" >&2
  exit 1
fi
setup_go_count="$(rg -o 'actions/setup-go@' .github/workflows | wc -l)"
version_file_count="$(rg -o 'go-version-file:[[:space:]]+go.mod' .github/workflows | wc -l)"
if [[ "${setup_go_count}" -ne "${version_file_count}" ]]; then
  echo "every setup-go step must consume go.mod's toolchain directive" >&2
  exit 1
fi
if rg -q 'go-version:' .github/workflows; then
  echo "workflow Go versions must not bypass go.mod's toolchain directive" >&2
  exit 1
fi
grep -F -- 'run: make vulncheck' .github/workflows/ci.yml >/dev/null || {
  echo "CI must gate changes and scheduled runs on vulnerability reachability" >&2
  exit 1
}

for phrase in 'persist-credentials: false' './scripts/prepare-release-notes.sh' 'make vulncheck' 'make test-race' 'make integration' 'make release-dist' 'candidate-version.txt'; do
  grep -F -- "${phrase}" .github/workflows/release-candidate.yml >/dev/null || {
    echo "candidate workflow is missing: ${phrase}" >&2; exit 1;
  }
done
for phrase in 'environment: release' 'contents: write' 'persist-credentials: false' 'make vulncheck' \
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
