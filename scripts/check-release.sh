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
diff -u \
  <(sed -n '/^validate_version()/,/^}/p' ./scripts/install-release.sh) \
  <(sed -n '/^validate_version()/,/^}/p' ./scripts/prepare-first-world.sh)

version=v0.0.0-check
commit=releasecheck
asset="kenogram-${version}-linux-amd64.tar.gz"
RELEASE_TARGETS=linux/amd64 RELEASE_COMMIT="${commit}" RELEASE_DATE=1970-01-01T00:00:00Z SOURCE_DATE_EPOCH=0 \
  ./scripts/package-release.sh "${version}" "${tmp_dir}/first" >/dev/null
RELEASE_TARGETS=linux/amd64 RELEASE_COMMIT="${commit}" RELEASE_DATE=1970-01-01T00:00:00Z SOURCE_DATE_EPOCH=0 \
  ./scripts/package-release.sh "${version}" "${tmp_dir}/second" >/dev/null
cmp "${tmp_dir}/first/${asset}" "${tmp_dir}/second/${asset}"
cmp "${tmp_dir}/first/checksums.txt" "${tmp_dir}/second/checksums.txt"
[[ "$(wc -l < "${tmp_dir}/first/checksums.txt")" -eq 5 ]] || { echo "checksums must name the archive, installers, and both world sources" >&2; exit 1; }
grep -F "  ${asset}" "${tmp_dir}/first/checksums.txt" >/dev/null
grep -F "  install-release.sh" "${tmp_dir}/first/checksums.txt" >/dev/null
grep -F "  prepare-first-world.sh" "${tmp_dir}/first/checksums.txt" >/dev/null
grep -F "  reference-world.Containerfile" "${tmp_dir}/first/checksums.txt" >/dev/null
grep -F "  ssh-world.Containerfile" "${tmp_dir}/first/checksums.txt" >/dev/null
cmp ./scripts/install-release.sh "${tmp_dir}/first/install-release.sh"
cmp ./scripts/prepare-first-world.sh "${tmp_dir}/first/prepare-first-world.sh"
cmp ./images/reference-world/Containerfile "${tmp_dir}/first/reference-world.Containerfile"
cmp ./images/ssh-world/Containerfile "${tmp_dir}/first/ssh-world.Containerfile"
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
cp "${tmp_dir}/first/install-release.sh" "${tmp_dir}/install-release.sh"
chmod 0755 "${tmp_dir}/install-release.sh"
install_output="$(PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/first" KENOGRAM_INSTALL_DIR="${tmp_dir}/install" \
  "${tmp_dir}/install-release.sh" "${version}")"
grep -F "${tmp_dir}/install/kenogram doctor" <<< "${install_output}" >/dev/null
grep -F "export PATH=\"${tmp_dir}/install:\$PATH\"" <<< "${install_output}" >/dev/null
"${tmp_dir}/install/kenogram" version | grep -F "kenogram ${version} commit=${commit}" >/dev/null
printf '{"tag_name":"%s"}\n' "${version}" > "${tmp_dir}/first/latest"
mkdir "${tmp_dir}/latest-install"
PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/first" KENOGRAM_INSTALL_DIR="${tmp_dir}/latest-install" \
  "${tmp_dir}/install-release.sh" >/dev/null
"${tmp_dir}/latest-install/kenogram" version | grep -F "kenogram ${version} commit=${commit}" >/dev/null

cat > "${tmp_dir}/mock-bin/podman" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  build)
    iidfile=""
    while [[ $# -gt 0 ]]; do
      case "$1" in --iidfile) iidfile="$2"; shift 2 ;; *) shift ;; esac
    done
    [[ -n "${iidfile}" ]] || exit 2
    if [[ -n "${KENOGRAM_TEST_CREATE_OUTPUT:-}" ]]; then
      printf 'concurrent owner\n' > "${KENOGRAM_TEST_CREATE_OUTPUT}"
    fi
    printf 'sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n' > "${iidfile}"
    ;;
  *) exit 2 ;;
esac
MOCK
chmod 0755 "${tmp_dir}/mock-bin/podman"
mkdir "${tmp_dir}/first-world"
cp "${tmp_dir}/first/prepare-first-world.sh" "${tmp_dir}/prepare-first-world.sh"
chmod 0755 "${tmp_dir}/prepare-first-world.sh"
(
  cd "${tmp_dir}/first-world"
  PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/first" \
    "${tmp_dir}/prepare-first-world.sh" "${version}" world.toml >/dev/null
  grep -F 'base = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"' world.toml >/dev/null
  grep -F "user = \"$(id -u):$(id -g)\"" world.toml >/dev/null
  if PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/first" \
    "${tmp_dir}/prepare-first-world.sh" "${version}" world.toml >/dev/null 2>&1; then
    echo "first-world preparer overwrote an existing declaration" >&2; exit 1
  fi
)

mkdir "${tmp_dir}/raced-world"
(
  cd "${tmp_dir}/raced-world"
  if PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/first" KENOGRAM_TEST_CREATE_OUTPUT=world.toml \
    "${tmp_dir}/prepare-first-world.sh" "${version}" world.toml >/dev/null 2>&1; then
    echo "first-world preparer replaced a declaration created during the image build" >&2; exit 1
  fi
  [[ "$(cat world.toml)" = "concurrent owner" ]] || { echo "concurrent declaration was modified" >&2; exit 1; }
)

mkdir "${tmp_dir}/bad" "${tmp_dir}/bad-install"
cp "${tmp_dir}/first/${asset}" "${tmp_dir}/bad/${asset}"
printf '%064d  %s\n' 0 "${asset}" > "${tmp_dir}/bad/checksums.txt"
if PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/bad" KENOGRAM_INSTALL_DIR="${tmp_dir}/bad-install" \
  "${tmp_dir}/install-release.sh" "${version}" >/dev/null 2>&1; then echo "installer accepted checksum mismatch" >&2; exit 1; fi
if PATH="${tmp_dir}/mock-bin:${PATH}" KENOGRAM_TEST_DIST="${tmp_dir}/first" KENOGRAM_INSTALL_DIR="${tmp_dir}/bad-install" \
  "${tmp_dir}/install-release.sh" v01.2.3 >/dev/null 2>&1; then echo "standalone installer accepted invalid version" >&2; exit 1; fi
for invalid in 1.2.3 v01.2.3 v1.02.3 v1.2.3. v1.2.3-01 v1.2.3-rc. v1.2.3-a..b v1.2.3+meta; do
  if "${tmp_dir}/prepare-first-world.sh" "${invalid}" "${tmp_dir}/invalid-world.toml" >/dev/null 2>&1; then
    echo "first-world preparer accepted invalid version ${invalid}" >&2; exit 1
  fi
done

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
