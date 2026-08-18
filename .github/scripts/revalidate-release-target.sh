#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "[release-target] $*" >&2
  exit 1
}

[ "$#" -eq 2 ] || die "usage: $0 <tag> <expected-main-commit>"
TAG=$1
EXPECTED_COMMIT=$2
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

[ "${GITHUB_REPOSITORY}" = unstableneutron/CLIProxyAPIPlus ] \
  || die "repository identity differs"
[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "expected commit must be an exact lowercase SHA"
[[ "${TAG}" =~ ^v[0-9][0-9A-Za-z._+-]*[-.]unstableneutron\.(0|[1-9][0-9]*)$ ]] \
  || die "release tag is invalid"

MAIN_COMMIT=$(gh api "/repos/${GITHUB_REPOSITORY}/commits/main" --jq .sha)
TAG_COMMIT=$(gh api "/repos/${GITHUB_REPOSITORY}/commits/${TAG}" --jq .sha)
if [ "${MAIN_COMMIT}" != "${EXPECTED_COMMIT}" ] || \
   [ "${TAG_COMMIT}" != "${EXPECTED_COMMIT}" ]; then
  die "main or tag moved from ${EXPECTED_COMMIT}"
fi
