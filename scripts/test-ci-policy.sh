#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
classifier="${repo_root}/scripts/classify-ci-paths.sh"
selector="${repo_root}/scripts/select-ci-mode.sh"
verifier="${repo_root}/scripts/verify-ci-results.sh"

classify_paths() {
  printf '%s\0' "$@" | bash "${classifier}"
}
[[ "$(classify_paths README.md requirements/security.md docs/assets/kenogram-mark.svg)" = editorial ]]
[[ "$(classify_paths .github/ISSUE_TEMPLATE/bug.yml)" = editorial ]]
[[ "$(classify_paths README.md internal/app/app.go)" = full ]]
[[ "$(classify_paths internal/e2e/testdata/prompt.md)" = full ]]
[[ "$(classify_paths docs/example.sh)" = full ]]
[[ "$(classify_paths images/reference-world/Containerfile)" = full ]]
[[ "$(classify_paths .github/workflows/ci.yml)" = full ]]
[[ "$(classify_paths scripts/classify-ci-paths.sh)" = full ]]
[[ "$(bash "${classifier}" </dev/null)" = full ]]

bash "${verifier}" editorial success skipped skipped skipped skipped
bash "${verifier}" full success success success success success
if bash "${verifier}" editorial success success skipped skipped skipped >/dev/null 2>&1; then
  echo "editorial CI accepted an executed race job" >&2
  exit 1
fi
if bash "${verifier}" full success success success failure success >/dev/null 2>&1; then
  echo "full CI accepted a failed runtime job" >&2
  exit 1
fi

test_repo="$(mktemp -d)"
trap 'rm -rf "${test_repo}"' EXIT
git -C "${test_repo}" init --quiet
git -C "${test_repo}" config user.email ci-policy@example.invalid
git -C "${test_repo}" config user.name ci-policy
printf '# Test\n' >"${test_repo}/README.md"
git -C "${test_repo}" add README.md
git -C "${test_repo}" commit --quiet -m base
base_sha="$(git -C "${test_repo}" rev-parse HEAD)"

printf '\nEditorial.\n' >>"${test_repo}/README.md"
git -C "${test_repo}" commit --quiet -am editorial
editorial_sha="$(git -C "${test_repo}" rev-parse HEAD)"
[[ "$(cd "${test_repo}" && bash "${selector}" pull_request "${base_sha}" "${editorial_sha}" "${classifier}" true)" = editorial ]]
[[ "$(cd "${test_repo}" && bash "${selector}" pull_request "${base_sha}" "${editorial_sha}" "${classifier}" false)" = full ]]
[[ "$(cd "${test_repo}" && bash "${selector}" push '' '' "${classifier}" true)" = full ]]
[[ "$(cd "${test_repo}" && bash "${selector}" schedule '' '' "${classifier}" true)" = full ]]
[[ "$(cd "${test_repo}" && bash "${selector}" workflow_dispatch '' '' "${classifier}" true)" = full ]]
[[ "$(cd "${test_repo}" && bash "${selector}" merge_group '' '' "${classifier}" true)" = full ]]

mkdir -p "${test_repo}/internal"
printf 'package internal\n' >"${test_repo}/internal/main.go"
git -C "${test_repo}" add internal/main.go
git -C "${test_repo}" commit --quiet -m implementation
implementation_sha="$(git -C "${test_repo}" rev-parse HEAD)"
[[ "$(cd "${test_repo}" && bash "${selector}" pull_request "${base_sha}" "${implementation_sha}" "${classifier}" true)" = full ]]

mkdir -p "${test_repo}/docs"
git -C "${test_repo}" mv internal/main.go docs/example.md
git -C "${test_repo}" commit --quiet -m rename
rename_sha="$(git -C "${test_repo}" rev-parse HEAD)"
[[ "$(cd "${test_repo}" && bash "${selector}" pull_request "${implementation_sha}" "${rename_sha}" "${classifier}" true)" = full ]]
if (cd "${test_repo}" && bash "${selector}" pull_request invalid "${rename_sha}" "${classifier}" true) >/dev/null 2>&1; then
  echo "pull-request CI accepted an invalid base commit" >&2
  exit 1
fi

echo "CI policy tests passed"
