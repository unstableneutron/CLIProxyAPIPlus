#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
POLICY="${SCRIPT_DIR}/validate-hotfix-release.sh"
WORKFLOW="${SCRIPT_DIR}/../workflows/hotfix-release.yml"
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
  (
    cd "${repo}"
    GITHUB_OUTPUT="${output}" "${POLICY}" \
      --tag "${tag}" \
      --expected-commit "${expected_commit}" \
      --base-tag "${base_tag}" \
      --expected-base-commit "${base_commit}"
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
    "${repo}" "hotfix tag ${HOTFIX_TAG} already exists" \
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
  assert_contains "${WORKFLOW}" "Reject reused or partially published identity"
  # shellcheck disable=SC2016 # The workflow shell expression is asserted literally.
  assert_contains "${WORKFLOW}" 'git push origin "refs/tags/${TAG}"'
  assert_contains "${WORKFLOW}" "uses: ./.github/workflows/release.yaml"
  assert_contains "${WORKFLOW}" "uses: ./.github/workflows/docker-image.yml"
  assert_contains "${WORKFLOW}" "verify-hotfix-release.sh"
  assert_contains "${WORKFLOW}" "hotfix-release-receipt.json"
  assert_contains "${WORKFLOW}" "--attached-receipt"
  assert_contains "${WORKFLOW}" "Require final fetched no-op plan"
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" has_changes'
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" target_drift'
  # shellcheck disable=SC2016 # Workflow shell expressions are asserted literally.
  assert_contains "${WORKFLOW}" 'plan_value "${FINAL_PLAN}" blocked'
  # shellcheck disable=SC2016 # The workflow shell expression is asserted literally.
  assert_contains "${RECOVERY_WORKFLOW}" 'TAG}" != "${RECORDED_RELEASE_TAG}'
  assert_contains "${UPSTREAM_WORKFLOW}" "hotfix-release-receipt.json"
  assert_contains "${UPSTREAM_WORKFLOW}" "verify-hotfix-release.sh"
}

main() {
  [ -x "${POLICY}" ] || fail "policy script is missing or not executable"
  test_policy_accepts_only_exact_next_release
  test_policy_accepts_consecutive_chained_suffixes
  test_workflow_contract_is_fail_closed
  echo "[OK] hotfix release policy tests passed"
}

main "$@"
