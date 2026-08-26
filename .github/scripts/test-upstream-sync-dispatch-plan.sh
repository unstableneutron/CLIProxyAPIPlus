#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
PLANNER="${SCRIPT_DIR}/upstream-sync-dispatch-plan.sh"
CLEANUP_ROOT=""

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

assert_contains() {
  local text=$1 expected=$2
  grep -Fq -- "${expected}" <<< "${text}" || fail "expected output to contain: ${expected}"
}
assert_not_contains() {
  local text=$1 unexpected=$2
  if grep -Fq -- "${unexpected}" <<< "${text}"; then
    fail "expected output not to contain: ${unexpected}"
  fi
}


expect_failure() {
  local expected=$1
  shift
  local output
  if output=$("${PLANNER}" "$@" 2>&1); then
    fail "dispatch planner unexpectedly succeeded"
  fi
  assert_contains "${output}" "${expected}"
}

main() {
  local root plan state output sha source_head digest
  root=$(mktemp -d)
  CLEANUP_ROOT=${root}
  trap 'rm -rf "${CLEANUP_ROOT}"' EXIT
  sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  source_head=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  digest=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  plan=${root}/plan.out
  state=${root}/run-state.json

  cat > "${plan}" <<EOF
has_changes=true
candidate_branch=upstream-sync/original-v7.2.142_plus-v7.2.127-7-537d553e444d
plan_fingerprint=537d553e444de13431d155950901285bdf69dca2
EOF
  output=$("${PLANNER}" repair --plan "${plan}" --repair-sha "${sha}" --repair-pr 91)
  assert_contains "${output}" "workflow run upstream-sync-v2.yml"
  assert_contains "${output}" "repair_ref=upstream-sync/original-v7.2.142_plus-v7.2.127-7-537d553e444d"
  assert_contains "${output}" "repair_sha=${sha}"
  assert_contains "${output}" "repair_pr=91"

  jq -n \
    --arg tag v7.2.142-rc.1.unstableneutron.0 \
    --arg sync_id original-v7.2.142-rc.1_plus-v7.2.127-7 \
    --arg fingerprint 537d553e444de13431d155950901285bdf69dca2 \
    --arg sha "${sha}" '{
      target: {expected_fork_tag: $tag, sync_id: $sync_id, plan_fingerprint: $fingerprint},
      candidate: {sha: $sha, acceptable: true},
      repair: {pr: 91}
    }' > "${state}"
  output=$("${PLANNER}" recovery \
    --state "${state}" \
    --tag v7.2.142-rc.1.unstableneutron.0 \
    --commit "${sha}" \
    --source-run-id 32947403342 \
    --source-run-attempt 1 \
    --artifact-id 59559356 \
    --artifact-name staged-release-assets-32947403342-1 \
    --artifact-digest "${digest}" \
    --source-head "${source_head}")
  assert_contains "${output}" "workflow run sync-release-tag.yml"
  assert_contains "${output}" "expected_sync_id=original-v7.2.142-rc.1_plus-v7.2.127-7"
  assert_contains "${output}" "staged_artifact_id=59559356"
  assert_contains "${output}" "staged_artifact_digest=${digest}"
  assert_contains "${output}" "source_workflow_head_sha=${source_head}"
  assert_not_contains "${output}" "artifact_name="
  assert_not_contains "${output}" "repair_pr="

  jq '.target.expected_fork_tag = "v7.2.142-rc.1.unstableneutron.1"' \
    "${state}" > "${state}.hotfix"
  expect_failure "tag is not an upstream root" recovery \
    --state "${state}.hotfix" \
    --tag v7.2.142-rc.1.unstableneutron.1 \
    --commit "${sha}" \
    --source-run-id 1 \
    --source-run-attempt 1 \
    --artifact-id 1 \
    --artifact-name staged-release-assets-1-1 \
    --artifact-digest "${digest}" \
    --source-head "${source_head}"

  jq '.repair.pr = null' "${state}" > "${state}.invalid"
  expect_failure "requires a recorded repair PR" recovery \
    --state "${state}.invalid" \
    --tag v7.2.142-rc.1.unstableneutron.0 \
    --commit "${sha}" \
    --source-run-id 1 \
    --source-run-attempt 1 \
    --artifact-id 1 \
    --artifact-name staged-release-assets-1-1 \
    --artifact-digest "${digest}" \
    --source-head "${source_head}"

  echo "[OK] upstream-sync dispatch plan tests passed"
}

main "$@"
