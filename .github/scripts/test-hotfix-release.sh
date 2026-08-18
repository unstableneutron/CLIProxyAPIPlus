#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
POLICY="${SCRIPT_DIR}/validate-hotfix-release.sh"
GHCR_IMAGE_STATE="${SCRIPT_DIR}/inspect-ghcr-image-state.sh"
IDENTITY_ABSENCE_CHECK="${SCRIPT_DIR}/confirm-hotfix-identities-absent.sh"
PUBLICATION_STATE_CHECK="${SCRIPT_DIR}/inspect-hotfix-publication-state.sh"
FINALIZATION_EVIDENCE_CHECK="${SCRIPT_DIR}/verify-hotfix-finalization-evidence.sh"
WORKFLOW="${SCRIPT_DIR}/../workflows/hotfix-release.yml"
DOCKER_WORKFLOW="${SCRIPT_DIR}/../workflows/docker-image.yml"
RECOVERY_WORKFLOW="${SCRIPT_DIR}/../workflows/sync-release-tag.yml"
UPSTREAM_WORKFLOW="${SCRIPT_DIR}/../workflows/upstream-sync-v2.yml"
BASE_TAG=v7.2.131-unstableneutron.0
HOTFIX_TAG=v7.2.131-unstableneutron.1

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

run_git() {
  git -c init.defaultBranch=main "$@"
}

assert_contains() {
  local file=$1
  local expected=$2
  grep -Fq -- "${expected}" "${file}" \
    || fail "expected ${file} to contain: ${expected}"
}

assert_not_contains() {
  local file=$1
  local unexpected=$2
  if grep -Fq -- "${unexpected}" "${file}"; then
    fail "expected ${file} not to contain: ${unexpected}"
  fi
}

output_value() {
  local file=$1
  local key=$2
  awk -F= -v key="${key}" \
    '$1 == key { sub(/^[^=]*=/, ""); print; exit }' \
    "${file}"
}

setup_policy_repo() {
  local root=$1
  local repo=${root}/repo
  local origin=${root}/origin.git
  mkdir -p "${repo}"
  run_git -C "${repo}" init -q
  run_git -C "${repo}" config user.name hotfix-test
  run_git -C "${repo}" config user.email hotfix-test@example.invalid
  cat > "${repo}/.ccs-fork-upstream.env" <<EOF
SCHEMA_VERSION=2
SYNC_ID=original-v7.2.131_plus-v7.2.127-3
PLAN_FINGERPRINT=eeef3819ca9dfb38b4528fc5dabc3324d538b19b
EXPECTED_FORK_TAG=${BASE_TAG}
EOF
  echo base > "${repo}/app.txt"
  run_git -C "${repo}" add .
  run_git -C "${repo}" commit -m base >/dev/null
  run_git -C "${repo}" tag -a "${BASE_TAG}" -m "Release ${BASE_TAG}"
  echo fixed > "${repo}/app.txt"
  run_git -C "${repo}" commit -am hotfix >/dev/null
  run_git clone -q --bare "${repo}" "${origin}"
  run_git -C "${repo}" remote add origin "${origin}"
  run_git -C "${repo}" fetch -q origin main:refs/remotes/origin/main
}

run_policy() {
  local repo=$1
  local output=$2
  local tag=$3
  local expected_commit=$4
  local base_tag=$5
  local base_commit=$6
  local existing_tag_policy=${7:-absent}
  (
    cd "${repo}"
    GITHUB_OUTPUT="${output}" "${POLICY}" \
      --tag "${tag}" \
      --expected-commit "${expected_commit}" \
      --base-tag "${base_tag}" \
      --expected-base-commit "${base_commit}" \
      --existing-tag-policy "${existing_tag_policy}"
  )
}

expect_policy_failure() {
  local repo=$1
  local expected_message=$2
  shift 2
  local output
  output=$(mktemp)
  if run_policy "${repo}" "${output}.fields" "$@" > "${output}" 2>&1; then
    fail "hotfix policy unexpectedly succeeded"
  fi
  assert_contains "${output}" "${expected_message}"
  rm -f "${output}" "${output}.fields"
}

test_policy_accepts_only_exact_next_release() {
  local root
  root=$(mktemp -d)
  setup_policy_repo "${root}"
  local repo=${root}/repo
  local base_commit hotfix_commit output
  base_commit=$(run_git -C "${repo}" rev-parse "${BASE_TAG}^{}")
  hotfix_commit=$(run_git -C "${repo}" rev-parse HEAD)
  output=${root}/policy.out

  run_policy \
    "${repo}" "${output}" "${HOTFIX_TAG}" "${hotfix_commit}" \
    "${BASE_TAG}" "${base_commit}" >/dev/null
  [ "$(output_value "${output}" expected_commit)" = "${hotfix_commit}" ] \
    || fail "policy output did not preserve expected commit"
  [ "$(output_value "${output}" base_commit)" = "${base_commit}" ] \
    || fail "policy output did not preserve base commit"
  [ "$(output_value "${output}" tag)" = "${HOTFIX_TAG}" ] \
    || fail "policy output did not preserve tag"
  [ "$(output_value "${output}" upstream_state_sha256)" = \
    "$(sha256sum "${repo}/.ccs-fork-upstream.env" | awk '{ print $1 }')" ] \
    || fail "policy output did not bind upstream state"

  touch "${repo}/untracked"
  expect_policy_failure \
    "${repo}" "requires a clean checkout" \
    "${HOTFIX_TAG}" "${hotfix_commit}" "${BASE_TAG}" "${base_commit}"
  rm "${repo}/untracked"

  expect_policy_failure \
    "${repo}" "origin/main resolves" \
    "${HOTFIX_TAG}" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    "${BASE_TAG}" "${base_commit}"
  expect_policy_failure \
    "${repo}" "hotfix tag must be the next suffix" \
    v7.2.131-unstableneutron.2 "${hotfix_commit}" \
    "${BASE_TAG}" "${base_commit}"
  expect_policy_failure \
    "${repo}" "base tag ${BASE_TAG} resolves" \
    "${HOTFIX_TAG}" "${hotfix_commit}" \
    "${BASE_TAG}" bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

  run_git -C "${repo}" tag -a "${HOTFIX_TAG}" -m reused
  expect_policy_failure \
    "${repo}" "local hotfix tag ${HOTFIX_TAG} exists without the remote identity" \
    "${HOTFIX_TAG}" "${hotfix_commit}" "${BASE_TAG}" "${base_commit}"
  run_git -C "${repo}" tag -d "${HOTFIX_TAG}" >/dev/null

  echo changed >> "${repo}/.ccs-fork-upstream.env"
  run_git -C "${repo}" commit -am "change represented state" >/dev/null
  local changed_commit
  changed_commit=$(run_git -C "${repo}" rev-parse HEAD)
  run_git -C "${repo}" push -q origin +HEAD:main
  run_git -C "${repo}" fetch -q origin main:refs/remotes/origin/main
  expect_policy_failure \
    "${repo}" "changed .ccs-fork-upstream.env" \
    "${HOTFIX_TAG}" "${changed_commit}" "${BASE_TAG}" "${base_commit}"

  rm -rf "${root}"
}

test_policy_adopts_only_exact_pushed_tag_after_side_effect_failure() {
  local root
  root=$(mktemp -d)
  setup_policy_repo "${root}"
  local repo=${root}/repo base_commit hotfix_commit output
  base_commit=$(run_git -C "${repo}" rev-parse "${BASE_TAG}^{}")
  hotfix_commit=$(run_git -C "${repo}" rev-parse HEAD)
  run_git -C "${repo}" config user.name 'cliproxy-hotfix-release[bot]'
  run_git -C "${repo}" config user.email 'cliproxy-hotfix-release@users.noreply.github.com'
  GIT_COMMITTER_NAME='cliproxy-hotfix-release[bot]' \
    GIT_COMMITTER_EMAIL=cliproxy-hotfix-release@users.noreply.github.com \
    run_git -C "${repo}" tag -a "${HOTFIX_TAG}" \
      -m "Hotfix release ${HOTFIX_TAG} after ${BASE_TAG}"
  run_git -C "${repo}" push -q origin "refs/tags/${HOTFIX_TAG}"
  output=${root}/recovered.out
  run_policy \
    "${repo}" "${output}" "${HOTFIX_TAG}" "${hotfix_commit}" \
    "${BASE_TAG}" "${base_commit}" exact >/dev/null
  [ "$(output_value "${output}" tag_state)" = exact ] \
    || fail "pushed exact tag was not classified as resumable"

  run_git -C "${repo}" tag -d "${HOTFIX_TAG}" >/dev/null
  GIT_COMMITTER_NAME='cliproxy-hotfix-release[bot]' \
    GIT_COMMITTER_EMAIL=cliproxy-hotfix-release@users.noreply.github.com \
    run_git -C "${repo}" tag -a "${HOTFIX_TAG}" -m "wrong message"
  run_git -C "${repo}" push -q --force origin "refs/tags/${HOTFIX_TAG}"
  expect_policy_failure \
    "${repo}" "unexpected message" \
    "${HOTFIX_TAG}" "${hotfix_commit}" "${BASE_TAG}" "${base_commit}" exact
  rm -rf "${root}"
}

test_policy_accepts_consecutive_chained_suffixes() {
  local root
  root=$(mktemp -d)
  setup_policy_repo "${root}"
  local repo=${root}/repo
  local first_commit second_commit third_commit output
  first_commit=$(run_git -C "${repo}" rev-parse HEAD)
  run_git -C "${repo}" tag -a "${HOTFIX_TAG}" \
    -m "Hotfix release ${HOTFIX_TAG} after ${BASE_TAG}"

  echo second > "${repo}/app.txt"
  run_git -C "${repo}" commit -am "second hotfix" >/dev/null
  second_commit=$(run_git -C "${repo}" rev-parse HEAD)
  run_git -C "${repo}" push -q origin +HEAD:main "refs/tags/${HOTFIX_TAG}"
  run_git -C "${repo}" fetch -q origin main:refs/remotes/origin/main
  output=${root}/second.out
  run_policy \
    "${repo}" "${output}" v7.2.131-unstableneutron.2 "${second_commit}" \
    "${HOTFIX_TAG}" "${first_commit}" >/dev/null
  [ "$(output_value "${output}" root_tag)" = "${BASE_TAG}" ] \
    || fail "second hotfix did not preserve the accepted upstream root"

  run_git -C "${repo}" tag -a v7.2.131-unstableneutron.2 \
    -m "Hotfix release v7.2.131-unstableneutron.2 after ${HOTFIX_TAG}"
  echo third > "${repo}/app.txt"
  run_git -C "${repo}" commit -am "third hotfix" >/dev/null
  third_commit=$(run_git -C "${repo}" rev-parse HEAD)
  run_git -C "${repo}" push -q origin +HEAD:main refs/tags/v7.2.131-unstableneutron.2
  run_git -C "${repo}" fetch -q origin main:refs/remotes/origin/main
  run_policy \
    "${repo}" "${root}/third.out" v7.2.131-unstableneutron.3 "${third_commit}" \
    v7.2.131-unstableneutron.2 "${second_commit}" >/dev/null

  expect_policy_failure \
    "${repo}" "hotfix tag must be the next suffix" \
    v7.2.131-unstableneutron.4 "${third_commit}" \
    v7.2.131-unstableneutron.2 "${second_commit}"
  rm -rf "${root}"
}

test_workflow_contract_is_fail_closed() {
  assert_contains "${WORKFLOW}" "workflow_dispatch:"
  assert_not_contains "${WORKFLOW}" "schedule:"
  assert_not_contains "${WORKFLOW}" "pull_request:"
  assert_contains "${WORKFLOW}" "github.actor"
  assert_contains "${WORKFLOW}" "github.ref"
  assert_contains "${WORKFLOW}" "validate-hotfix-release.sh"
  assert_contains "${WORKFLOW}" "Verify complete previous release chain"
  assert_contains "${WORKFLOW}" "verify-hotfix-chain.sh"
  assert_contains "${WORKFLOW}" "test-verify-hotfix-chain.sh"
  assert_contains "${WORKFLOW}" "Classify absent or exact resumable publication state"
  assert_contains "${WORKFLOW}" ".github/scripts/inspect-hotfix-publication-state.sh"
  assert_contains "${WORKFLOW}" ".github/scripts/verify-hotfix-finalization-evidence.sh"
  assert_contains "${WORKFLOW}" ".github/scripts/confirm-hotfix-identities-absent.sh"
  assert_not_contains "${WORKFLOW}" 'if docker buildx imagetools inspect'
  assert_contains "${DOCKER_WORKFLOW}" "inspect_image_state()"
  assert_not_contains "${DOCKER_WORKFLOW}" ".github/scripts/inspect-ghcr-image-state.sh"
  assert_not_contains "${DOCKER_WORKFLOW}" '2>/dev/null)"; then'
  # shellcheck disable=SC2016 # The workflow shell expression is asserted literally.
  assert_contains "${WORKFLOW}" 'git push origin "refs/tags/${TAG}"'
  assert_contains "${WORKFLOW}" "Adopting independently verified existing tag"
  assert_contains "${WORKFLOW}" "uses: ./.github/workflows/release.yaml"
  assert_contains "${WORKFLOW}" "uses: ./.github/workflows/docker-image.yml"
  assert_contains "${WORKFLOW}" "verify-hotfix-release.sh"
  assert_contains "${WORKFLOW}" "hotfix-release-receipt.json"
  assert_contains "${WORKFLOW}" "--attached-receipt"
  assert_contains "${WORKFLOW}" "Publish or adopt immutable hotfix receipt"
  local artifact_line receipt_upload_line
  # shellcheck disable=SC2016 # GitHub expression is asserted literally.
  artifact_line=$(grep -nF 'name: hotfix-release-receipt-${{ github.run_id }}-${{ github.run_attempt }}' \
    "${WORKFLOW}" | tail -n 1 | cut -d: -f1)
  # shellcheck disable=SC2016 # Workflow shell expression is asserted literally.
  receipt_upload_line=$(grep -nF 'gh release upload "${TAG}" hotfix-release-receipt.json' \
    "${WORKFLOW}" | tail -n 1 | cut -d: -f1)
  [ "${artifact_line}" -lt "${receipt_upload_line}" ] \
    || fail "receipt must publish only after its complete Actions artifact"
  assert_contains "${WORKFLOW}" "Require final fetched no-op plan"
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" has_changes'
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" target_drift'
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" blocked'
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" plan_fingerprint'
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" candidate_branch'
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" next_fork_tag'
  # shellcheck disable=SC2016 # The workflow shell expression is asserted literally.
  assert_contains "${RECOVERY_WORKFLOW}" 'TAG}" != "${RECORDED_RELEASE_TAG}'
  assert_contains "${UPSTREAM_WORKFLOW}" "hotfix-release-receipt.json"
  assert_contains "${UPSTREAM_WORKFLOW}" "verify-hotfix-release.sh"
}

test_publication_state_requires_correlated_identities() {
  local root gh docker output
  root=$(mktemp -d)
  gh=${root}/gh
  docker=${root}/docker
  output=${root}/output
  cat > "${gh}" <<'EOF'
#!/usr/bin/env bash
if [ "${PUBLICATION_RELEASE_STATE:?}" = absent ]; then
  printf 'HTTP/2.0 404 Not Found\n'
  exit 1
fi
exit 99
EOF
  cat > "${docker}" <<'EOF'
#!/usr/bin/env bash
ref=${*: -1}
case "${PUBLICATION_IMAGE_STATE:?}:${ref}" in
  absent:*) echo "ERROR: ${ref}: not found" >&2; exit 1 ;;
  partial:*amd64) echo 'Name: test'; exit 0 ;;
  partial:*) echo "ERROR: ${ref}: not found" >&2; exit 1 ;;
  *) exit 99 ;;
esac
EOF
  chmod +x "${gh}" "${docker}"
  GITHUB_OUTPUT=${output} PUBLICATION_RELEASE_STATE=absent PUBLICATION_IMAGE_STATE=absent \
    PATH="${root}:${PATH}" "${PUBLICATION_STATE_CHECK}" \
      test owner/repository ghcr.io/example/image absent >/dev/null
  [ "$(output_value "${output}" publication_state)" = absent ] \
    || fail "fully absent publication was not classified as absent"

  : > "${output}"
  GITHUB_OUTPUT=${output} PUBLICATION_RELEASE_STATE=absent PUBLICATION_IMAGE_STATE=partial \
    PATH="${root}:${PATH}" "${PUBLICATION_STATE_CHECK}" \
      test owner/repository ghcr.io/example/image exact >/dev/null
  [ "$(output_value "${output}" publication_state)" = publishing ] \
    || fail "exact-tag partial architecture publication was not resumable"
  if GITHUB_OUTPUT=${output} PUBLICATION_RELEASE_STATE=absent PUBLICATION_IMAGE_STATE=partial \
    PATH="${root}:${PATH}" "${PUBLICATION_STATE_CHECK}" \
      test owner/repository ghcr.io/example/image absent >/dev/null 2>&1; then
    fail "publication state accepted image identities without an exact tag"
  fi
  rm -rf "${root}"
}

test_finalization_recovers_exact_prior_attempt_evidence() {
  local root gh receipt final_plan artifact bomb digest size output conclusion
  root=$(mktemp -d)
  gh=${root}/gh
  receipt=${root}/hotfix-release-receipt.json
  final_plan=${root}/final-plan.out
  artifact=${root}/artifact.zip
  output=${root}/output
  cat > "${receipt}" <<EOF
{"workflow_run_id":"123","release_workflow":{"commit":"$(printf 'a%.0s' {1..40})","run_id":"123","run_attempt":"1"}}
EOF
  printf 'deterministic=plan\n' > "${final_plan}"
  cp "${receipt}" "${root}/independently-verified-receipt.json"
  python3 - "${root}" "${artifact}" <<'PY'
import os
import sys
import zipfile

root, artifact = sys.argv[1:]
with zipfile.ZipFile(artifact, "w", zipfile.ZIP_STORED) as archive:
    for name in (
        "hotfix-release-receipt.json",
        "independently-verified-receipt.json",
        "final-plan.out",
    ):
        archive.write(os.path.join(root, name), name)
PY
  digest="sha256:$(sha256sum "${artifact}" | awk '{ print $1 }')"
  size=$(stat -c %s "${artifact}")
  cat > "${gh}" <<'EOF'
#!/usr/bin/env bash
endpoint=${*: -1}
case "${endpoint}" in
  */actions/runs/123/attempts/1)
    printf '%s\n' "${ATTEMPT_JSON}"
    ;;
  */actions/runs/123/artifacts?per_page=100)
    printf '%s\n' "${ARTIFACTS_JSON}"
    ;;
  */actions/artifacts/456/zip)
    cat "${SOURCE_ARTIFACT_ZIP}"
    ;;
  *) echo "unexpected endpoint ${endpoint}" >&2; exit 1 ;;
esac
EOF
  chmod +x "${gh}"
  local commit
  commit=$(printf 'a%.0s' {1..40})
  ARTIFACTS_JSON=$(jq -cn \
    --arg digest "${digest}" \
    --argjson size "${size}" \
    --arg commit "${commit}" '{total_count:1,artifacts:[{id:456,name:"hotfix-release-receipt-123-1",digest:$digest,size_in_bytes:$size,expired:false,archive_download_url:"https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/actions/artifacts/456/zip",workflow_run:{id:123,repository_id:1247056725,head_repository_id:1247056725,head_sha:$commit}}]}')
  export ARTIFACTS_JSON SOURCE_ARTIFACT_ZIP=${artifact}
  for conclusion in failure cancelled timed_out; do
    ATTEMPT_JSON=$(jq -cn \
      --arg commit "${commit}" \
      --arg conclusion "${conclusion}" \
      '{id:123,run_attempt:1,path:".github/workflows/hotfix-release.yml",event:"workflow_dispatch",head_branch:"main",head_sha:$commit,status:"completed",conclusion:$conclusion,actor:{login:"unstableneutron",id:156744497},repository:{full_name:"unstableneutron/CLIProxyAPIPlus",id:1247056725}}')
    export ATTEMPT_JSON
    GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=2 \
      PATH="${root}:${PATH}" "${FINALIZATION_EVIDENCE_CHECK}" \
        "${receipt}" "${final_plan}" > "${output}"
    assert_contains "${output}" "adopted hotfix finalization evidence"
  done
  ATTEMPT_JSON=$(jq -c '.conclusion = "success"' <<< "${ATTEMPT_JSON}")
  export ATTEMPT_JSON
  if GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=2 \
    PATH="${root}:${PATH}" "${FINALIZATION_EVIDENCE_CHECK}" \
      "${receipt}" "${final_plan}" >/dev/null 2>&1; then
    fail "finalization recovery accepted an earlier successful attempt"
  fi
  ATTEMPT_JSON=$(jq -c '.conclusion = "failure"' <<< "${ATTEMPT_JSON}")
  export ATTEMPT_JSON
  printf 'tampered=plan\n' > "${final_plan}"
  if GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=2 \
    PATH="${root}:${PATH}" "${FINALIZATION_EVIDENCE_CHECK}" \
      "${receipt}" "${final_plan}" >/dev/null 2>&1; then
    fail "finalization recovery accepted a mismatched deterministic plan"
  fi
  printf 'deterministic=plan\n' > "${final_plan}"
  bomb=${root}/bomb.zip
  python3 - "${root}" "${bomb}" <<'PY'
import pathlib
import sys
import zipfile

root = pathlib.Path(sys.argv[1])
with zipfile.ZipFile(sys.argv[2], "w", zipfile.ZIP_DEFLATED) as archive:
    archive.write(root / "hotfix-release-receipt.json", "hotfix-release-receipt.json")
    archive.write(root / "independently-verified-receipt.json", "independently-verified-receipt.json")
    archive.writestr("final-plan.out", b"x" * 1_000_001)
PY
  digest="sha256:$(sha256sum "${bomb}" | awk '{ print $1 }')"
  size=$(stat -c %s "${bomb}")
  ARTIFACTS_JSON=$(jq -c \
    --arg digest "${digest}" --argjson size "${size}" \
    '.artifacts[0].digest = $digest | .artifacts[0].size_in_bytes = $size' \
    <<< "${ARTIFACTS_JSON}")
  SOURCE_ARTIFACT_ZIP=${bomb}
  export ARTIFACTS_JSON SOURCE_ARTIFACT_ZIP
  if GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=2 \
    PATH="${root}:${PATH}" "${FINALIZATION_EVIDENCE_CHECK}" \
      "${receipt}" "${final_plan}" > "${output}" 2>&1; then
    fail "finalization recovery accepted an oversized compressed artifact member"
  fi
  assert_contains "${output}" "archive member exceeds its output limit"
  unset ATTEMPT_JSON ARTIFACTS_JSON SOURCE_ARTIFACT_ZIP
  rm -rf "${root}"
}

test_ghcr_image_state_is_fail_closed() {
  local root docker output state present_output
  root=$(mktemp -d)
  docker=${root}/docker
  output=${root}/output
  cat > "${docker}" <<'EOF'
#!/usr/bin/env bash
case "${DOCKER_TEST_CASE:?}" in
  present) printf 'Name: ghcr.io/example/image:test\nDigest: sha256:%064d\n' 0; exit 0 ;;
  not_found) echo 'ERROR: ghcr.io/example/image:test: not found' >&2; exit 1 ;;
  manifest_unknown) echo 'ERROR: ghcr.io/example/image:test: manifest unknown' >&2; exit 1 ;;
  unauthorized) echo 'ERROR: unexpected status from HEAD request: 401 Unauthorized' >&2; exit 1 ;;
  forbidden) echo 'ERROR: unexpected status from HEAD request: 403 Forbidden' >&2; exit 1 ;;
  rate_limited) echo 'ERROR: unexpected status from HEAD request: 429 Too Many Requests' >&2; exit 1 ;;
  server_error) echo 'ERROR: unexpected status from HEAD request: 500 Internal Server Error' >&2; exit 1 ;;
  network) echo 'ERROR: failed to do request: dial tcp: network is unreachable' >&2; exit 1 ;;
  timeout) echo 'ERROR: failed to do request: context deadline exceeded' >&2; exit 1 ;;
  mixed) printf 'ERROR: unexpected status from HEAD request: 401 Unauthorized\nERROR: ghcr.io/example/image:test: not found\n' >&2; exit 1 ;;
  unknown) echo 'ERROR: unclassified registry response' >&2; exit 1 ;;
esac
EOF
  chmod +x "${docker}"

  for accepted_case in not_found manifest_unknown; do
    state=$(DOCKER_TEST_CASE=${accepted_case} PATH="${root}:${PATH}" \
      "${GHCR_IMAGE_STATE}" ghcr.io/example/image:test)
    [ "${state}" = absent ] \
      || fail "GHCR image state did not confirm ${accepted_case} as absent"
  done

  present_output=${root}/present
  state=$(DOCKER_TEST_CASE=present PATH="${root}:${PATH}" \
    "${GHCR_IMAGE_STATE}" ghcr.io/example/image:test "${present_output}")
  [ "${state}" = present ] || fail "GHCR image state did not report an existing image"
  assert_contains "${present_output}" "Digest: sha256:"

  for rejected_case in unauthorized forbidden rate_limited server_error network timeout mixed unknown; do
    if DOCKER_TEST_CASE=${rejected_case} PATH="${root}:${PATH}" \
      "${GHCR_IMAGE_STATE}" ghcr.io/example/image:test > "${output}" 2>&1; then
      fail "GHCR image state accepted ambiguous ${rejected_case} response"
    fi
  done
  rm -rf "${root}"
}

test_candidate_identity_absence_is_fail_closed() {
  local root git gh docker output status_case
  root=$(mktemp -d)
  git=${root}/git
  gh=${root}/gh
  docker=${root}/docker
  output=${root}/output
  cat > "${git}" <<'EOF'
#!/usr/bin/env bash
case "${GIT_TEST_CASE:?}" in
  absent) exit 2 ;;
  present) echo 'commit refs/tags/test'; exit 0 ;;
  operational) echo 'fatal: unable to access origin' >&2; exit 128 ;;
esac
EOF
  cat > "${gh}" <<'EOF'
#!/usr/bin/env bash
case "${GH_TEST_CASE:?}" in
  absent) echo 'HTTP/2.0 404 Not Found'; exit 1 ;;
  present) echo 'HTTP/2.0 200 OK'; exit 0 ;;
  unauthorized) echo 'HTTP/2.0 401 Unauthorized'; exit 1 ;;
  rate_limited) echo 'HTTP/2.0 429 Too Many Requests'; exit 1 ;;
  server_error) echo 'HTTP/2.0 500 Internal Server Error'; exit 1 ;;
  network) echo 'failed to connect to api.github.com' >&2; exit 1 ;;
esac
EOF
  cat > "${docker}" <<'EOF'
#!/usr/bin/env bash
case "${DOCKER_TEST_CASE:?}" in
  absent) echo 'ERROR: ghcr.io/example/image:test: not found' >&2; exit 1 ;;
  present) printf 'Name: ghcr.io/example/image:test\nDigest: sha256:%064d\n' 0; exit 0 ;;
esac
EOF
  chmod +x "${git}" "${gh}" "${docker}"

  if ! GIT_TEST_CASE=absent GH_TEST_CASE=absent DOCKER_TEST_CASE=absent \
    PATH="${root}:${PATH}" "${IDENTITY_ABSENCE_CHECK}" \
      test owner/repository ghcr.io/example/image > "${output}" 2>&1; then
    fail "candidate identity check rejected confirmed absence"
  fi

  for status_case in present operational; do
    if GIT_TEST_CASE=${status_case} GH_TEST_CASE=absent DOCKER_TEST_CASE=absent \
      PATH="${root}:${PATH}" "${IDENTITY_ABSENCE_CHECK}" \
        test owner/repository ghcr.io/example/image > "${output}" 2>&1; then
      fail "candidate identity check accepted git ${status_case}"
    fi
  done
  for status_case in present unauthorized rate_limited server_error network; do
    if GIT_TEST_CASE=absent GH_TEST_CASE=${status_case} DOCKER_TEST_CASE=absent \
      PATH="${root}:${PATH}" "${IDENTITY_ABSENCE_CHECK}" \
        test owner/repository ghcr.io/example/image > "${output}" 2>&1; then
      fail "candidate identity check accepted GitHub ${status_case}"
    fi
  done
  if GIT_TEST_CASE=absent GH_TEST_CASE=absent DOCKER_TEST_CASE=present \
    PATH="${root}:${PATH}" "${IDENTITY_ABSENCE_CHECK}" \
      test owner/repository ghcr.io/example/image > "${output}" 2>&1; then
    fail "candidate identity check accepted existing GHCR identity"
  fi
  rm -rf "${root}"
}

main() {
  [ -x "${POLICY}" ] || fail "policy script is missing or not executable"
  [ -x "${GHCR_IMAGE_STATE}" ] || fail "GHCR image state checker is missing or not executable"
  [ -x "${IDENTITY_ABSENCE_CHECK}" ] || fail "candidate identity checker is missing or not executable"
  [ -x "${PUBLICATION_STATE_CHECK}" ] || fail "publication state checker is missing or not executable"
  [ -x "${FINALIZATION_EVIDENCE_CHECK}" ] || fail "finalization evidence checker is missing or not executable"
  test_policy_accepts_only_exact_next_release
  test_policy_adopts_only_exact_pushed_tag_after_side_effect_failure
  test_policy_accepts_consecutive_chained_suffixes
  test_workflow_contract_is_fail_closed
  test_publication_state_requires_correlated_identities
  test_finalization_recovers_exact_prior_attempt_evidence
  test_ghcr_image_state_is_fail_closed
  test_candidate_identity_absence_is_fail_closed
  echo "[OK] hotfix release policy tests passed"
}

main "$@"
