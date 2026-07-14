#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 6 ]]; then
  echo "usage: verify-ci-results.sh MODE CHECK RACE APPLE_HOST RUNTIME RUNTIME_HERMES" >&2
  exit 2
fi
mode="$1"
check_result="$2"
race_result="$3"
apple_host_result="$4"
runtime_result="$5"
runtime_hermes_result="$6"

expect() {
  local job="$1"
  local actual="$2"
  local wanted="$3"
  if [[ "${actual}" != "${wanted}" ]]; then
    echo "${job} result is ${actual}, expected ${wanted} in ${mode} mode" >&2
    exit 1
  fi
}

expect check "${check_result}" success
case "${mode}" in
  editorial)
    expect race "${race_result}" skipped
    expect apple-host "${apple_host_result}" skipped
    expect runtime "${runtime_result}" skipped
    expect runtime-hermes "${runtime_hermes_result}" skipped
    ;;
  full)
    expect race "${race_result}" success
    expect apple-host "${apple_host_result}" success
    expect runtime "${runtime_result}" success
    expect runtime-hermes "${runtime_hermes_result}" success
    ;;
  *)
    echo "invalid CI mode: ${mode}" >&2
    exit 1
    ;;
esac
