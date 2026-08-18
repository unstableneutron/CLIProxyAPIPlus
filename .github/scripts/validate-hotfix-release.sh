#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "${SCRIPT_DIR}/hotfix-release-tag.sh"

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
parse_fork_release_tag "${TAG}" || die "--tag must be a fork release tag"
TAG_PREFIX=${FORK_TAG_PREFIX}
parse_fork_release_tag "${BASE_TAG}" || die "--base-tag must be a fork release tag"
BASE_PREFIX=${FORK_TAG_PREFIX}
BASE_SUFFIX=${FORK_TAG_SUFFIX}

git rev-parse --git-dir >/dev/null 2>&1 || die "run inside the repository"
if [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
  die "hotfix release requires a clean checkout"
fi
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

if [ "${TAG_PREFIX}" != "${BASE_PREFIX}" ]; then
  die "hotfix tag and base tag are on different release lines"
fi
EXPECTED_TAG="${BASE_PREFIX}.$((BASE_SUFFIX + 1))"
if [ "${TAG}" != "${EXPECTED_TAG}" ]; then
  die "hotfix tag must be the next suffix ${EXPECTED_TAG}, got ${TAG}"
fi
REMOTE_TAG=$(mktemp)
trap 'rm -f "${REMOTE_TAG}"' EXIT
set +e
git ls-remote --exit-code --tags origin "refs/tags/${TAG}" > "${REMOTE_TAG}"
REMOTE_TAG_STATUS=$?
set -e
case "${REMOTE_TAG_STATUS}" in
  0) die "hotfix tag ${TAG} already exists" ;;
  2)
    if git rev-parse --verify "refs/tags/${TAG}" >/dev/null 2>&1; then
      die "local hotfix tag ${TAG} exists without the remote identity"
    fi
    ;;
  *) die "could not determine whether remote hotfix tag ${TAG} exists" ;;
esac

LATEST_TAG="$({
  git tag --merged refs/remotes/origin/main --list 'v*-unstableneutron.*'
  git tag --merged refs/remotes/origin/main --list 'v*.unstableneutron.*'
} | sort -Vu | tail -n 1)"
if [ "${LATEST_TAG}" != "${BASE_TAG}" ]; then
  die "expected latest accepted tag ${BASE_TAG}, got ${LATEST_TAG}"
fi

BASE_STATE=$(mktemp)
EXPECTED_STATE=$(mktemp)
trap 'rm -f "${REMOTE_TAG}" "${BASE_STATE}" "${EXPECTED_STATE}"' EXIT
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
ORIGINAL_TAG=$(state_value ORIGINAL_TAG)
if [ -z "${SYNC_ID}" ]; then
  die "upstream-sync state is missing SYNC_ID"
fi
[[ "${PLAN_FINGERPRINT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "upstream-sync state has an invalid PLAN_FINGERPRINT"
parse_fork_release_tag "${RECORDED_TAG}" \
  || die "upstream-sync state records an invalid accepted root tag"
ROOT_PREFIX=${FORK_TAG_PREFIX}
ROOT_SUFFIX=${FORK_TAG_SUFFIX}
fork_tag_prefix_for_source_tag "${ORIGINAL_TAG}" \
  || die "upstream-sync state has an invalid ORIGINAL_TAG"
if [ "${ROOT_PREFIX}" != "${FORK_TAG_PREFIX}" ] || \
   [ "${ROOT_PREFIX}" != "${BASE_PREFIX}" ]; then
  die "upstream-sync state root ${RECORDED_TAG} does not match release line ${BASE_PREFIX}"
fi
if [ "${ROOT_SUFFIX}" -gt "${BASE_SUFFIX}" ]; then
  die "upstream-sync state root ${RECORDED_TAG} is newer than base ${BASE_TAG}"
fi
git rev-parse --verify "refs/tags/${RECORDED_TAG}^{commit}" >/dev/null 2>&1 \
  || die "accepted upstream root tag ${RECORDED_TAG} is not fetched"
ROOT_COMMIT=$(git rev-parse "refs/tags/${RECORDED_TAG}^{}")
STATE_SHA256=$(sha256sum "${EXPECTED_STATE}" | awk '{ print $1 }')

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "tag=${TAG}"
    echo "expected_commit=${EXPECTED_COMMIT}"
    echo "base_tag=${BASE_TAG}"
    echo "base_commit=${BASE_COMMIT}"
    echo "root_tag=${RECORDED_TAG}"
    echo "root_commit=${ROOT_COMMIT}"
    echo "sync_id=${SYNC_ID}"
    echo "plan_fingerprint=${PLAN_FINGERPRINT}"
    echo "upstream_state_sha256=${STATE_SHA256}"
  } >> "${GITHUB_OUTPUT}"
fi

echo "[OK] hotfix release ${TAG} is pinned to ${EXPECTED_COMMIT} after ${BASE_TAG}"
