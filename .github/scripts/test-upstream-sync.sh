#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
HELPER="${SCRIPT_DIR}/upstream-sync.sh"
VALIDATOR="${SCRIPT_DIR}/validate-upstream-sync.sh"
RENDERER="${SCRIPT_DIR}/render-upstream-sync-report.sh"
export UPSTREAM_SYNC_OWNERSHIP_FILE="${UPSTREAM_SYNC_OWNERSHIP_FILE:-${SCRIPT_DIR}/../upstream-sync-ownership.tsv}"
export MODELS_REMOTE="${MODELS_REMOTE:-models-upstream}"

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

assert_contains() {
  local file=$1
  local expected=$2
  if ! grep -Fq -- "${expected}" "${file}"; then
    echo "--- ${file} ---" >&2
    cat "${file}" >&2
    fail "expected ${file} to contain: ${expected}"
  fi
}

assert_not_contains() {
  local file=$1
  local unexpected=$2
  if grep -Fq -- "${unexpected}" "${file}"; then
    echo "--- ${file} ---" >&2
    grep -Fn -- "${unexpected}" "${file}" >&2 || true
    fail "expected ${file} not to contain: ${unexpected}"
  fi
}

output_value() {
  local file=$1
  local key=$2
  awk -F= -v key="${key}" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "${file}"
}

assert_output_key() {
  local file=$1
  local key=$2
  local value
  value=$(output_value "${file}" "${key}")
  if [ -z "${value}" ]; then
    fail "expected non-empty ${key} in ${file}"
  fi
}

assert_equal() {
  local expected=$1
  local actual=$2
  local description=$3
  if [ "${expected}" != "${actual}" ]; then
    fail "${description}: expected ${expected}, got ${actual}"
  fi
}

run_git() {
  git -c init.defaultBranch=main "$@"
}

commit_file() {
  local repo=$1
  local path=$2
  local content=$3
  local message=$4

  mkdir -p "${repo}/$(dirname -- "${path}")"
  printf '%s\n' "${content}" > "${repo}/${path}"
  run_git -C "${repo}" add "${path}"
  run_git -C "${repo}" commit -m "${message}" >/dev/null
}

new_repo() {
  local repo=$1
  mkdir -p "${repo}"
  run_git -C "${repo}" init -q
  run_git -C "${repo}" config user.name "sync-test"
  run_git -C "${repo}" config user.email "sync-test@example.invalid"
}

clone_for_fork() {
  local src=$1
  local dst=$2
  run_git clone -q "${src}" "${dst}"
  run_git -C "${dst}" config user.name "sync-test"
  run_git -C "${dst}" config user.email "sync-test@example.invalid"
}

setup_base_graph() {
  local root=$1
  local original=${root}/original
  local plus=${root}/plus
  local models=${root}/models
  local fork=${root}/fork
  local origin=${root}/origin.git

  new_repo "${original}"
  commit_file "${original}" README.md original-1 "original base"
  run_git -C "${original}" tag v7.1.45

  clone_for_fork "${original}" "${plus}"
  commit_file "${plus}" internal/auth/copilot/provider.go plus-provider "plus provider"
  run_git -C "${plus}" tag v7.1.45-0

  clone_for_fork "${plus}" "${fork}"
  commit_file "${fork}" internal/runtime/executor/fork.go fork-change "fork change"
  run_git -C "${fork}" tag v7.1.45-0.unstableneutron.0

  new_repo "${models}"
  commit_file "${models}" models.json models-1 "models base"

  run_git clone -q --bare "${fork}" "${origin}"
  run_git -C "${fork}" remote set-url origin "${origin}"
  run_git -C "${fork}" remote add original-upstream "${original}"
  run_git -C "${fork}" remote add plus-upstream "${plus}"
  run_git -C "${fork}" remote add models-upstream "${models}"

  commit_file "${original}" README.md original-66 "original v7.1.66"
  run_git -C "${original}" tag v7.1.66

  printf '%s\n' "${fork}"
}

test_plan_emits_exact_snapshot_and_candidate_branch() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local out=${root}/plan.out

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${out}" "${HELPER}" plan >/dev/null
  )

  local key
  for key in base_fork_commit models_commit plan_fingerprint candidate_branch expected_fork_tag; do
    assert_output_key "${out}" "${key}"
  done

  local fingerprint
  fingerprint=$(output_value "${out}" plan_fingerprint)
  for key in original plus-tag plus-head models; do
    run_git -C "${fork}" rev-parse --verify "refs/upstream-sync/${fingerprint}/${key}" >/dev/null
  done

  assert_contains "${out}" "candidate_branch=upstream-sync/original-v7.1.66_plus-v7.1.45-0-"
  rm -rf "${root}"
}

test_materialize_uses_namespaced_refs_without_network() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plan_out=${root}/plan.out
  local materialize_out=${root}/materialize.out

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null
    run_git remote set-url origin "${root}/missing-origin.git"
    run_git remote set-url original-upstream "${root}/missing-original.git"
    run_git remote set-url plus-upstream "${root}/missing-plus.git"
    run_git remote set-url models-upstream "${root}/missing-models.git"
    GITHUB_OUTPUT="${materialize_out}" "${HELPER}" materialize "${plan_out}" >/dev/null
  )

  local original_commit plus_tag_commit candidate_branch
  original_commit=$(output_value "${plan_out}" original_head)
  plus_tag_commit=$(output_value "${plan_out}" plus_tag_head)
  candidate_branch=$(output_value "${plan_out}" candidate_branch)
  run_git -C "${fork}" merge-base --is-ancestor "${original_commit}" HEAD
  run_git -C "${fork}" merge-base --is-ancestor "${plus_tag_commit}" HEAD
  assert_equal "${candidate_branch}" "$(run_git -C "${fork}" branch --show-current)" "materialized branch"
  assert_contains "${materialize_out}" "conflicts=false"
  assert_contains "${fork}/internal/registry/models/models.json" "models-1"
  rm -rf "${root}"
}

test_same_target_produces_same_plan_fingerprint() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local first=${root}/first.out
  local second=${root}/second.out

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${first}" "${HELPER}" plan >/dev/null
    FORCE_REBUILD=false GITHUB_OUTPUT="${second}" "${HELPER}" plan >/dev/null
  )

  assert_equal \
    "$(output_value "${first}" plan_fingerprint)" \
    "$(output_value "${second}" plan_fingerprint)" \
    "stable plan fingerprint"
  rm -rf "${root}"
}

test_moved_target_produces_new_plan_fingerprint() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local models=${root}/models
  local first=${root}/first.out
  local second=${root}/second.out

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${first}" "${HELPER}" plan >/dev/null
  )
  commit_file "${models}" models.json models-2 "move models head"
  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${second}" "${HELPER}" plan >/dev/null
  )

  if [ "$(output_value "${first}" plan_fingerprint)" = "$(output_value "${second}" plan_fingerprint)" ]; then
    fail "moving a selected target did not change the plan fingerprint"
  fi
  rm -rf "${root}"
}

test_validation_driver_modes_tooling_and_artifacts() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plan_out=${root}/plan.out
  local calls=${root}/calls.log

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null

    UPSTREAM_SYNC_INVARIANT_CMD="printf 'invariants\\n' >> '${calls}'" \
      UPSTREAM_SYNC_SYMBOL_CMD="printf 'symbols\\n' >> '${calls}'" \
      UPSTREAM_SYNC_BUILD_CMD="printf 'build\\n' >> '${calls}'" \
      UPSTREAM_SYNC_TEST_CMD="printf 'tests\\n' >> '${calls}'" \
      UPSTREAM_SYNC_HELPER_TEST_CMD="printf 'helper-tests\\n' >> '${calls}'" \
      UPSTREAM_SYNC_SHELLCHECK_CMD="printf 'shellcheck\\n' >> '${calls}'" \
      UPSTREAM_SYNC_ACTIONLINT_CMD="printf 'actionlint\\n' >> '${calls}'" \
      UPSTREAM_SYNC_TOOLING_MODE=auto \
      "${VALIDATOR}" --mode quick --plan "${plan_out}" --report-dir "${root}/quick"
  )

  assert_contains "${calls}" "invariants"
  assert_contains "${calls}" "symbols"
  if grep -Eq '^(build|tests|helper-tests|shellcheck|actionlint)$' "${calls}"; then
    fail "quick validation ran a full or tooling gate"
  fi
  assert_contains "${root}/quick/validation.env" "OVERALL_STATUS=passed"
  assert_contains "${root}/quick/validation.env" "BUILD_STATUS=skipped"
  assert_contains "${root}/quick/validation.json" '"overall_status": "passed"'

  : > "${calls}"
  (
    cd "${fork}"
    UPSTREAM_SYNC_INVARIANT_CMD="printf 'invariants\\n' >> '${calls}'" \
      UPSTREAM_SYNC_SYMBOL_CMD="printf 'symbols\\n' >> '${calls}'" \
      UPSTREAM_SYNC_BUILD_CMD="printf 'build\\n' >> '${calls}'" \
      UPSTREAM_SYNC_TEST_CMD="printf 'tests\\n' >> '${calls}'" \
      UPSTREAM_SYNC_HELPER_TEST_CMD="printf 'helper-tests\\n' >> '${calls}'" \
      UPSTREAM_SYNC_SHELLCHECK_CMD="printf 'shellcheck\\n' >> '${calls}'" \
      UPSTREAM_SYNC_ACTIONLINT_CMD="printf 'actionlint\\n' >> '${calls}'" \
      UPSTREAM_SYNC_TOOLING_MODE=auto \
      "${VALIDATOR}" --mode full --plan "${plan_out}" --report-dir "${root}/full"
  )

  local gate
  for gate in invariants symbols build tests; do
    assert_equal "1" "$(grep -c "^${gate}$" "${calls}")" "${gate} execution count"
  done
  if grep -Eq '^(helper-tests|shellcheck|actionlint)$' "${calls}"; then
    fail "unchanged candidate ran tooling validation"
  fi

  commit_file "${fork}" .github/scripts/tooling-marker.sh marker "change sync tooling"
  : > "${calls}"
  (
    cd "${fork}"
    UPSTREAM_SYNC_INVARIANT_CMD=true \
      UPSTREAM_SYNC_SYMBOL_CMD=true \
      UPSTREAM_SYNC_BUILD_CMD=true \
      UPSTREAM_SYNC_TEST_CMD=true \
      UPSTREAM_SYNC_HELPER_TEST_CMD="printf 'helper-tests\\n' >> '${calls}'" \
      UPSTREAM_SYNC_SHELLCHECK_CMD="printf 'shellcheck\\n' >> '${calls}'" \
      UPSTREAM_SYNC_ACTIONLINT_CMD="printf 'actionlint\\n' >> '${calls}'" \
      UPSTREAM_SYNC_TOOLING_MODE=auto \
      "${VALIDATOR}" --mode full --plan "${plan_out}" --report-dir "${root}/tooling"
  )
  for gate in helper-tests shellcheck actionlint; do
    assert_equal "1" "$(grep -c "^${gate}$" "${calls}")" "${gate} execution count"
  done
  assert_contains "${root}/tooling/validation.env" "TOOLING_REQUIRED=true"

  rm -rf "${root}"
}

test_validation_failure_preserves_all_logs() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plan_out=${root}/plan.out
  local report_dir=${root}/failed

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null
    set +e
    UPSTREAM_SYNC_INVARIANT_CMD=true \
      UPSTREAM_SYNC_SYMBOL_CMD=true \
      UPSTREAM_SYNC_BUILD_CMD="printf 'intentional build failure\\n'; false" \
      UPSTREAM_SYNC_TEST_CMD="printf 'tests still ran\\n'" \
      UPSTREAM_SYNC_TOOLING_MODE=auto \
      "${VALIDATOR}" --mode full --plan "${plan_out}" --report-dir "${report_dir}"
    status=$?
    set -e
    if [ "${status}" -eq 0 ]; then
      fail "validation passed despite a failed build gate"
    fi
  )

  assert_contains "${report_dir}/build.log" "intentional build failure"
  assert_contains "${report_dir}/tests.log" "tests still ran"
  assert_contains "${report_dir}/validation.env" "OVERALL_STATUS=failed"
  assert_contains "${report_dir}/validation.env" "BUILD_STATUS=failed"
  [ -f "${report_dir}/validation.json" ] || fail "failed validation did not write validation.json"
  rm -rf "${root}"
}

test_record_state_writes_schema_v2_before_validation() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plan_out=${root}/plan.out

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null
    GITHUB_OUTPUT="${root}/materialize.out" "${HELPER}" materialize "${plan_out}" >/dev/null
    "${HELPER}" record-state "${plan_out}"
  )

  local state=${fork}/.ccs-fork-upstream.env
  assert_contains "${state}" "SCHEMA_VERSION=2"
  assert_contains "${state}" "SYNC_ID=$(output_value "${plan_out}" safe_sync_id)"
  assert_contains "${state}" "PLAN_FINGERPRINT=$(output_value "${plan_out}" plan_fingerprint)"
  assert_contains "${state}" "BASE_FORK_COMMIT=$(output_value "${plan_out}" base_fork_commit)"
  assert_contains "${state}" "MODELS_REPOSITORY=router-for-me/models"
  assert_contains "${state}" "MODELS_COMMIT=$(output_value "${plan_out}" models_commit)"
  assert_contains "${state}" "EXPECTED_FORK_TAG=$(output_value "${plan_out}" expected_fork_tag)"
  assert_contains "${state}" "CANDIDATE_BRANCH=$(output_value "${plan_out}" candidate_branch)"
  assert_equal \
    "Record upstream sync candidate state" \
    "$(run_git -C "${fork}" log -1 --format=%s)" \
    "candidate state commit subject"
  if [ -n "$(run_git -C "${fork}" status --porcelain)" ]; then
    fail "record-state left the candidate dirty"
  fi
  rm -rf "${root}"
}

assert_freshness_failure() {
  local fork=$1
  local plan_out=$2
  local freshness_out=$3
  local reason=$4

  set +e
  (
    cd "${fork}"
    GITHUB_OUTPUT="${freshness_out}" "${HELPER}" check-freshness "${plan_out}" >/dev/null
  )
  local status=$?
  set -e
  if [ "${status}" -eq 0 ]; then
    fail "freshness passed despite ${reason}"
  fi
  assert_contains "${freshness_out}" "fresh=false"
  assert_contains "${freshness_out}" "stale_reasons=${reason}"
}

test_freshness_passes_for_unchanged_snapshot() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plan_out=${root}/plan.out
  local freshness_out=${root}/freshness.out

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null
    GITHUB_OUTPUT="${freshness_out}" "${HELPER}" check-freshness "${plan_out}" >/dev/null
  )

  assert_contains "${freshness_out}" "fresh=true"
  assert_contains "${freshness_out}" "stale_reasons="
  rm -rf "${root}"
}

test_freshness_rejects_moved_original_tag() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local original=${root}/original
  local plan_out=${root}/plan.out
  local freshness_out=${root}/freshness.out

  (cd "${fork}" && FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null)
  commit_file "${original}" moved-original.txt moved "move original tag target"
  run_git -C "${original}" tag -f v7.1.66 >/dev/null
  assert_freshness_failure "${fork}" "${plan_out}" "${freshness_out}" original-tag-moved
  rm -rf "${root}"
}

test_freshness_rejects_moved_plus_head() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plus=${root}/plus
  local plan_out=${root}/plan.out
  local freshness_out=${root}/freshness.out

  (cd "${fork}" && FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null)
  commit_file "${plus}" internal/auth/copilot/moved.go moved "move Plus head"
  assert_freshness_failure "${fork}" "${plan_out}" "${freshness_out}" plus-head-moved
  rm -rf "${root}"
}

test_freshness_rejects_moved_models_head() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local models=${root}/models
  local plan_out=${root}/plan.out
  local freshness_out=${root}/freshness.out

  (cd "${fork}" && FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null)
  commit_file "${models}" models.json moved-models "move models head"
  assert_freshness_failure "${fork}" "${plan_out}" "${freshness_out}" models-head-moved
  rm -rf "${root}"
}

test_freshness_rejects_changed_origin_main() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plan_out=${root}/plan.out
  local freshness_out=${root}/freshness.out

  (cd "${fork}" && FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null)
  commit_file "${fork}" concurrent.txt concurrent "move fork main"
  run_git -C "${fork}" push -q origin main
  assert_freshness_failure "${fork}" "${plan_out}" "${freshness_out}" fork-main-moved
  rm -rf "${root}"
}

test_freshness_can_ignore_only_fork_base_drift() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plan_out=${root}/plan.out
  local freshness_out=${root}/freshness.out

  (cd "${fork}" && FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null)
  commit_file "${fork}" concurrent.txt concurrent "move fork main"
  run_git -C "${fork}" push -q origin main

  (
    cd "${fork}"
    UPSTREAM_SYNC_ALLOW_FORK_BASE_DRIFT=true \
      GITHUB_OUTPUT="${freshness_out}" \
      "${HELPER}" check-freshness "${plan_out}" >/dev/null
  )

  assert_contains "${freshness_out}" "fresh=true"
  assert_contains "${freshness_out}" "stale_reasons="
  rm -rf "${root}"
}

test_provenance_recommends_owner_specific_actions() {
  local root
  root=$(mktemp -d)
  local original=${root}/original
  local plus=${root}/plus
  local models=${root}/models
  local fork=${root}/fork
  local origin=${root}/origin.git
  local plan_out=${root}/plan.out
  local report_dir=${root}/reports

  new_repo "${original}"
  commit_file "${original}" shared-original.txt base "add original-only base"
  commit_file "${original}" shared-plus.txt base "add Plus-only base"
  commit_file "${original}" shared-both.txt base "add shared base"
  commit_file "${original}" internal/auth/copilot/provider.go base "add Plus-owned base"
  commit_file "${original}" .github/workflows/fork.yml base "add fork-owned base"
  run_git -C "${original}" tag v7.1.45

  clone_for_fork "${original}" "${plus}"
  commit_file "${plus}" shared-plus.txt plus "Plus changes shared path"
  commit_file "${plus}" shared-both.txt plus "Plus changes contested path"
  commit_file "${plus}" internal/auth/copilot/provider.go plus "Plus changes owned path"
  run_git -C "${plus}" tag v7.1.45-0

  clone_for_fork "${plus}" "${fork}"
  commit_file "${fork}" .github/workflows/fork.yml fork "fork changes owned path"
  run_git clone -q --bare "${fork}" "${origin}"
  run_git -C "${fork}" remote set-url origin "${origin}"
  run_git -C "${fork}" remote add original-upstream "${original}"
  run_git -C "${fork}" remote add plus-upstream "${plus}"

  new_repo "${models}"
  commit_file "${models}" models.json models "add models"
  run_git -C "${fork}" remote add models-upstream "${models}"

  commit_file "${original}" shared-original.txt original "original changes shared path"
  commit_file "${original}" shared-both.txt original "original changes contested path"
  run_git -C "${original}" tag v7.1.66

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null
    GITHUB_OUTPUT="${root}/materialize.out" "${HELPER}" materialize "${plan_out}" >/dev/null
    cat "${root}/materialize.out" >> "${plan_out}"
    UPSTREAM_SYNC_REPORT_DIR="${report_dir}" \
      GITHUB_OUTPUT="${root}/provenance.out" \
      "${HELPER}" report-provenance "${plan_out}" HEAD >/dev/null
  )

  local tsv=${report_dir}/provenance.tsv
  local markdown=${report_dir}/provenance.md
  assert_contains "${tsv}" $'shared-original.txt\tshared-hotspot\toriginal\treview-original-update\tfalse'
  assert_contains "${tsv}" $'shared-both.txt\tshared-hotspot\toriginal,plus\tmanual-compose\ttrue'
  assert_not_contains "${tsv}" ".github/workflows/fork.yml"
  assert_not_contains "${tsv}" "shared-plus.txt"
  assert_contains "${markdown}" "Manual composition required: **yes**"
  rm -rf "${root}"
}

test_provenance_ignores_historical_overlap_for_represented_sources() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plan_out=${root}/plan.out
  local materialize_out=${root}/materialize.out
  local report_dir=${root}/reports

  (
    cd "${fork}"
    run_git fetch -q original-upstream refs/tags/v7.1.66:refs/tags/v7.1.66
    run_git merge --no-edit refs/tags/v7.1.66 >/dev/null
    run_git push -q origin main
    FORCE_REBUILD=true GITHUB_OUTPUT="${plan_out}" "${HELPER}" plan >/dev/null
    GITHUB_OUTPUT="${materialize_out}" "${HELPER}" materialize "${plan_out}" >/dev/null
    cat "${materialize_out}" >> "${plan_out}"
    "${HELPER}" record-state "${plan_out}" >/dev/null
    UPSTREAM_SYNC_REPORT_DIR="${report_dir}" \
      GITHUB_OUTPUT="${root}/provenance.out" \
      "${HELPER}" report-provenance "${plan_out}" HEAD >/dev/null
  )

  assert_contains "${root}/provenance.out" "manual_composition_required=false"
  if awk -F'\t' 'NR > 1 && $5 == "true" { found = 1 } END { exit !found }' "${report_dir}/provenance.tsv"; then
    fail "represented source history produced a manual-composition row"
  fi
  rm -rf "${root}"
}

test_shared_conflict_aborts_without_side_checkout() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local original=${root}/original
  local out=${root}/merge.out
  local log=${root}/merge.log
  local before

  commit_file "${original}" internal/runtime/executor/fork.go original-change "original conflicts with fork overlay"
  run_git -C "${original}" tag v7.1.67
  before=$(run_git -C "${fork}" rev-parse HEAD)

  (
    cd "${fork}"
    run_git fetch -q original-upstream refs/tags/v7.1.67:refs/tags/v7.1.67
    GITHUB_OUTPUT="${out}" "${HELPER}" merge-ref original refs/tags/v7.1.67 > "${log}" 2>&1
  )

  assert_contains "${out}" "conflicts=true"
  assert_contains "${log}" "requires manual composition"
  assert_equal "${before}" "$(run_git -C "${fork}" rev-parse HEAD)" "shared conflict branch head"
  assert_equal "fork-change" "$(cat "${fork}/internal/runtime/executor/fork.go")" "shared conflict worktree content"
  if [ -n "$(run_git -C "${fork}" status --porcelain)" ]; then
    fail "shared conflict left an unresolved worktree"
  fi
  rm -rf "${root}"
}

test_report_renderer_includes_required_evidence() {
  local root
  root=$(mktemp -d)
  local plan=${root}/plan.out
  local validation=${root}/validation.env
  local provenance=${root}/provenance.tsv
  local report=${root}/report.md

  cat > "${plan}" <<'EOF'
safe_sync_id=original-v7.2.67_plus-v7.2.62-5
base_fork_commit=1111111111111111111111111111111111111111
original_tag=v7.2.67
original_head=2222222222222222222222222222222222222222
plus_tag=v7.2.62-5
plus_tag_head=3333333333333333333333333333333333333333
plus_head=4444444444444444444444444444444444444444
plus_head_included=true
models_commit=5555555555555555555555555555555555555555
plan_fingerprint=6666666666666666666666666666666666666666
candidate_branch=upstream-sync/original-v7.2.67_plus-v7.2.62-5-666666666666
expected_fork_tag=v7.2.67-unstableneutron.1
fresh=false
stale_reasons=models-head-moved
conflicts=true
conflict_files=sdk/shared.go
workflow_url=https://github.com/unstableneutron/CLIProxyAPIPlus/actions/runs/123
EOF
  cat > "${validation}" <<'EOF'
OVERALL_STATUS=failed
INVARIANTS_STATUS=passed
SYMBOL_SURVIVAL_STATUS=passed
BUILD_STATUS=failed
TESTS_STATUS=passed
HELPER_TESTS_STATUS=passed
SHELLCHECK_STATUS=passed
ACTIONLINT_STATUS=passed
EOF
  cat > "${provenance}" <<'EOF'
path	owner	provenance	action	manual_composition
sdk/shared.go	shared-hotspot	original,plus	manual-compose	true
sdk/fork.go	fork-owned	fork	preserve-fork	false
EOF

  "${RENDERER}" --plan "${plan}" --validation "${validation}" --provenance "${provenance}" --output "${report}"

  assert_contains "${report}" "v7.2.67"
  assert_contains "${report}" "2222222222222222222222222222222222222222"
  assert_contains "${report}" "3333333333333333333333333333333333333333"
  assert_contains "${report}" "4444444444444444444444444444444444444444"
  assert_contains "${report}" "5555555555555555555555555555555555555555"
  assert_contains "${report}" "6666666666666666666666666666666666666666"
  assert_contains "${report}" "upstream-sync/original-v7.2.67_plus-v7.2.62-5-666666666666"
  assert_contains "${report}" "v7.2.67-unstableneutron.1"
  assert_contains "${report}" "Fresh: **false**"
  assert_contains "${report}" "models-head-moved"
  assert_contains "${report}" "sdk/shared.go"
  assert_contains "${report}" "shared-hotspot"
  assert_contains "${report}" "manual-compose"
  assert_contains "${report}" "Manual composition required: **yes**"
  # shellcheck disable=SC2016
  assert_contains "${report}" 'Build | `failed`'
  assert_contains "${report}" "https://github.com/unstableneutron/CLIProxyAPIPlus/actions/runs/123"
  rm -rf "${root}"
}

test_report_renderer_handles_no_optional_conflicts() {
  local root
  root=$(mktemp -d)
  local plan=${root}/plan.out
  local validation=${root}/validation.env
  local provenance=${root}/provenance.tsv
  local report=${root}/report.md

  cat > "${plan}" <<'EOF'
safe_sync_id=sync-id
base_fork_commit=1111111111111111111111111111111111111111
original_tag=v7.2.67
original_head=2222222222222222222222222222222222222222
plus_tag=v7.2.62-5
plus_tag_head=3333333333333333333333333333333333333333
plus_head=3333333333333333333333333333333333333333
plus_head_included=false
models_commit=5555555555555555555555555555555555555555
plan_fingerprint=6666666666666666666666666666666666666666
candidate_branch=upstream-sync/sync-id-666666666666
expected_fork_tag=v7.2.67-unstableneutron.0
EOF
  cat > "${validation}" <<'EOF'
OVERALL_STATUS=passed
INVARIANTS_STATUS=passed
SYMBOL_SURVIVAL_STATUS=passed
BUILD_STATUS=passed
TESTS_STATUS=passed
HELPER_TESTS_STATUS=skipped
SHELLCHECK_STATUS=skipped
ACTIONLINT_STATUS=skipped
EOF
  printf 'path\towner\tprovenance\taction\tmanual_composition\n' > "${provenance}"

  "${RENDERER}" --plan "${plan}" --validation "${validation}" --provenance "${provenance}" --output "${report}"
  assert_contains "${report}" "Conflicts: **None**"
  assert_contains "${report}" "Manual composition required: **no**"
  assert_contains "${report}" "Workflow: None"
  rm -rf "${root}"
}

test_report_renderer_rejects_missing_required_plan_field() {
  local root
  root=$(mktemp -d)
  local plan=${root}/plan.out
  local validation=${root}/validation.env
  local provenance=${root}/provenance.tsv

  printf 'original_tag=v7.2.67\n' > "${plan}"
  printf 'OVERALL_STATUS=passed\n' > "${validation}"
  printf 'path\towner\tprovenance\taction\tmanual_composition\n' > "${provenance}"
  if "${RENDERER}" --plan "${plan}" --validation "${validation}" --provenance "${provenance}" --output "${root}/report.md" 2>/dev/null; then
    fail "renderer accepted a plan missing required snapshot fields"
  fi
  rm -rf "${root}"
}

test_v2_workflow_contract_is_candidate_first_and_manual_only() {
  local workflow=${SCRIPT_DIR}/../workflows/upstream-sync-v2.yml

  assert_contains "${workflow}" "workflow_dispatch:"
  assert_contains "${workflow}" "options: [shadow, promote]"
  assert_contains "${workflow}" "base_ref:"
  assert_contains "${workflow}" "force_candidate:"
  assert_contains "${workflow}" "materialize"
  assert_contains "${workflow}" "record-state"
  assert_contains "${workflow}" "check-freshness"
  assert_contains "${workflow}" "validate-upstream-sync.sh --mode full"
  assert_contains "${workflow}" "--force-with-lease"
  assert_contains "${workflow}" "gh pr create"
  assert_contains "${workflow}" "git push origin HEAD:main"
  # shellcheck disable=SC2016 # The workflow expression is asserted literally.
  assert_contains "${workflow}" 'git push origin "refs/tags/${TAG}"'
  assert_contains "${workflow}" "verify-upstream-release.sh"
  assert_contains "${workflow}" "gh release download"
  assert_contains "${workflow}" "gh release upload"
  assert_contains "${workflow}" "upstream-sync-receipt.json"
  assert_equal \
    "1" \
    "$(grep -c 'validate-upstream-sync.sh --mode full' "${workflow}")" \
    "full validation invocation count"
  assert_not_contains "${workflow}" "schedule:"
  assert_not_contains "${workflow}" "gh issue"
  assert_not_contains "${workflow}" "force_pr"
  assert_not_contains "${workflow}" "upstream-sync/pending-overlay"
}

test_publication_workflows_are_reusable_and_checked() {
  local release=${SCRIPT_DIR}/../workflows/release.yaml
  local docker=${SCRIPT_DIR}/../workflows/docker-image.yml
  local recovery=${SCRIPT_DIR}/../workflows/sync-release-tag.yml
  local dockerfile=${SCRIPT_DIR}/../../Dockerfile

  assert_contains "${VALIDATOR}" "test-verify-upstream-release.sh"
  assert_contains "${VALIDATOR}" "UPSTREAM_SYNC_TOOLING_MODE=auto"

  assert_contains "${release}" "workflow_call:"
  assert_contains "${release}" "expected_commit:"
  assert_contains "${release}" "release_url:"
  assert_contains "${release}" "asset_names_json:"
  assert_contains "${release}" "release_commit:"
  assert_contains "${release}" "goreleaser/goreleaser-action@f06c13b6b1a9625abc9e6e439d9c05a8f2190e94"
  assert_contains "${release}" "version: v2.17.0"
  assert_not_contains "${release}" "version: latest"

  assert_contains "${docker}" "workflow_call:"
  assert_contains "${docker}" "publish_latest:"
  assert_contains "${docker}" "runner: ubuntu-24.04-arm"
  assert_contains "${docker}" "platform: linux/arm64"
  assert_contains "${docker}" "runner: ubuntu-24.04"
  assert_contains "${docker}" "platform: linux/amd64"
  # shellcheck disable=SC2016 # GitHub expressions are asserted literally.
  assert_contains "${docker}" 'cache-from: type=gha,scope=cliproxy-${{ matrix.arch }}'
  # shellcheck disable=SC2016 # GitHub expressions are asserted literally.
  assert_contains "${docker}" 'cache-to: type=gha,mode=max,scope=cliproxy-${{ matrix.arch }}'
  assert_contains "${docker}" "push-by-digest=true"
  assert_not_contains "${docker}" "setup-qemu-action"
  assert_not_contains "${docker}" "Refresh models catalog"

  assert_contains "${recovery}" "uses: ./.github/workflows/release.yaml"
  assert_contains "${recovery}" "uses: ./.github/workflows/docker-image.yml"
  assert_contains "${recovery}" "verify-upstream-release.sh"
  assert_contains "${recovery}" "gh release upload"
  assert_contains "${recovery}" "upstream-sync-receipt.json"
  assert_not_contains "${recovery}" "gh workflow run"

  assert_contains "${dockerfile}" "# syntax=docker/dockerfile:1"
  assert_contains "${dockerfile}" "golang:1.26-bookworm@sha256:18aedc16aa19b3fd7ded7245fc14b109e054d65d22ed53c355c899582bbb2113"
  assert_contains "${dockerfile}" "debian:bookworm@sha256:30482e873082e906a4908c10529180aefb6f77620aea7404b909829fadc5d168"
  assert_contains "${dockerfile}" "--mount=type=cache,target=/go/pkg/mod"
  assert_contains "${dockerfile}" "--mount=type=cache,target=/root/.cache/go-build"
}

test_detects_original_ahead_of_plus() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local out=${root}/plan.out

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${out}" "${HELPER}" plan >/dev/null
  )

  assert_contains "${out}" "has_changes=true"
  assert_contains "${out}" "blocked=false"
  assert_contains "${out}" "original_tag=v7.1.66"
  assert_contains "${out}" "plus_tag=v7.1.45-0"
  assert_contains "${out}" "fork_tag_prefix=v7.1.66-unstableneutron"
  assert_contains "${out}" "next_fork_tag=v7.1.66-unstableneutron.0"
}

test_noops_when_latest_fork_tag_represents_both_sources() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local out=${root}/plan.out

  (
    cd "${fork}"
    run_git fetch -q original-upstream main --tags
    run_git merge --no-edit refs/tags/v7.1.66 >/dev/null
    run_git tag v7.1.66-unstableneutron.0
    run_git push -q origin main --tags
    FORCE_REBUILD=false GITHUB_OUTPUT="${out}" "${HELPER}" plan >/dev/null
  )

  assert_contains "${out}" "has_changes=false"
  assert_contains "${out}" "latest_fork_tag=v7.1.66-unstableneutron.0"
}

test_includes_safe_plus_head_delta() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plus=${root}/plus
  local out=${root}/plan.out

  commit_file "${plus}" internal/auth/copilot/hotfix.go plus-hotfix "plus hotfix"

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${out}" "${HELPER}" plan >/dev/null
  )

  assert_contains "${out}" "has_changes=true"
  assert_contains "${out}" "blocked=false"
  assert_contains "${out}" "plus_head_included=true"
  assert_contains "${out}" "plus_head_delta_paths=internal/auth/copilot/hotfix.go"
}

test_blocks_unsafe_plus_head_delta() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plus=${root}/plus
  local out=${root}/plan.out

  commit_file "${plus}" internal/runtime/executor/shared.go plus-shared-hotfix "plus shared hotfix"

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${out}" "${HELPER}" plan >/dev/null
  )

  assert_contains "${out}" "has_changes=true"
  assert_contains "${out}" "blocked=true"
  assert_contains "${out}" "block_reason=plus-head-delta-touches-shared-paths"
  assert_contains "${out}" "plus_head_included=false"
  assert_contains "${out}" "unsafe_plus_head_delta_paths=internal/runtime/executor/shared.go"
}

test_original_merge_protects_plus_owned_paths() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local original=${root}/original
  local out=${root}/merge.out

  commit_file "${fork}" .github/workflows/release.yaml fork-workflow "fork workflow"
  commit_file "${original}" internal/auth/copilot/provider.go original-clobber "original clobber"
  commit_file "${original}" .github/workflows/release.yaml original-workflow "original workflow"
  run_git -C "${original}" tag v7.1.67

  (
    cd "${fork}"
    run_git fetch -q original-upstream refs/tags/v7.1.67:refs/tags/v7.1.67
    GITHUB_OUTPUT="${out}" "${HELPER}" merge-ref original refs/tags/v7.1.67 >/dev/null
  )

  assert_contains "${out}" "conflicts=true"
  # shellcheck disable=SC2016
  assert_contains "${out}" '| `.github/workflows/release.yaml` | `fork-owned` |'
  if ! grep -Fq plus-provider "${fork}/internal/auth/copilot/provider.go"; then
    fail "original merge overwrote Plus-owned provider file"
  fi
  if ! grep -Fq fork-workflow "${fork}/.github/workflows/release.yaml"; then
    fail "original merge overwrote fork-owned workflow file"
  fi
}

test_original_merge_supports_linked_worktree() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local linked=${root}/fork-worktree
  local out=${root}/worktree-merge.out

  run_git -C "${fork}" worktree add -q -b linked-worktree "${linked}" HEAD

  (
    cd "${linked}"
    run_git fetch -q original-upstream refs/tags/v7.1.66:refs/tags/v7.1.66
    GITHUB_OUTPUT="${out}" "${HELPER}" merge-ref original refs/tags/v7.1.66 >/dev/null
  )

  assert_contains "${out}" "conflicts=false"
}

test_plus_merge_can_update_plus_owned_paths() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local plus=${root}/plus
  local out=${root}/merge.out

  commit_file "${plus}" internal/auth/copilot/provider.go plus-update "plus provider update"

  (
    cd "${fork}"
    run_git fetch -q plus-upstream refs/heads/main:refs/remotes/plus-upstream/main
    GITHUB_OUTPUT="${out}" "${HELPER}" merge-ref plus-head refs/remotes/plus-upstream/main >/dev/null
  )

  assert_contains "${out}" "conflicts=false"
  if ! grep -Fq plus-update "${fork}/internal/auth/copilot/provider.go"; then
    fail "Plus merge did not update Plus-owned provider file"
  fi
}

test_pending_overlay_branch_name_is_stable() {
  local branch
  branch=$("${HELPER}" pending-overlay-branch)

  if [ "${branch}" != "upstream-sync/pending-overlay" ]; then
    fail "expected stable pending overlay branch, got: ${branch}"
  fi
}

test_manifest_classifies_fork_surfaces() {
  local root
  root=$(mktemp -d)
  local out=${root}/classify.out

  printf '%s\n' \
    .github/scripts/upstream-sync.sh \
    .github/scripts/test-upstream-sync.sh \
    .github/upstream-sync-ownership.tsv \
    .github/upstream-sync-invariants.tsv \
    internal/runtime/executor/gemini_cli_executor.go \
    sdk/api/handlers/gemini/gemini-cli_handlers.go \
    sdk/auth/gemini.go \
    | "${HELPER}" classify-paths > "${out}"

  # shellcheck disable=SC2016
  assert_contains "${out}" '| `.github/scripts/upstream-sync.sh` | `fork-owned` |'
  # shellcheck disable=SC2016
  assert_contains "${out}" '| `.github/scripts/test-upstream-sync.sh` | `fork-owned` |'
  # shellcheck disable=SC2016
  assert_contains "${out}" '| `.github/upstream-sync-ownership.tsv` | `fork-owned` |'
  # shellcheck disable=SC2016
  assert_contains "${out}" '| `.github/upstream-sync-invariants.tsv` | `fork-owned` |'
  # shellcheck disable=SC2016
  assert_contains "${out}" '| `internal/runtime/executor/gemini_cli_executor.go` | `fork-owned` |'
  # shellcheck disable=SC2016
  assert_contains "${out}" '| `sdk/api/handlers/gemini/gemini-cli_handlers.go` | `fork-owned` |'
  # shellcheck disable=SC2016
  assert_contains "${out}" '| `sdk/auth/gemini.go` | `fork-owned` |'
}

test_original_merge_reports_owned_clobber_without_text_conflict() {
  local root
  root=$(mktemp -d)
  local original=${root}/original
  local plus=${root}/plus
  local models=${root}/models
  local fork=${root}/fork
  local out=${root}/merge.out

  new_repo "${original}"
  commit_file "${original}" docker-compose.yml $'fork_setting=base\nupstream_setting=base' "original base"
  run_git -C "${original}" tag v7.1.45

  clone_for_fork "${original}" "${plus}"
  clone_for_fork "${plus}" "${fork}"
  run_git -C "${fork}" remote add original-upstream "${original}"

  commit_file "${fork}" docker-compose.yml $'fork_setting=fork\nupstream_setting=base' "fork setting"
  commit_file "${original}" docker-compose.yml $'fork_setting=base\nupstream_setting=original' "original setting"
  run_git -C "${original}" tag v7.1.67

  (
    cd "${fork}"
    run_git fetch -q original-upstream refs/tags/v7.1.67:refs/tags/v7.1.67
    GITHUB_OUTPUT="${out}" "${HELPER}" merge-ref original refs/tags/v7.1.67 >/dev/null
  )

  assert_contains "${out}" "conflicts=true"
  assert_contains "${out}" "ownership_clobber_files=docker-compose.yml"
  if [ "$(cat "${fork}/docker-compose.yml")" != $'fork_setting=fork\nupstream_setting=base' ]; then
    cat "${fork}/docker-compose.yml" >&2
    fail "owned clobber resolution did not preserve fork side"
  fi
}

test_original_merge_skips_identical_owned_path_touch() {
  local root
  root=$(mktemp -d)
  local original=${root}/original
  local plus=${root}/plus
  local fork=${root}/fork
  local out=${root}/merge.out

  new_repo "${original}"
  commit_file "${original}" docker-compose.yml "setting=base" "original base"
  run_git -C "${original}" tag v7.1.45

  clone_for_fork "${original}" "${plus}"
  clone_for_fork "${plus}" "${fork}"
  run_git -C "${fork}" remote add original-upstream "${original}"

  commit_file "${fork}" docker-compose.yml "setting=fork" "fork setting"
  commit_file "${original}" docker-compose.yml "setting=fork" "original same setting"
  run_git -C "${original}" tag v7.1.67

  (
    cd "${fork}"
    run_git fetch -q original-upstream refs/tags/v7.1.67:refs/tags/v7.1.67
    GITHUB_OUTPUT="${out}" "${HELPER}" merge-ref original refs/tags/v7.1.67 >/dev/null
  )

  assert_contains "${out}" "conflicts=false"
}

test_check_invariants_detects_missing_pattern() {
  local root
  root=$(mktemp -d)
  local out=${root}/invariants.out

  new_repo "${root}/repo"
  mkdir -p "${root}/repo/.github"
  printf '%s\n' \
    $'contains\timportant.go\tmust_keep_symbol\timportant symbol remains' \
    > "${root}/repo/.github/upstream-sync-invariants.tsv"
  commit_file "${root}/repo" important.go "package main" "add important file"

  set +e
  (
    cd "${root}/repo"
    "${HELPER}" check-invariants
  ) > "${out}" 2>&1
  local exit_code=$?
  set -e

  if [ ${exit_code} -eq 0 ]; then
    fail "check-invariants passed despite missing required pattern"
  fi
  assert_contains "${out}" "important symbol remains"

  commit_file "${root}/repo" important.go $'package main\nconst must_keep_symbol = true' "restore important symbol"
  (
    cd "${root}/repo"
    "${HELPER}" check-invariants
  ) > "${out}" 2>&1
}

test_plan_reports_target_drift_from_recorded_state() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local out=${root}/plan.out

  cat > "${fork}/.ccs-fork-upstream.env" <<'EOF'
ORIGINAL_TAG=v7.1.45
ORIGINAL_COMMIT=old-original
PLUS_TAG=v7.1.45-0
PLUS_TAG_COMMIT=old-plus-tag
PLUS_HEAD_COMMIT=old-plus-head
PLUS_HEAD_INCLUDED=false
EOF
  run_git -C "${fork}" add .ccs-fork-upstream.env
  run_git -C "${fork}" commit -m "record old upstream state" >/dev/null

  (
    cd "${fork}"
    FORCE_REBUILD=false GITHUB_OUTPUT="${out}" "${HELPER}" plan >/dev/null
  )

  assert_contains "${out}" "target_drift=true"
  # shellcheck disable=SC2016
  assert_contains "${out}" 'Original tag: `v7.1.45` -> `v7.1.66`'
  # shellcheck disable=SC2016
  assert_contains "${out}" 'Original commit: `old-original` -> `'
  assert_contains "${out}" $'\nPlus tag: `v7.1.45-0` -> `v7.1.66-0`'
  # shellcheck disable=SC2016
  assert_contains "${out}" 'Plus tag commit: `old-plus-tag` -> `'
}

test_replay_plan_reports_all_phase_conflicts_and_gates() {
  local root
  root=$(mktemp -d)
  local original=${root}/original
  local plus=${root}/plus
  local models=${root}/models
  local fork=${root}/fork
  local origin=${root}/origin.git
  local out=${root}/replay.out

  new_repo "${original}"
  commit_file "${original}" README.md base "original base"
  commit_file "${original}" original-conflict.txt base "original conflict base"
  commit_file "${original}" plus-conflict.txt base "plus conflict base"
  run_git -C "${original}" tag v7.1.45

  clone_for_fork "${original}" "${plus}"
  run_git -C "${plus}" tag v7.1.45-0

  new_repo "${models}"
  commit_file "${models}" models.json models-1 "models base"

  clone_for_fork "${plus}" "${fork}"
  mkdir -p "${fork}/.github"
  printf '\n' > "${fork}/.github/upstream-sync-invariants.tsv"
  commit_file "${fork}" original-conflict.txt fork-original "fork original conflict"
  commit_file "${fork}" plus-conflict.txt fork-plus "fork plus conflict"
  commit_file "${fork}" .github/upstream-sync-invariants.tsv "" "add empty invariants"
  run_git -C "${fork}" tag v7.1.45-unstableneutron.0

  run_git clone -q --bare "${fork}" "${origin}"
  run_git -C "${fork}" remote set-url origin "${origin}"
  run_git -C "${fork}" remote add original-upstream "${original}"
  run_git -C "${fork}" remote add plus-upstream "${plus}"
  run_git -C "${fork}" remote add models-upstream "${models}"

  commit_file "${original}" original-conflict.txt original-new "original new conflict"
  run_git -C "${original}" tag v7.1.66
  commit_file "${plus}" plus-conflict.txt plus-new "plus new conflict"
  run_git -C "${plus}" tag v7.1.66-0

  (
    cd "${fork}"
    UPSTREAM_SYNC_REPLAY_BUILD_CMD=true \
      UPSTREAM_SYNC_REPLAY_TEST_CMD=true \
      "${HELPER}" replay-plan > "${out}" 2>&1
  )

  assert_contains "${out}" "Original tag: v7.1.66"
  assert_contains "${out}" "Plus tag: v7.1.66-0"
  assert_contains "${out}" "original-conflict.txt"
  assert_contains "${out}" "plus-conflict.txt"
  assert_contains "${out}" "Original merge: conflicts=true"
  assert_contains "${out}" "Plus release overlay: conflicts=true"
  assert_contains "${out}" "Invariant status: passed"
  assert_contains "${out}" "Build status: passed"
  assert_contains "${out}" "Test status: passed"

  if ! run_git -C "${fork}" diff --quiet; then
    run_git -C "${fork}" status --short >&2
    fail "replay-plan mutated the source checkout"
  fi
}

test_replay_plan_fails_when_gate_fails() {
  local root
  root=$(mktemp -d)
  local fork
  fork=$(setup_base_graph "${root}")
  local out=${root}/replay-fail.out

  (
    cd "${fork}"
    set +e
    UPSTREAM_SYNC_REPLAY_BUILD_CMD=false \
      UPSTREAM_SYNC_REPLAY_TEST_CMD=true \
      "${HELPER}" replay-plan > "${out}" 2>&1
    status=$?
    set -e
    if [ "${status}" -eq 0 ]; then
      fail "expected replay-plan to fail when build gate fails"
    fi
  )

  assert_contains "${out}" "Build status: failed"
}

test_original_merge_writes_overlay_at_risk_report() {
  local root=${TMPDIR:-/tmp}/upstream-sync-test-risk-$$
  rm -rf "${root}"
  setup_base_graph "${root}"

  local original=${root}/original
  local fork=${root}/fork
  local out=${root}/merge.out
  local report_dir=${root}/reports

  commit_file "${original}" internal/runtime/executor/fork.go $'package executor\nfunc UpstreamFork() {}' "original adds shared fork path"
  run_git -C "${original}" tag v7.1.67
  run_git -C "${fork}" fetch -q original-upstream main --tags

  (
    cd "${fork}"
    UPSTREAM_SYNC_REPORT_DIR="${report_dir}" GITHUB_OUTPUT="${out}" "${HELPER}" merge-ref original refs/tags/v7.1.67 >/dev/null
  )

  assert_contains "${out}" "conflicts=true"
  assert_contains "${out}" "overlay_at_risk_report=${report_dir}/overlay-at-risk-original.diff"
  assert_contains "${out}" "| \`internal/runtime/executor/fork.go\` | \`1\` |"
  assert_contains "${report_dir}/overlay-at-risk-original.diff" "## internal/runtime/executor/fork.go"
  assert_contains "${report_dir}/overlay-at-risk-original.diff" "fork-change"
}

test_check_symbol_survival_detects_deleted_overlay_symbols() {
  local root=${TMPDIR:-/tmp}/upstream-sync-test-symbols-$$
  rm -rf "${root}"
  mkdir -p "${root}"

  local original=${root}/original
  local fork=${root}/fork
  local out=${root}/symbols.out
  new_repo "${original}"
  commit_file "${original}" internal/runtime/executor/shared.go $'package executor\nfunc UpstreamOnly() {}' "original shared symbol"
  clone_for_fork "${original}" "${fork}"

  commit_file "${fork}" internal/runtime/executor/shared.go $'package executor\nfunc UpstreamOnly() {}\nfunc ForkOnly() {}' "fork shared symbol"
  commit_file "${fork}" internal/runtime/executor/shared_test.go $'package executor\nimport "testing"\nfunc TestForkOnly(t *testing.T) {}' "fork shared test"
  local baseline upstream_ref
  baseline=$(run_git -C "${fork}" rev-parse HEAD)
  upstream_ref=$(run_git -C "${original}" rev-parse HEAD)

  (cd "${fork}" && "${HELPER}" check-symbol-survival "${baseline}" "${upstream_ref}") > "${out}" 2>&1
  assert_contains "${out}" "[OK] symbol-survival gate passed."

  printf '%s\n' 'package executor' 'func UpstreamOnly() {}' > "${fork}/internal/runtime/executor/shared.go"
  rm -f "${fork}/internal/runtime/executor/shared_test.go"

  set +e
  (cd "${fork}" && "${HELPER}" check-symbol-survival "${baseline}" "${upstream_ref}") > "${out}" 2>&1
  local exit_code=$?
  set -e
  if [ ${exit_code} -eq 0 ]; then
    fail "check-symbol-survival passed despite deleted fork-only symbols"
  fi
  assert_contains "${out}" "[FAIL] missing overlay symbol: ForkOnly"
  assert_contains "${out}" "DELETED FORK TESTS"
  assert_contains "${out}" "[FAIL] deleted fork test: TestForkOnly"

  mkdir -p "${fork}/.github"
  printf '%s\n' '# symbol	reason' $'ForkOnly\tintentionally superseded in test' $'TestForkOnly\tintentionally superseded in test' > "${fork}/.github/upstream-sync-dropped-symbols.tsv"
  (cd "${fork}" && "${HELPER}" check-symbol-survival "${baseline}" "${upstream_ref}") > "${out}" 2>&1
  assert_contains "${out}" "[SKIP] dropped overlay symbol allowlisted: ForkOnly"
  assert_contains "${out}" "[SKIP] dropped overlay symbol allowlisted: TestForkOnly"
  assert_contains "${out}" "[OK] symbol-survival gate passed with allowlisted removals."
}

main() {
  test_plan_emits_exact_snapshot_and_candidate_branch
  test_materialize_uses_namespaced_refs_without_network
  test_same_target_produces_same_plan_fingerprint
  test_moved_target_produces_new_plan_fingerprint
  test_validation_driver_modes_tooling_and_artifacts
  test_validation_failure_preserves_all_logs
  test_record_state_writes_schema_v2_before_validation
  test_freshness_passes_for_unchanged_snapshot
  test_freshness_rejects_moved_original_tag
  test_freshness_rejects_moved_plus_head
  test_freshness_rejects_moved_models_head
  test_freshness_rejects_changed_origin_main
  test_freshness_can_ignore_only_fork_base_drift
  test_provenance_recommends_owner_specific_actions
  test_provenance_ignores_historical_overlap_for_represented_sources
  test_shared_conflict_aborts_without_side_checkout
  test_report_renderer_includes_required_evidence
  test_report_renderer_handles_no_optional_conflicts
  test_report_renderer_rejects_missing_required_plan_field
  test_v2_workflow_contract_is_candidate_first_and_manual_only
  test_publication_workflows_are_reusable_and_checked
  test_detects_original_ahead_of_plus
  test_noops_when_latest_fork_tag_represents_both_sources
  test_includes_safe_plus_head_delta
  test_blocks_unsafe_plus_head_delta
  test_original_merge_protects_plus_owned_paths
  test_original_merge_supports_linked_worktree
  test_plus_merge_can_update_plus_owned_paths
  test_pending_overlay_branch_name_is_stable
  test_manifest_classifies_fork_surfaces
  test_original_merge_reports_owned_clobber_without_text_conflict
  test_original_merge_writes_overlay_at_risk_report
  test_original_merge_skips_identical_owned_path_touch
  test_check_invariants_detects_missing_pattern
  test_check_symbol_survival_detects_deleted_overlay_symbols
  test_plan_reports_target_drift_from_recorded_state
  test_replay_plan_reports_all_phase_conflicts_and_gates
  test_replay_plan_fails_when_gate_fails
  echo "[OK] upstream-sync helper tests passed"
}

main "$@"
