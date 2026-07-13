#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
fail() {
  printf 'release version must be Semantic Versioning with a v prefix; got %q\n' "${version}" >&2
  exit 2
}

[[ "${version}" == v* && "${version}" != *+* ]] || fail
value="${version#v}"
if [[ "${value}" == *-* ]]; then
  core="${value%%-*}"
  prerelease="${value#*-}"
else
  core="${value}"
  prerelease=""
fi

[[ "${core}" != .* && "${core}" != *. && "${core}" != *..* ]] || fail
IFS=. read -r -a core_parts <<< "${core}"
[[ "${#core_parts[@]}" -eq 3 ]] || fail
for part in "${core_parts[@]}"; do
  [[ "${part}" =~ ^(0|[1-9][0-9]*)$ ]] || fail
done

if [[ "${value}" == *-* ]]; then
  [[ -n "${prerelease}" && "${prerelease}" != .* && "${prerelease}" != *. && "${prerelease}" != *..* ]] || fail
  IFS=. read -r -a identifiers <<< "${prerelease}"
  for identifier in "${identifiers[@]}"; do
    [[ -n "${identifier}" && "${identifier}" =~ ^[0-9A-Za-z-]+$ ]] || fail
    if [[ "${identifier}" =~ ^[0-9]+$ && ! "${identifier}" =~ ^(0|[1-9][0-9]*)$ ]]; then
      fail
    fi
  done
fi

printf '%s\n' "${version}"
