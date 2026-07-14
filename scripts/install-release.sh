#!/usr/bin/env bash
set -euo pipefail

KENOGRAM_RELEASE_REPO="${KENOGRAM_RELEASE_REPO:-idolum-ai/kenogram}"
KENOGRAM_INSTALL_DIR="${KENOGRAM_INSTALL_DIR:-${HOME}/.local/bin}"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
download() {
  local url="$1" destination="$2"
  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --silent --show-error --retry 3 --proto '=https' --tlsv1.2 \
      "${url}" --output "${destination}" || die "download failed: ${url}"
  elif command -v wget >/dev/null 2>&1; then
    wget --https-only --tries=3 --quiet --output-document="${destination}" "${url}" || die "download failed: ${url}"
  else
    die "curl or wget is required"
  fi
}
checksum_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}';
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}';
  else die "sha256sum or shasum is required"; fi
}
normalize_arch() {
  case "${1:-}" in x86_64|amd64) printf 'amd64\n' ;; arm64|aarch64) printf 'arm64\n' ;; *) die "unsupported architecture: ${1:-unknown}" ;; esac
}
latest_version() {
  download "https://api.github.com/repos/${KENOGRAM_RELEASE_REPO}/releases/latest" "$1"
  sed -nE 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$1" | head -n1
}
validate_version() {
  local version="${1:-}" value core prerelease part identifier
  local -a core_parts identifiers
  [[ "${version}" == v* && "${version}" != *+* ]] || die "release version must be Semantic Versioning with a v prefix; got ${version:-<empty>}"
  value="${version#v}"
  if [[ "${value}" == *-* ]]; then core="${value%%-*}"; prerelease="${value#*-}"; else core="${value}"; prerelease=""; fi
  [[ "${core}" != .* && "${core}" != *. && "${core}" != *..* ]] || die "invalid release version: ${version}"
  IFS=. read -r -a core_parts <<< "${core}"
  [[ "${#core_parts[@]}" -eq 3 ]] || die "invalid release version: ${version}"
  for part in "${core_parts[@]}"; do
    [[ "${part}" =~ ^(0|[1-9][0-9]*)$ ]] || die "invalid release version: ${version}"
  done
  if [[ "${value}" == *-* ]]; then
    [[ -n "${prerelease}" && "${prerelease}" != .* && "${prerelease}" != *. && "${prerelease}" != *..* ]] || die "invalid release version: ${version}"
    IFS=. read -r -a identifiers <<< "${prerelease}"
    for identifier in "${identifiers[@]}"; do
      [[ -n "${identifier}" && "${identifier}" =~ ^[0-9A-Za-z-]+$ ]] || die "invalid release version: ${version}"
      [[ ! "${identifier}" =~ ^[0-9]+$ || "${identifier}" =~ ^(0|[1-9][0-9]*)$ ]] || die "invalid release version: ${version}"
    done
  fi
  printf '%s\n' "${version}"
}

version="${1:-}"
for command in uname tar install awk sed grep mktemp; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ "$(uname -s)" = Linux ]] || die "Kenogram release binaries support Linux only"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"; rm -f "${install_tmp:-}"' EXIT
[[ -n "${version}" ]] || version="$(latest_version "${tmp_dir}/latest.json")"
version="$(validate_version "${version}")"

arch="$(normalize_arch "$(uname -m)")"
asset="kenogram-${version}-linux-${arch}.tar.gz"
base_url="https://github.com/${KENOGRAM_RELEASE_REPO}/releases/download/${version}"
download "${base_url}/${asset}" "${tmp_dir}/${asset}"
download "${base_url}/checksums.txt" "${tmp_dir}/checksums.txt"
expected="$(awk -v asset="${asset}" '$2 == asset { print $1; exit }' "${tmp_dir}/checksums.txt")"
[[ -n "${expected}" ]] || die "checksum for ${asset} is missing"
[[ "$(checksum_file "${tmp_dir}/${asset}")" = "${expected}" ]] || die "checksum mismatch for ${asset}"

entries="$(tar -tzf "${tmp_dir}/${asset}" | LC_ALL=C sort)"
[[ "${entries}" = "$(printf '%s\n' LICENSE README.md kenogram)" ]] || die "archive contains unexpected files"
tar -xzf "${tmp_dir}/${asset}" -C "${tmp_dir}" kenogram
[[ -f "${tmp_dir}/kenogram" && ! -L "${tmp_dir}/kenogram" && -x "${tmp_dir}/kenogram" ]] || die "archive binary is not a regular executable"
"${tmp_dir}/kenogram" version | grep -F "kenogram ${version} " >/dev/null || die "binary version does not match ${version}"

mkdir -p "${KENOGRAM_INSTALL_DIR}"
[[ ! -d "${KENOGRAM_INSTALL_DIR}/kenogram" ]] || die "install target is a directory: ${KENOGRAM_INSTALL_DIR}/kenogram"
install_tmp="$(mktemp "${KENOGRAM_INSTALL_DIR}/.kenogram-install.XXXXXX")"
install -m 0755 "${tmp_dir}/kenogram" "${install_tmp}"
mv -f "${install_tmp}" "${KENOGRAM_INSTALL_DIR}/kenogram"
[[ -f "${KENOGRAM_INSTALL_DIR}/kenogram" && ! -L "${KENOGRAM_INSTALL_DIR}/kenogram" && -x "${KENOGRAM_INSTALL_DIR}/kenogram" ]] || die "installed binary is invalid"
printf 'Installed %s to %s/kenogram\n' "${version}" "${KENOGRAM_INSTALL_DIR}"
printf 'Check host prerequisites with: %s/kenogram doctor\n' "${KENOGRAM_INSTALL_DIR}"
case ":${PATH}:" in
  *":${KENOGRAM_INSTALL_DIR}:"*) ;;
  *) printf 'To run kenogram by name in this shell: export PATH="%s:$PATH"\n' "${KENOGRAM_INSTALL_DIR}" ;;
esac
