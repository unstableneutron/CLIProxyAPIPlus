#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
HELPER="${SCRIPT_DIR}/upstream-sync.sh"

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

  commit_file "${original}" internal/auth/copilot/provider.go original-clobber "original clobber"
  run_git -C "${original}" tag v7.1.67

  (
    cd "${fork}"
    run_git fetch -q original-upstream refs/tags/v7.1.67:refs/tags/v7.1.67
    GITHUB_OUTPUT="${out}" "${HELPER}" merge-ref original refs/tags/v7.1.67 >/dev/null
  )

  assert_contains "${out}" "conflicts=false"
  if ! grep -Fq plus-provider "${fork}/internal/auth/copilot/provider.go"; then
    fail "original merge overwrote Plus-owned provider file"
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

main() {
  test_detects_original_ahead_of_plus
  test_noops_when_latest_fork_tag_represents_both_sources
  test_includes_safe_plus_head_delta
  test_blocks_unsafe_plus_head_delta
  test_original_merge_protects_plus_owned_paths
  test_plus_merge_can_update_plus_owned_paths
  echo "[OK] upstream-sync helper tests passed"
}

main "$@"
