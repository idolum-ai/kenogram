#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
notes_path="${2:-}"
[[ -f "${notes_path}" ]] || { echo "release notes file is missing" >&2; exit 2; }

for heading in '## Summary' '## Compatibility' '## Validation'; do
  grep -F -x "${heading}" "${notes_path}" >/dev/null || {
    echo "release notes for ${version} are missing required heading: ${heading}" >&2
    exit 2
  }
done

meaningful="$(sed -E '/^[[:space:]]*($|#|<!--|-->|- \[[ xX]\])/d' "${notes_path}" | tr -d '[:space:]')"
[[ "${#meaningful}" -ge 80 ]] || {
  echo "release notes for ${version} do not contain enough reviewed content" >&2
  exit 2
}
