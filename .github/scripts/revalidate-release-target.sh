#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "[release-target] $*" >&2
  exit 1
}

[ "$#" -ge 2 ] && [ "$#" -le 3 ] || die "usage: $0 <tag> <expected-release-commit> [exact|descendant]"
TAG=$1
EXPECTED_COMMIT=$2
MAIN_POLICY=${3:-exact}
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

[ "${GITHUB_REPOSITORY}" = unstableneutron/CLIProxyAPIPlus ] \
  || die "repository identity differs"
[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "expected commit must be an exact lowercase SHA"
[[ "${TAG}" =~ ^v[0-9][0-9A-Za-z._+-]*[-.]unstableneutron\.(0|[1-9][0-9]*)$ ]] \
  || die "release tag is invalid"
case "${MAIN_POLICY}" in
  exact|descendant) ;;
  *) die "main policy must be exact or descendant" ;;
esac

MAIN_COMMIT=$(gh api "/repos/${GITHUB_REPOSITORY}/commits/main" --jq .sha)
TAG_COMMIT=$(gh api "/repos/${GITHUB_REPOSITORY}/commits/${TAG}" --jq .sha)
if [ "${TAG_COMMIT}" != "${EXPECTED_COMMIT}" ]; then
  die "tag moved from ${EXPECTED_COMMIT}"
fi
if [ "${MAIN_POLICY}" = exact ]; then
  [ "${MAIN_COMMIT}" = "${EXPECTED_COMMIT}" ] \
    || die "main moved from ${EXPECTED_COMMIT}"
else
  MAIN_STATUS=$(gh api \
    "/repos/${GITHUB_REPOSITORY}/compare/${EXPECTED_COMMIT}...${MAIN_COMMIT}" \
    --jq .status)
  case "${MAIN_STATUS}" in
    identical|ahead) ;;
    *) die "main is not descended from ${EXPECTED_COMMIT}" ;;
  esac
fi
