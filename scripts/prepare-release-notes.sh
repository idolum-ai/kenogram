#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
title="${2:-}"
url="${3:-}"
body_path="${4:-}"
output_path="${5:-}"
[[ -f "${body_path}" && -n "${output_path}" ]] || {
  echo "usage: scripts/prepare-release-notes.sh VERSION PR_TITLE PR_URL BODY_PATH OUTPUT_PATH" >&2
  exit 2
}

./scripts/validate-release-notes.sh "${version}" "${body_path}"
{
  printf '# %s\n\n' "${version}"
  [[ -z "${url}" ]] || printf 'Release PR: [%s](%s)\n\n' "${title}" "${url}"
  cat "${body_path}"
  printf '\n'
} > "${output_path}"
