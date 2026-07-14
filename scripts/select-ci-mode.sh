#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 5 ]]; then
  echo "usage: select-ci-mode.sh EVENT BASE_SHA HEAD_SHA CLASSIFIER PATH_AWARE_ENABLED" >&2
  exit 2
fi
event_name="$1"
base_sha="$2"
head_sha="$3"
classifier="$4"
path_aware_enabled="$5"

# Protected, non-PR entry points and incomplete rollouts always replay the
# complete evidence set. An administrator enables selection only after the
# organization-ruleset workflow is active.
if [[ "${event_name}" != pull_request || "${path_aware_enabled}" != true ]]; then
  printf 'full\n'
  exit 0
fi

[[ "${base_sha}" =~ ^[0-9a-f]{40}$ && "${head_sha}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "pull-request CI requires exact base and head commit IDs" >&2
  exit 1
}
[[ -f "${classifier}" ]] || {
  echo "trusted CI classifier is unavailable: ${classifier}" >&2
  exit 1
}
git cat-file -e "${base_sha}^{commit}"
git cat-file -e "${head_sha}^{commit}"
git diff --no-renames --name-only -z "${base_sha}...${head_sha}" |
  bash "${classifier}"
