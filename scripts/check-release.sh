#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

for version in v0.0.0 v1.2.3-rc.1 v1.2.3-alpha-7; do ./scripts/validate-release-version.sh "${version}" >/dev/null; done
for version in 1.2.3 v01.2.3 v1.02.3 v1.2.3. v1.2.3-01 v1.2.3-rc. v1.2.3-a..b v1.2.3+meta; do
  if ./scripts/validate-release-version.sh "${version}" >/dev/null 2>&1; then echo "validator accepted ${version}" >&2; exit 1; fi
done

version=v0.0.0-check
commit=releasecheck
asset="kenogram-${version}-linux-amd64.tar.gz"
RELEASE_TARGETS=linux/amd64 RELEASE_COMMIT="${commit}" RELEASE_DATE=1970-01-01T00:00:00Z SOURCE_DATE_EPOCH=0 \
  ./scripts/package-release.sh "${version}" "${tmp_dir}/first" >/dev/null
RELEASE_TARGETS=linux/amd64 RELEASE_COMMIT="${commit}" RELEASE_DATE=1970-01-01T00:00:00Z SOURCE_DATE_EPOCH=0 \
  ./scripts/package-release.sh "${version}" "${tmp_dir}/second" >/dev/null
cmp "${tmp_dir}/first/${asset}" "${tmp_dir}/second/${asset}"
cmp "${tmp_dir}/first/checksums.txt" "${tmp_dir}/second/checksums.txt"
[[ "$(wc -l < "${tmp_dir}/first/checksums.txt")" -eq 1 ]] || { echo "checksums must name exactly one test asset" >&2; exit 1; }
grep -F "  ${asset}" "${tmp_dir}/first/checksums.txt" >/dev/null
[[ "$(tar -tzf "${tmp_dir}/first/${asset}" | LC_ALL=C sort)" = "$(printf '%s\n' LICENSE README.md kenogram)" ]] || {
  echo "archive contents are incorrect" >&2; exit 1;
}
tar -xzf "${tmp_dir}/first/${asset}" -C "${tmp_dir}" kenogram
"${tmp_dir}/kenogram" version | grep -F "kenogram ${version} commit=${commit}" >/dev/null

mkdir -p "${tmp_dir}/mock-bin" "${tmp_dir}/install"
cat > "${tmp_dir}/mock-bin/curl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
url=""
destination=""
while [[ $# -gt 0 ]]; do
  case "$1" in --output) destination="$2"; shift 2 ;; https://*) url="$1"; shift ;; *) shift ;; esac
done
cp "${KENOGRAM_TEST_DIST}/${url##*/}" "${destination}"
MOCK
chmod 0755 "${tmp_dir}/mock-bin/curl"
PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/first" KENOGRAM_INSTALL_DIR="${tmp_dir}/install" \
  ./scripts/install-release.sh "${version}" >/dev/null
"${tmp_dir}/install/kenogram" version | grep -F "kenogram ${version} commit=${commit}" >/dev/null
mkdir "${tmp_dir}/bad" "${tmp_dir}/bad-install"
cp "${tmp_dir}/first/${asset}" "${tmp_dir}/bad/${asset}"
printf '%064d  %s\n' 0 "${asset}" > "${tmp_dir}/bad/checksums.txt"
if PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/bad" KENOGRAM_INSTALL_DIR="${tmp_dir}/bad-install" \
  ./scripts/install-release.sh "${version}" >/dev/null 2>&1; then echo "installer accepted checksum mismatch" >&2; exit 1; fi

cat > "${tmp_dir}/notes.md" <<'NOTES'
## Summary

This candidate preserves Kenogram's reviewed source and observable artifact identity.

## Compatibility

No declaration migration is required and Linux host requirements are unchanged.

## Validation

The full gate, integration proof, archive identity, and checksums passed.
NOTES
./scripts/validate-release-notes.sh "${version}" "${tmp_dir}/notes.md"
./scripts/prepare-release-notes.sh "${version}" "Release ${version}" https://example.test/pr/1 \
  "${tmp_dir}/notes.md" "${tmp_dir}/prepared.md"
grep -F "# ${version}" "${tmp_dir}/prepared.md" >/dev/null

mkdir "${tmp_dir}/history"
(
  cd "${tmp_dir}/history"
  git init -q
  git config user.name Kenogram
  git config user.email kenogram@example.test
  printf 'first\n' > history.txt
  git add history.txt
  git commit -q -m 'First candidate change'
  printf 'second\n' >> history.txt
  git commit -q -am 'Second candidate change'
  "${repo_root}/scripts/generate-release-notes.sh" --output notes.md --title "${version}"
  grep -F 'Second candidate change' notes.md >/dev/null
  grep -F '## Compatibility' notes.md >/dev/null
)

echo "release tooling check passed"
