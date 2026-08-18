#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "[hotfix-finalization-evidence] $*" >&2
  exit 1
}

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <attached-receipt> <regenerated-final-plan>" >&2
  exit 2
fi
RECEIPT=$1
FINAL_PLAN=$2
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"
: "${GITHUB_RUN_ATTEMPT:?GITHUB_RUN_ATTEMPT is required}"
[ "${GITHUB_REPOSITORY}" = unstableneutron/CLIProxyAPIPlus ] \
  || die "unexpected repository ${GITHUB_REPOSITORY}"
[[ "${GITHUB_RUN_ID}" =~ ^[1-9][0-9]*$ ]] || die "current workflow run ID is invalid"
[[ "${GITHUB_RUN_ATTEMPT}" =~ ^[1-9][0-9]*$ ]] || die "current workflow attempt is invalid"
[ -s "${RECEIPT}" ] || die "attached receipt is missing"
[ -s "${FINAL_PLAN}" ] || die "regenerated final plan is missing"

EVIDENCE_RUN_ID=$(jq -er \
  '.workflow_run_id | select(type == "string" and test("^[1-9][0-9]*$"))' \
  "${RECEIPT}") || die "attached receipt workflow run ID is invalid"
EVIDENCE_ATTEMPT=$(jq -er \
  '.release_workflow.run_attempt | select(type == "string" and test("^[1-9][0-9]*$"))' \
  "${RECEIPT}") || die "attached receipt workflow attempt is invalid"
if [ "${EVIDENCE_RUN_ID}" != "${GITHUB_RUN_ID}" ] || \
   [ "${EVIDENCE_ATTEMPT}" -ge "${GITHUB_RUN_ATTEMPT}" ]; then
  die "attached receipt is not evidence from an earlier attempt of this run"
fi
EXPECTED_COMMIT=$(jq -er '.release_workflow.commit | select(type == "string" and test("^[0-9a-f]{40}$"))' \
  "${RECEIPT}") || die "attached receipt workflow commit is invalid"

ROOT=$(mktemp -d)
trap 'rm -rf "${ROOT}"' EXIT
ATTEMPT_FILE=${ROOT}/attempt.json
gh api "/repos/${GITHUB_REPOSITORY}/actions/runs/${EVIDENCE_RUN_ID}/attempts/${EVIDENCE_ATTEMPT}" \
  > "${ATTEMPT_FILE}"
jq -e \
  --arg path .github/workflows/hotfix-release.yml \
  --arg commit "${EXPECTED_COMMIT}" \
  --argjson run_id "${EVIDENCE_RUN_ID}" \
  --argjson attempt "${EVIDENCE_ATTEMPT}" '
    .id == $run_id and .run_attempt == $attempt and
    .path == $path and .event == "workflow_dispatch" and
    .head_branch == "main" and .head_sha == $commit and
    .status == "completed" and
    (.conclusion == "success" or .conclusion == "failure" or
      .conclusion == "cancelled" or .conclusion == "timed_out") and
    .actor.login == "unstableneutron" and .actor.id == 156744497 and
    .repository.full_name == "unstableneutron/CLIProxyAPIPlus" and
    .repository.id == 1247056725
  ' "${ATTEMPT_FILE}" >/dev/null \
  || die "receipt publication attempt has an unexpected identity"

ARTIFACT_NAME="hotfix-release-receipt-${EVIDENCE_RUN_ID}-${EVIDENCE_ATTEMPT}"
ARTIFACTS=${ROOT}/artifacts.json
gh api "/repos/${GITHUB_REPOSITORY}/actions/runs/${EVIDENCE_RUN_ID}/artifacts?per_page=100" \
  > "${ARTIFACTS}"
jq -e '.total_count == (.artifacts | length)' "${ARTIFACTS}" >/dev/null \
  || die "workflow artifact listing is incomplete"
[ "$(jq --arg name "${ARTIFACT_NAME}" '[.artifacts[] | select(.name == $name)] | length' \
  "${ARTIFACTS}")" -eq 1 ] || die "receipt evidence artifact is missing or duplicated"
jq -e \
  --arg name "${ARTIFACT_NAME}" \
  --arg commit "${EXPECTED_COMMIT}" \
  --argjson run_id "${EVIDENCE_RUN_ID}" '
    [.artifacts[] | select(.name == $name)] | .[0] |
    .expired == false and
    (.id | type) == "number" and (.id | floor) == .id and .id > 0 and .id <= 9007199254740991 and
    (.size_in_bytes | type) == "number" and (.size_in_bytes | floor) == .size_in_bytes and
      .size_in_bytes > 0 and .size_in_bytes <= 4000000 and
    (.digest | type) == "string" and (.digest | test("^sha256:[0-9a-f]{64}$")) and
    .archive_download_url == ("https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/actions/artifacts/" + (.id | tostring) + "/zip") and
    .workflow_run.id == $run_id and .workflow_run.repository_id == 1247056725 and
    .workflow_run.head_repository_id == 1247056725 and .workflow_run.head_sha == $commit
  ' "${ARTIFACTS}" >/dev/null || die "receipt evidence artifact identity differs"

ARTIFACT_ID=$(jq -r --arg name "${ARTIFACT_NAME}" '.artifacts[] | select(.name == $name) | .id' "${ARTIFACTS}")
ARTIFACT_SIZE=$(jq -r --arg name "${ARTIFACT_NAME}" '.artifacts[] | select(.name == $name) | .size_in_bytes' "${ARTIFACTS}")
ARTIFACT_DIGEST=$(jq -r --arg name "${ARTIFACT_NAME}" '.artifacts[] | select(.name == $name) | .digest' "${ARTIFACTS}")
ARTIFACT_ZIP=${ROOT}/artifact.zip
gh api -H 'Accept: application/vnd.github+json' \
  "/repos/${GITHUB_REPOSITORY}/actions/artifacts/${ARTIFACT_ID}/zip" > "${ARTIFACT_ZIP}"
if [ "$(stat -c %s "${ARTIFACT_ZIP}")" -ne "${ARTIFACT_SIZE}" ] || \
   [ "sha256:$(sha256sum "${ARTIFACT_ZIP}" | awk '{ print $1 }')" != "${ARTIFACT_DIGEST}" ]; then
  die "receipt evidence artifact bytes differ"
fi
diff -u \
  <(printf '%s\n' final-plan.out hotfix-release-receipt.json independently-verified-receipt.json | sort) \
  <(unzip -Z1 "${ARTIFACT_ZIP}" | sed 's#^.*/##' | sort) >/dev/null \
  || die "receipt evidence artifact file set differs"
cmp -s <(unzip -p "${ARTIFACT_ZIP}" hotfix-release-receipt.json) "${RECEIPT}" \
  || die "artifact receipt bytes differ"
cmp -s <(unzip -p "${ARTIFACT_ZIP}" independently-verified-receipt.json) "${RECEIPT}" \
  || die "independently verified artifact receipt bytes differ"
cmp -s <(unzip -p "${ARTIFACT_ZIP}" final-plan.out) "${FINAL_PLAN}" \
  || die "artifact final plan differs from deterministic regeneration"

echo "[OK] adopted hotfix finalization evidence from run ${EVIDENCE_RUN_ID} attempt ${EVIDENCE_ATTEMPT}"
