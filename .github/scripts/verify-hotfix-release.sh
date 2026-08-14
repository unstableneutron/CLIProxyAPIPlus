#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
UPSTREAM_VERIFIER="${SCRIPT_DIR}/verify-upstream-release.sh"

die() {
  echo "[hotfix-release-verifier] $*" >&2
  exit 1
}

TAG=""
EXPECTED_COMMIT=""
BASE_TAG=""
EXPECTED_BASE_COMMIT=""
EXPECTED_SYNC_ID=""
EXPECTED_PLAN_FINGERPRINT=""
IMAGE=""
MAIN_POLICY=exact
REQUIRE_LATEST_PARITY=""
RECEIPT=""
ATTACHED_RECEIPT=""

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
    --expected-sync-id)
      [ "$#" -ge 2 ] || die "--expected-sync-id requires a value"
      EXPECTED_SYNC_ID=$2
      shift 2
      ;;
    --expected-plan-fingerprint)
      [ "$#" -ge 2 ] || die "--expected-plan-fingerprint requires a value"
      EXPECTED_PLAN_FINGERPRINT=$2
      shift 2
      ;;
    --image)
      [ "$#" -ge 2 ] || die "--image requires a value"
      IMAGE=$2
      shift 2
      ;;
    --main-policy)
      [ "$#" -ge 2 ] || die "--main-policy requires a value"
      MAIN_POLICY=$2
      shift 2
      ;;
    --require-latest-parity)
      [ "$#" -ge 2 ] || die "--require-latest-parity requires a value"
      REQUIRE_LATEST_PARITY=$2
      shift 2
      ;;
    --receipt)
      [ "$#" -ge 2 ] || die "--receipt requires a value"
      RECEIPT=$2
      shift 2
      ;;
    --attached-receipt)
      [ "$#" -ge 2 ] || die "--attached-receipt requires a value"
      ATTACHED_RECEIPT=$2
      shift 2
      ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-commit must be a 40-character lowercase commit"
[[ "${EXPECTED_BASE_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-base-commit must be a 40-character lowercase commit"
[ -n "${TAG}" ] || die "--tag is required"
[ -n "${BASE_TAG}" ] || die "--base-tag is required"
[ -n "${EXPECTED_SYNC_ID}" ] || die "--expected-sync-id is required"
[[ "${EXPECTED_PLAN_FINGERPRINT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-plan-fingerprint must be a 40-character lowercase hash"
[ -n "${IMAGE}" ] || die "--image is required"
case "${MAIN_POLICY}" in
  exact|descendant) ;;
  *) die "--main-policy must be exact or descendant" ;;
esac
case "${REQUIRE_LATEST_PARITY}" in
  true|false) ;;
  *) die "--require-latest-parity must be true or false" ;;
esac
[ -n "${RECEIPT}" ] || die "--receipt is required"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

HEAD_COMMIT=$(git rev-parse 'HEAD^{commit}')
if [ "${MAIN_POLICY}" = exact ]; then
  if [ "${HEAD_COMMIT}" != "${EXPECTED_COMMIT}" ]; then
    die "HEAD resolves to ${HEAD_COMMIT}, expected ${EXPECTED_COMMIT}"
  fi
elif ! git merge-base --is-ancestor "${EXPECTED_COMMIT}" "${HEAD_COMMIT}"; then
  die "HEAD ${HEAD_COMMIT} does not descend from ${EXPECTED_COMMIT}"
fi
LOCAL_BASE_COMMIT=$(git rev-parse "refs/tags/${BASE_TAG}^{}")
if [ "${LOCAL_BASE_COMMIT}" != "${EXPECTED_BASE_COMMIT}" ]; then
  die "base tag ${BASE_TAG} resolves locally to ${LOCAL_BASE_COMMIT}, expected ${EXPECTED_BASE_COMMIT}"
fi
REMOTE_BASE_COMMIT=$(gh api \
  "repos/${GITHUB_REPOSITORY}/commits/${BASE_TAG}" \
  --jq .sha)
if [ "${REMOTE_BASE_COMMIT}" != "${EXPECTED_BASE_COMMIT}" ]; then
  die "base tag ${BASE_TAG} resolves remotely to ${REMOTE_BASE_COMMIT}, expected ${EXPECTED_BASE_COMMIT}"
fi
BASE_COMPARE_STATUS=$(gh api \
  "repos/${GITHUB_REPOSITORY}/compare/${EXPECTED_BASE_COMMIT}...${EXPECTED_COMMIT}" \
  --jq .status)
if [ "${BASE_COMPARE_STATUS}" != ahead ]; then
  die "hotfix commit ${EXPECTED_COMMIT} is not strictly ahead of ${EXPECTED_BASE_COMMIT}"
fi

BASE_STATE=$(mktemp)
EXPECTED_STATE=$(mktemp)
CORE_RECEIPT=$(mktemp)
ASSET_DIR=$(mktemp -d)
trap 'rm -f "${BASE_STATE}" "${EXPECTED_STATE}" "${CORE_RECEIPT}"; rm -rf "${ASSET_DIR}"' EXIT
git show "${EXPECTED_BASE_COMMIT}:.ccs-fork-upstream.env" > "${BASE_STATE}" \
  || die "base release does not contain upstream-sync state"
git show "${EXPECTED_COMMIT}:.ccs-fork-upstream.env" > "${EXPECTED_STATE}" \
  || die "hotfix release does not contain upstream-sync state"
if ! cmp -s "${BASE_STATE}" "${EXPECTED_STATE}"; then
  die "hotfix release changed .ccs-fork-upstream.env"
fi
STATE_SYNC_ID=$(awk -F= '$1 == "SYNC_ID" { print $2; exit }' "${EXPECTED_STATE}")
STATE_FINGERPRINT=$(awk -F= '$1 == "PLAN_FINGERPRINT" { print $2; exit }' "${EXPECTED_STATE}")
STATE_RECORDED_TAG=$(awk -F= '$1 == "EXPECTED_FORK_TAG" { print $2; exit }' "${EXPECTED_STATE}")
if [ "${STATE_SYNC_ID}" != "${EXPECTED_SYNC_ID}" ] || \
   [ "${STATE_FINGERPRINT}" != "${EXPECTED_PLAN_FINGERPRINT}" ] || \
   [ "${STATE_RECORDED_TAG}" != "${BASE_TAG}" ]; then
  die "hotfix upstream-sync state does not match the accepted base release"
fi
STATE_SHA256=$(sha256sum "${EXPECTED_STATE}" | awk '{ print $1 }')

"${UPSTREAM_VERIFIER}" \
  --tag "${TAG}" \
  --expected-commit "${EXPECTED_COMMIT}" \
  --expected-sync-id "${EXPECTED_SYNC_ID}" \
  --expected-plan-fingerprint "${EXPECTED_PLAN_FINGERPRINT}" \
  --image "${IMAGE}" \
  --main-policy "${MAIN_POLICY}" \
  --require-architecture-tags true \
  --require-latest-parity "${REQUIRE_LATEST_PARITY}" \
  --receipt "${CORE_RECEIPT}"

RELEASE_API=$(gh api "repos/${GITHUB_REPOSITORY}/releases/tags/${TAG}")
RELEASE_ASSET_DIGESTS=$(jq -ce '
    [.assets[] |
      select(.name != "upstream-sync-receipt.json" and .name != "hotfix-release-receipt.json") |
      if (.name | type) != "string" or (.name | length) == 0 or
         (.digest | type) != "string" or
         (.digest | test("^sha256:[0-9a-f]{64}$") | not)
      then error("release asset has a missing or invalid digest")
      else {key: .name, value: .digest}
      end
    ] |
    if length == ([.[].key] | unique | length)
    then from_entries
    else error("release contains duplicate asset names")
    end
  ' <<< "${RELEASE_API}") \
  || die "release assets do not have a unique SHA-256 identity"
if [ "$(jq 'length' <<< "${RELEASE_ASSET_DIGESTS}")" -lt 2 ]; then
  die "release must contain checksums.txt and at least one archive"
fi
if ! jq -e 'has("checksums.txt")' <<< "${RELEASE_ASSET_DIGESTS}" >/dev/null; then
  die "release asset digest set is missing checksums.txt"
fi

gh release download "${TAG}" \
  --repo "${GITHUB_REPOSITORY}" \
  --pattern checksums.txt \
  --dir "${ASSET_DIR}"
CHECKSUM_FILE="${ASSET_DIR}/checksums.txt"
[ -f "${CHECKSUM_FILE}" ] || die "could not download checksums.txt"
CHECKSUM_DIGEST="sha256:$(sha256sum "${CHECKSUM_FILE}" | awk '{ print $1 }')"
EXPECTED_CHECKSUM_DIGEST=$(jq -r '."checksums.txt"' <<< "${RELEASE_ASSET_DIGESTS}")
if [ "${CHECKSUM_DIGEST}" != "${EXPECTED_CHECKSUM_DIGEST}" ]; then
  die "checksums.txt content does not match its GitHub asset digest"
fi

CHECKSUM_ASSETS='{}'
while IFS= read -r LINE || [ -n "${LINE}" ]; do
  [ -n "${LINE}" ] || continue
  if [[ ! "${LINE}" =~ ^([0-9a-f]{64})[[:space:]]+\*?(.+)$ ]]; then
    die "checksums.txt contains a malformed line"
  fi
  DIGEST="sha256:${BASH_REMATCH[1]}"
  NAME=${BASH_REMATCH[2]}
  if [ "${NAME}" = checksums.txt ] || \
     ! jq -e --arg name "${NAME}" 'has($name)' <<< "${RELEASE_ASSET_DIGESTS}" >/dev/null; then
    die "checksums.txt references unexpected asset ${NAME}"
  fi
  if jq -e --arg name "${NAME}" 'has($name)' <<< "${CHECKSUM_ASSETS}" >/dev/null; then
    die "checksums.txt contains duplicate asset ${NAME}"
  fi
  API_DIGEST=$(jq -r --arg name "${NAME}" '.[$name]' <<< "${RELEASE_ASSET_DIGESTS}")
  if [ "${DIGEST}" != "${API_DIGEST}" ]; then
    die "checksum for ${NAME} does not match its GitHub asset digest"
  fi
  CHECKSUM_ASSETS=$(jq -c \
    --arg name "${NAME}" \
    --arg digest "${DIGEST}" \
    '. + {($name): $digest}' \
    <<< "${CHECKSUM_ASSETS}")
done < "${CHECKSUM_FILE}"
EXPECTED_ARCHIVE_COUNT=$(jq 'length - 1' <<< "${RELEASE_ASSET_DIGESTS}")
if [ "$(jq 'length' <<< "${CHECKSUM_ASSETS}")" -ne "${EXPECTED_ARCHIVE_COUNT}" ]; then
  die "checksums.txt does not cover every release archive"
fi

WORKFLOW_PATH=.github/workflows/hotfix-release.yml
WORKFLOW_REF=${GITHUB_WORKFLOW_REF:-${WORKFLOW_PATH}@local}
WORKFLOW_RUN_ID=${GITHUB_RUN_ID:-local}
WORKFLOW_RUN_ATTEMPT=${GITHUB_RUN_ATTEMPT:-local}
mkdir -p "$(dirname -- "${RECEIPT}")"
RECEIPT_TEMP=$(mktemp "${RECEIPT}.tmp.XXXXXX")
trap 'rm -f "${BASE_STATE}" "${EXPECTED_STATE}" "${CORE_RECEIPT}" "${RECEIPT_TEMP}"; rm -rf "${ASSET_DIR}"' EXIT
jq \
  --arg receipt_type hotfix-release \
  --argjson hotfix_schema_version 1 \
  --arg base_tag "${BASE_TAG}" \
  --arg base_commit "${EXPECTED_BASE_COMMIT}" \
  --arg state_sha256 "${STATE_SHA256}" \
  --argjson release_asset_digests "${RELEASE_ASSET_DIGESTS}" \
  --arg workflow_path "${WORKFLOW_PATH}" \
  --arg workflow_ref "${WORKFLOW_REF}" \
  --arg workflow_commit "${EXPECTED_COMMIT}" \
  --arg workflow_run_id "${WORKFLOW_RUN_ID}" \
  --arg workflow_run_attempt "${WORKFLOW_RUN_ATTEMPT}" '
    . + {
      receipt_type: $receipt_type,
      hotfix_schema_version: $hotfix_schema_version,
      previous_release: {
        tag: $base_tag,
        commit: $base_commit
      },
      upstream_state: {
        sync_id: .sync_id,
        plan_fingerprint: .plan_fingerprint,
        sha256: $state_sha256
      },
      release_asset_digests: $release_asset_digests,
      release_workflow: {
        path: $workflow_path,
        ref: $workflow_ref,
        commit: $workflow_commit,
        run_id: $workflow_run_id,
        run_attempt: $workflow_run_attempt
      }
    }
  ' "${CORE_RECEIPT}" > "${RECEIPT_TEMP}"
if [ -n "${ATTACHED_RECEIPT}" ]; then
  [ -f "${ATTACHED_RECEIPT}" ] \
    || die "attached receipt does not exist: ${ATTACHED_RECEIPT}"
  jq -e . "${ATTACHED_RECEIPT}" >/dev/null 2>&1 \
    || die "attached hotfix receipt is not valid JSON"
  if ! diff -u \
    <(jq -S . "${ATTACHED_RECEIPT}") \
    <(jq -S . "${RECEIPT_TEMP}"); then
    die "attached hotfix receipt does not match independent verification"
  fi
fi
mv "${RECEIPT_TEMP}" "${RECEIPT}"
trap 'rm -f "${BASE_STATE}" "${EXPECTED_STATE}" "${CORE_RECEIPT}"; rm -rf "${ASSET_DIR}"' EXIT

echo "[OK] verified hotfix release ${TAG} at ${EXPECTED_COMMIT}; receipt=${RECEIPT}"
