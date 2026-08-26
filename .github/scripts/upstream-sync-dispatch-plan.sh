#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/portable-tools.sh
source "${SCRIPT_DIR}/portable-tools.sh"
# shellcheck source=.github/scripts/hotfix-release-tag.sh
source "${SCRIPT_DIR}/hotfix-release-tag.sh"

REPOSITORY=${GITHUB_REPOSITORY:-unstableneutron/CLIProxyAPIPlus}


die() {
  echo "[upstream-sync-dispatch-plan] $*" >&2
  exit 1
}

state_value() {
  local file=$1 key=$2
  awk -F= -v key="${key}" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "${file}"
}

require_file() {
  local path=$1
  [ -f "${path}" ] || die "file not found: ${path}"
}

require_sha() {
  local name=$1 value=$2
  [[ "${value}" =~ ^[0-9a-f]{40}$ ]] || die "${name} must be a lowercase 40-character SHA"
}

require_positive_integer() {
  local name=$1 value=$2
  [[ "${value}" =~ ^[1-9][0-9]*$ ]] || die "${name} must be a positive decimal integer"
}

print_command() {
  local argument
  printf 'gh'
  for argument in "$@"; do
    printf ' %q' "${argument}"
  done
  printf '\n'
}

cmd_repair() {
  local plan_file="" repair_sha="" repair_pr=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --plan) plan_file=${2:-}; shift 2 ;;
      --repair-sha) repair_sha=${2:-}; shift 2 ;;
      --repair-pr) repair_pr=${2:-}; shift 2 ;;
      --repository) REPOSITORY=${2:-}; shift 2 ;;
      *) die "unknown repair argument: $1" ;;
    esac
  done
  require_file "${plan_file}"
  require_sha repair-sha "${repair_sha}"
  require_positive_integer repair-pr "${repair_pr}"
  local branch fingerprint
  branch=$(state_value "${plan_file}" candidate_branch)
  fingerprint=$(state_value "${plan_file}" plan_fingerprint)
  [ -n "${branch}" ] || die "plan lacks candidate_branch"
  [[ "${fingerprint}" =~ ^[0-9a-f]{40}$ ]] || die "plan lacks a valid plan_fingerprint"
  [ "$(state_value "${plan_file}" has_changes)" = true ] || die "repair dispatch requires a changing plan"
  print_command workflow run upstream-sync-v2.yml \
    --repo "${REPOSITORY}" \
    --ref main \
    -f mode=promote \
    -f force_candidate=false \
    -f "repair_ref=${branch}" \
    -f "repair_sha=${repair_sha}" \
    -f "repair_fingerprint=${fingerprint}" \
    -f "repair_pr=${repair_pr}"
}

cmd_recovery() {
  local state_file="" tag="" commit="" source_run_id="" source_run_attempt=""
  local artifact_id="" artifact_name="" artifact_digest="" source_head=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --state) state_file=${2:-}; shift 2 ;;
      --tag) tag=${2:-}; shift 2 ;;
      --commit) commit=${2:-}; shift 2 ;;
      --source-run-id) source_run_id=${2:-}; shift 2 ;;
      --source-run-attempt) source_run_attempt=${2:-}; shift 2 ;;
      --artifact-id) artifact_id=${2:-}; shift 2 ;;
      --artifact-name) artifact_name=${2:-}; shift 2 ;;
      --artifact-digest) artifact_digest=${2:-}; shift 2 ;;
      --source-head) source_head=${2:-}; shift 2 ;;
      --repository) REPOSITORY=${2:-}; shift 2 ;;
      *) die "unknown recovery argument: $1" ;;
    esac
  done
  require_file "${state_file}"
  require_sha commit "${commit}"
  require_sha source-head "${source_head}"
  require_positive_integer source-run-id "${source_run_id}"
  require_positive_integer source-run-attempt "${source_run_attempt}"
  require_positive_integer artifact-id "${artifact_id}"
  [[ "${artifact_digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "artifact-digest is invalid"
  [[ "${artifact_name}" =~ ^staged-release-assets-[1-9][0-9]*-[1-9][0-9]*$ ]] \
    || die "artifact-name is invalid"
  [ "${artifact_name}" = "staged-release-assets-${source_run_id}-${source_run_attempt}" ] \
    || die "artifact-name differs from the source run identity"
  parse_fork_release_tag "${tag}" || die "tag is invalid"
  [ "${FORK_TAG_SUFFIX}" -eq 0 ] || die "tag is not an upstream root"

  local sync_id fingerprint repair_pr state_tag state_commit
  sync_id=$(jq -er '.target.sync_id | select(type == "string" and length > 0)' "${state_file}") \
    || die "run state lacks target.sync_id"
  fingerprint=$(jq -er '.target.plan_fingerprint | select(test("^[0-9a-f]{40}$"))' "${state_file}") \
    || die "run state lacks a valid target.plan_fingerprint"
  repair_pr=$(jq -er '.repair.pr | select(type == "number" and floor == . and . > 0)' "${state_file}") \
    || die "guarded recovery requires a recorded repair PR"
  state_tag=$(jq -er '.target.expected_fork_tag | select(type == "string")' "${state_file}") \
    || die "run state lacks target.expected_fork_tag"
  state_commit=$(jq -er '.candidate.sha | select(test("^[0-9a-f]{40}$"))' "${state_file}") \
    || die "run state lacks a valid candidate SHA"
  jq -e '.candidate.acceptable == true' "${state_file}" >/dev/null \
    || die "guarded recovery requires an acceptable candidate"
  [ "${state_tag}" = "${tag}" ] || die "tag differs from the run-state target"
  [ "${state_commit}" = "${commit}" ] || die "commit differs from the run-state candidate"

  print_command workflow run sync-release-tag.yml \
    --repo "${REPOSITORY}" \
    --ref main \
    -f "tag=${tag}" \
    -f "expected_commit=${commit}" \
    -f "expected_sync_id=${sync_id}" \
    -f "expected_plan_fingerprint=${fingerprint}" \
    -f "source_run_id=${source_run_id}" \
    -f "source_run_attempt=${source_run_attempt}" \
    -f "staged_artifact_id=${artifact_id}" \
    -f "staged_artifact_digest=${artifact_digest}" \
    -f "source_workflow_head_sha=${source_head}"
}

main() {
  local command=${1:-}
  shift || true
  case "${command}" in
    repair) cmd_repair "$@" ;;
    recovery) cmd_recovery "$@" ;;
    *) die "usage: $0 {repair|recovery} [arguments]" ;;
  esac
}

main "$@"
