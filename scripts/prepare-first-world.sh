#!/usr/bin/env bash
set -euo pipefail

KENOGRAM_RELEASE_REPO="${KENOGRAM_RELEASE_REPO:-idolum-ai/kenogram}"

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
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else die "sha256sum or shasum is required"; fi
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

version="$(validate_version "${1:-}")"
output="${2:-world.toml}"
[[ ! -e "${output}" ]] || die "refusing to overwrite ${output}"
for command in podman id awk mktemp tr; do command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"; done

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT
base_url="https://github.com/${KENOGRAM_RELEASE_REPO}/releases/download/${version}"
download "${base_url}/checksums.txt" "${tmp_dir}/checksums.txt"
download "${base_url}/reference-world.Containerfile" "${tmp_dir}/Containerfile"
expected="$(awk '$2 == "reference-world.Containerfile" { print $1; exit }' "${tmp_dir}/checksums.txt")"
[[ -n "${expected}" ]] || die "release checksum for reference-world.Containerfile is missing"
[[ "$(checksum_file "${tmp_dir}/Containerfile")" = "${expected}" ]] || die "reference-world Containerfile checksum mismatch"

podman build --pull=missing --build-arg "USER_ID=$(id -u)" --build-arg "GROUP_ID=$(id -g)" \
  --iidfile "${tmp_dir}/image-id" --file "${tmp_dir}/Containerfile" "${tmp_dir}"
image_id="$(tr -d '[:space:]' < "${tmp_dir}/image-id")"
[[ "${image_id}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "Podman returned an invalid image ID: ${image_id}"

if ! (
  set -o noclobber
  umask 077
  {
    printf 'version = 1\nname = "first"\n\n'
    printf '[world]\nhostname = "first"\nbase = "%s"\nworkdir = "/workspace"\nuser = "%s:%s"\n\n' "${image_id}" "$(id -u)" "$(id -g)"
    printf '[resources]\ncpus = 1\nmemory_bytes = 536870912\npids = 128\n\n'
    printf '[workspace]\npaths = ["/workspace"]\n\n'
    printf '[[services]]\nname = "tmux"\ncommand = ["/bin/sh", "-c", "/usr/bin/tmux new-session -d -s main && exec /usr/bin/tmux wait-for kenogram-stop"]\nautostart = true\nrestart = "never"\n'
  } > "${output}"
); then
  die "refusing to overwrite ${output}"
fi
printf 'Wrote %s with exact local image %s\n' "${output}" "${image_id}"
