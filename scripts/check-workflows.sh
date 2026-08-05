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
version_file_count="$(rg -o 'go-version-file:[[:space:]]+(source/)?go.mod' .github/workflows | wc -l)"
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

bash scripts/test-ci-policy.sh
for phrase in 'merge_group:' 'fetch-depth: 0' 'github.workflow_sha' \
  'PATH_AWARE_CI_ENABLED' \
  '../.ci-policy/scripts/classify-ci-paths.sh' \
  '.ci-policy/scripts/verify-ci-results.sh' \
  'path: .ci-policy' 'path: source' 'persist-credentials: false' \
  "if: steps.scope.outputs.mode == 'editorial'" \
  "if: needs.check.outputs.mode == 'full'" 'if: always()' \
  'needs: [check, race, apple-host, runtime, runtime-hermes]'; do
  grep -F -- "${phrase}" .github/workflows/ci.yml >/dev/null || {
    echo "path-aware CI is missing: ${phrase}" >&2; exit 1;
  }
done

for phrase in 'persist-credentials: false' './scripts/prepare-release-notes.sh' 'make vulncheck' 'make test-race' 'make integration' 'make release-dist' \
  'commit="$(git rev-parse HEAD)"' 'git show -s --format=%ct HEAD' \
  'candidate-smoke/kenogram version --json' 'KENOGRAM_INTEGRATION_BINARY=' \
  'candidate-version.txt' 'candidate-provenance.json'; do
  grep -F -- "${phrase}" .github/workflows/release-candidate.yml >/dev/null || {
    echo "candidate workflow is missing: ${phrase}" >&2; exit 1;
  }
done
for phrase in 'environment: release' 'contents: write' 'persist-credentials: false' 'make vulncheck' \
  'name: Check out candidate-reviewed head' 'ref: ${{ github.event.pull_request.head.sha }}' \
  'path: .reviewed-release-head' "git -C .reviewed-release-head rev-parse 'HEAD^{tree}'" \
  'merged release tree differs from the candidate-reviewed head' 'git push origin "${SOURCE_SHA}:refs/tags/${TAG}"' \
  'commit="$(git rev-parse "${RELEASE_MERGE_SHA}")"' 'git show -s --format=%ct "${RELEASE_MERGE_SHA}"' \
  'release-smoke/kenogram version --json' \
  'KENOGRAM_INTEGRATION_BINARY=' 'release-provenance.json' \
  '--verify-tag --draft' 'gh release upload' '--draft=false'; do
  grep -F -- "${phrase}" .github/workflows/release.yml >/dev/null || {
    echo "release workflow is missing: ${phrase}" >&2; exit 1;
  }
done
if rg -n 'git rev-parse --short' .github/workflows/release-candidate.yml .github/workflows/release.yml; then
  echo "release workflows must retain the full source commit" >&2
  exit 1
fi
if rg -n 'git show -s --format=%cI' .github/workflows/release-candidate.yml .github/workflows/release.yml; then
  echo "release workflows must canonicalize source dates to UTC" >&2
  exit 1
fi
if grep -F -- 'git fetch --no-tags origin "refs/pull/' .github/workflows/release.yml >/dev/null; then
  echo "release verification must not depend on a credentialless pull-ref fetch" >&2
  exit 1
fi
if grep -R -E 'uses:[[:space:]]+actions/(checkout|setup-go|upload-artifact|download-artifact)@v[0-9]+' .github/workflows >/dev/null; then
  echo "official actions must be pinned by full commit SHA" >&2
  exit 1
fi
echo "workflow sanity check passed"
