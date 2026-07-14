#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
output_dir="${2:-dist}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
version="$("${script_dir}/validate-release-version.sh" "${version}")"

for command in go git; do
  command -v "${command}" >/dev/null 2>&1 || { echo "required command not found: ${command}" >&2; exit 1; }
done

tar_bin="${TAR:-}"
if [[ -z "${tar_bin}" ]]; then
  if command -v gtar >/dev/null 2>&1; then tar_bin=gtar; else tar_bin=tar; fi
fi
command -v "${tar_bin}" >/dev/null 2>&1 || { echo "required command not found: ${tar_bin}" >&2; exit 1; }
"${tar_bin}" --version 2>/dev/null | grep -q 'GNU tar' || {
  echo "GNU tar is required for reproducible release archives" >&2
  exit 1
}

repo_root="$(git rev-parse --show-toplevel)"
cd "${repo_root}"
release_commit="${RELEASE_COMMIT:-$(git rev-parse --short=12 HEAD)}"
release_date="${RELEASE_DATE:-$(git show -s --format=%cI HEAD)}"
source_epoch="${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}"
output_dir="$(mkdir -p "${output_dir}" && cd "${output_dir}" && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

ldflags="-s -w -X github.com/idolum-ai/kenogram/internal/version.Version=${version} -X github.com/idolum-ai/kenogram/internal/version.Commit=${release_commit} -X github.com/idolum-ai/kenogram/internal/version.Date=${release_date}"
assets=()
read -r -a targets <<< "${RELEASE_TARGETS:-linux/amd64 linux/arm64}"
for target in "${targets[@]}"; do
  case "${target}" in linux/amd64|linux/arm64) ;; *) echo "unsupported release target: ${target}" >&2; exit 2 ;; esac
  os="${target%/*}"
  arch="${target#*/}"
  asset="kenogram-${version}-${os}-${arch}.tar.gz"
  package_dir="${work_dir}/${os}-${arch}"
  mkdir -p "${package_dir}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build \
    -trimpath -buildvcs=false -ldflags "${ldflags}" \
    -o "${package_dir}/kenogram" ./cmd/kenogram
  chmod 0755 "${package_dir}/kenogram"
  cp README.md LICENSE "${package_dir}/"
  rm -f "${output_dir}/${asset}"
  "${tar_bin}" --sort=name --mtime="@${source_epoch}" --owner=0 --group=0 --numeric-owner \
    -czf "${output_dir}/${asset}" -C "${package_dir}" kenogram README.md LICENSE
  assets+=("${asset}")
done

installer="install-release.sh"
cp "scripts/${installer}" "${output_dir}/${installer}"
chmod 0755 "${output_dir}/${installer}"
assets+=("${installer}")

first_world="prepare-first-world.sh"
cp "scripts/${first_world}" "${output_dir}/${first_world}"
chmod 0755 "${output_dir}/${first_world}"
cp "images/reference-world/Containerfile" "${output_dir}/reference-world.Containerfile"
cp "images/ssh-world/Containerfile" "${output_dir}/ssh-world.Containerfile"
assets+=("${first_world}" "reference-world.Containerfile" "ssh-world.Containerfile")

(
  cd "${output_dir}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${assets[@]}" > checksums.txt
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${assets[@]}" > checksums.txt
  else
    echo "sha256sum or shasum is required" >&2
    exit 1
  fi
)
printf 'packaged %s at %s\n' "${version}" "${output_dir}"
