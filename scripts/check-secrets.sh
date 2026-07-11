#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
pattern='[0-9]{8,}:[A-Za-z0-9_-]{30,}|sk-ant-[A-Za-z0-9_-]{20,}'
if find . -type f -not -path './.git/*' -not -path './bin/*' -print0 | xargs -0 -r rg -n "$pattern" >/dev/null; then
  echo "possible secret in repository" >&2
  exit 1
fi
echo "secret check passed"
