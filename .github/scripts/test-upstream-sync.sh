#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
HELPER="${SCRIPT_DIR}/upstream-sync.sh"
export UPSTREAM_SYNC_OWNERSHIP_FILE="${UPSTREAM_SYNC_OWNERSHIP_FILE:-${SCRIPT_DIR}/../upstream-sync-ownership.tsv}"

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

  run_git clone -q --bare "${fork}" "${origin}"
  run_git -C "${fork}" remote set-url origin "${origin}"
  run_git -C "${fork}" remote add original-upstream "${original}"
  run_git -C "${fork}" remote add plus-upstream "${plus}"

  commit_file "${original}" README.md original-66 "original v7.1.66"
  run_git -C "${original}" tag v7.1.66

  printf '%s\n' "${fork}"
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

main() {
  test_detects_original_ahead_of_plus
  test_noops_when_latest_fork_tag_represents_both_sources
  test_includes_safe_plus_head_delta
  test_blocks_unsafe_plus_head_delta
  test_original_merge_protects_plus_owned_paths
  test_plus_merge_can_update_plus_owned_paths
  test_pending_overlay_branch_name_is_stable
  test_manifest_classifies_fork_surfaces
  test_original_merge_reports_owned_clobber_without_text_conflict
  test_original_merge_skips_identical_owned_path_touch
  test_check_invariants_detects_missing_pattern
  test_plan_reports_target_drift_from_recorded_state
  test_replay_plan_reports_all_phase_conflicts_and_gates
  test_replay_plan_fails_when_gate_fails
  echo "[OK] upstream-sync helper tests passed"
}

main "$@"
