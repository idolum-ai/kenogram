#!/usr/bin/env bash
set -euo pipefail

# Read NUL-delimited repository-relative paths and select the least expensive
# CI mode that can still prove the change. Unknown and empty inputs fail closed.
mode=editorial
seen=false
while IFS= read -r -d '' path; do
  seen=true
  case "${path}" in
    README.md | CONTRIBUTING.md | CHANGELOG.md | LICENSE | \
      docs/*.md | requirements/*.md | \
      images/*/README.md | .github/*.md | \
      .github/ISSUE_TEMPLATE/*.yml | .github/ISSUE_TEMPLATE/*.yaml)
      ;;
    *)
      mode=full
      ;;
  esac
done

if [[ "${seen}" = false ]]; then
  mode=full
fi
printf '%s\n' "${mode}"
