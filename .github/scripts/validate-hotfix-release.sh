#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "[hotfix-release-policy] $*" >&2
  exit 1
}

TAG=""
EXPECTED_COMMIT=""
BASE_TAG=""
EXPECTED_BASE_COMMIT=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --tag)
      [ "$#" -ge 2 ] || die "--tag requires a value"
      TAG=$2
      shift 2
      ;;
    --expected-commit)
      [ "$#" -ge 2 ] || die "--expected-commit requires a value"
      EXPECTED_COMMIT=$2
      shift 2
      ;;
    --base-tag)
      [ "$#" -ge 2 ] || die "--base-tag requires a value"
      BASE_TAG=$2
      shift 2
      ;;
    --expected-base-commit)
      [ "$#" -ge 2 ] || die "--expected-base-commit requires a value"
      EXPECTED_BASE_COMMIT=$2
      shift 2
      ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-commit must be a 40-character lowercase commit"
[[ "${EXPECTED_BASE_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-base-commit must be a 40-character lowercase commit"
[[ "${TAG}" =~ ^v[0-9][0-9A-Za-z.+-]*unstableneutron\.[0-9]+$ ]] \
  || die "--tag must be a fork release tag"
[[ "${BASE_TAG}" =~ ^v[0-9][0-9A-Za-z.+-]*unstableneutron\.[0-9]+$ ]] \
  || die "--base-tag must be a fork release tag"

git rev-parse --git-dir >/dev/null 2>&1 || die "run inside the repository"
git rev-parse --verify 'refs/remotes/origin/main^{commit}' >/dev/null 2>&1 \
  || die "origin/main is not fetched"
MAIN_COMMIT=$(git rev-parse 'refs/remotes/origin/main^{commit}')
HEAD_COMMIT=$(git rev-parse 'HEAD^{commit}')
if [ "${MAIN_COMMIT}" != "${EXPECTED_COMMIT}" ]; then
  die "origin/main resolves to ${MAIN_COMMIT}, expected ${EXPECTED_COMMIT}"
fi
if [ "${HEAD_COMMIT}" != "${EXPECTED_COMMIT}" ]; then
  die "HEAD resolves to ${HEAD_COMMIT}, expected ${EXPECTED_COMMIT}"
fi

git rev-parse --verify "refs/tags/${BASE_TAG}^{commit}" >/dev/null 2>&1 \
  || die "base tag ${BASE_TAG} is not fetched"
if [ "$(git cat-file -t "refs/tags/${BASE_TAG}")" != tag ]; then
  die "base tag ${BASE_TAG} must be annotated"
fi
BASE_COMMIT=$(git rev-parse "refs/tags/${BASE_TAG}^{}")
if [ "${BASE_COMMIT}" != "${EXPECTED_BASE_COMMIT}" ]; then
  die "base tag ${BASE_TAG} resolves to ${BASE_COMMIT}, expected ${EXPECTED_BASE_COMMIT}"
fi
if ! git merge-base --is-ancestor "${BASE_COMMIT}" "${EXPECTED_COMMIT}"; then
  die "expected commit ${EXPECTED_COMMIT} does not descend from ${BASE_TAG}"
fi
if [ "${BASE_COMMIT}" = "${EXPECTED_COMMIT}" ]; then
  die "a hotfix release must contain at least one commit after ${BASE_TAG}"
fi

if [[ "${BASE_TAG}" =~ ^(.+unstableneutron\.)([0-9]+)$ ]]; then
  EXPECTED_TAG="${BASH_REMATCH[1]}$((10#${BASH_REMATCH[2]} + 1))"
else
  die "could not derive the next hotfix tag from ${BASE_TAG}"
fi
if [ "${TAG}" != "${EXPECTED_TAG}" ]; then
  die "hotfix tag must be the next suffix ${EXPECTED_TAG}, got ${TAG}"
fi
if git rev-parse --verify "refs/tags/${TAG}^{commit}" >/dev/null 2>&1; then
  die "hotfix tag ${TAG} already exists"
fi

LATEST_TAG="$({
  git tag --merged refs/remotes/origin/main --list 'v*-unstableneutron.*'
  git tag --merged refs/remotes/origin/main --list 'v*.unstableneutron.*'
} | sort -Vu | tail -n 1)"
if [ "${LATEST_TAG}" != "${BASE_TAG}" ]; then
  die "base tag ${BASE_TAG} is not the latest accepted tag ${LATEST_TAG}"
fi

BASE_STATE=$(mktemp)
EXPECTED_STATE=$(mktemp)
trap 'rm -f "${BASE_STATE}" "${EXPECTED_STATE}"' EXIT
git show "${BASE_COMMIT}:.ccs-fork-upstream.env" > "${BASE_STATE}" \
  || die "base release does not contain upstream-sync state"
git show "${EXPECTED_COMMIT}:.ccs-fork-upstream.env" > "${EXPECTED_STATE}" \
  || die "expected commit does not contain upstream-sync state"
if ! cmp -s "${BASE_STATE}" "${EXPECTED_STATE}"; then
  die "hotfix commits changed .ccs-fork-upstream.env"
fi

state_value() {
  local key=$1
  awk -F= -v key="${key}" \
    '$1 == key { sub(/^[^=]*=/, ""); print; exit }' \
    "${EXPECTED_STATE}"
}

SYNC_ID=$(state_value SYNC_ID)
PLAN_FINGERPRINT=$(state_value PLAN_FINGERPRINT)
RECORDED_TAG=$(state_value EXPECTED_FORK_TAG)
if [ -z "${SYNC_ID}" ]; then
  die "upstream-sync state is missing SYNC_ID"
fi
[[ "${PLAN_FINGERPRINT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "upstream-sync state has an invalid PLAN_FINGERPRINT"
if [ "${RECORDED_TAG}" != "${BASE_TAG}" ]; then
  die "upstream-sync state records ${RECORDED_TAG}, expected base tag ${BASE_TAG}"
fi
STATE_SHA256=$(sha256sum "${EXPECTED_STATE}" | awk '{ print $1 }')

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "tag=${TAG}"
    echo "expected_commit=${EXPECTED_COMMIT}"
    echo "base_tag=${BASE_TAG}"
    echo "base_commit=${BASE_COMMIT}"
    echo "sync_id=${SYNC_ID}"
    echo "plan_fingerprint=${PLAN_FINGERPRINT}"
    echo "upstream_state_sha256=${STATE_SHA256}"
  } >> "${GITHUB_OUTPUT}"
fi

echo "[OK] hotfix release ${TAG} is pinned to ${EXPECTED_COMMIT} after ${BASE_TAG}"
