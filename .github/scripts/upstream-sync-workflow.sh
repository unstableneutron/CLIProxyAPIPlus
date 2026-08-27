#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
RECEIPT_TEMP_DIR=""


die() {
  echo "[upstream-sync-workflow] $*" >&2
  exit 1
}

write_output() {
  local key=$1 value=$2
  [ -n "${GITHUB_OUTPUT:-}" ] || die "GITHUB_OUTPUT is required"
  printf '%s=%s\n' "${key}" "${value}" >> "${GITHUB_OUTPUT}"
}

state_value() {
  local file=$1 key=$2
  awk -F= -v key="${key}" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "${file}"
}

require_sha() {
  local name=$1 value=$2
  [[ "${value}" =~ ^[0-9a-f]{40}$ ]] || die "${name} must be an exact lowercase 40-character SHA"
}

require_positive_integer() {
  local name=$1 value=$2
  [[ "${value}" =~ ^[1-9][0-9]*$ ]] || die "${name} must be a positive decimal integer"
}

cmd_classify_candidate() {
  local validation_file=$1 report_file=$2
  local validation_status candidate_sha composition_acceptable=false acceptable=false
  validation_status=$(state_value "${validation_file}" OVERALL_STATUS)
  [ -n "${validation_status}" ] || die "validation report lacks OVERALL_STATUS"
  candidate_sha=$(git rev-parse HEAD)
  if [ "${MANUAL_COMPOSITION:-}" != true ] || [ "${REPAIR_VALIDATED:-}" = true ]; then
    composition_acceptable=true
  fi
  if [ "${BLOCKED:-}" != true ] && \
     [ "${CONFLICTS:-}" != true ] && \
     [ "${FRESH:-}" = true ] && \
     [ "${composition_acceptable}" = true ] && \
     [ "${validation_status}" = passed ]; then
    acceptable=true
  fi
  write_output acceptable "${acceptable}"
  write_output candidate_sha "${candidate_sha}"
  write_output validation_status "${validation_status}"
  if [ "${acceptable}" = true ]; then
    write_output outcome candidate-validated
  else
    write_output outcome needs-manual-action
  fi
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    cat "${report_file}" >> "${GITHUB_STEP_SUMMARY}"
  fi
}

cmd_select_effective_candidate() {
  if [ "${RESUME_RELEASE:-}" = true ]; then
    write_output acceptable "${RESUME_ACCEPTABLE:-}"
    write_output base_fork_commit "${RESUME_BASE_FORK_COMMIT:-}"
    write_output candidate_branch "${RESUME_CANDIDATE_BRANCH:-}"
    write_output candidate_sha "${RESUME_CANDIDATE_SHA:-}"
    write_output expected_fork_tag "${RESUME_EXPECTED_FORK_TAG:-}"
    write_output has_changes "${RESUME_HAS_CHANGES:-}"
    write_output plan_fingerprint "${RESUME_PLAN_FINGERPRINT:-}"
    write_output resume_release true
    write_output sync_id "${RESUME_SYNC_ID:-}"
  else
    write_output acceptable "${NORMAL_ACCEPTABLE:-}"
    write_output base_fork_commit "${NORMAL_BASE_FORK_COMMIT:-}"
    write_output candidate_branch "${NORMAL_CANDIDATE_BRANCH:-}"
    write_output candidate_sha "${NORMAL_CANDIDATE_SHA:-}"
    write_output expected_fork_tag "${NORMAL_EXPECTED_FORK_TAG:-}"
    write_output has_changes "${NORMAL_HAS_CHANGES:-}"
    write_output plan_fingerprint "${NORMAL_PLAN_FINGERPRINT:-}"
    write_output resume_release false
    write_output sync_id "${NORMAL_SYNC_ID:-}"
  fi
}

cmd_publish_candidate() {
  local branch=$1 fingerprint=$2
  local remote_sha remote_fingerprint
  remote_sha=$(git ls-remote --heads origin "refs/heads/${branch}" | awk '{ print $1; exit }')
  if [ -n "${remote_sha}" ]; then
    git fetch --quiet origin "refs/heads/${branch}:refs/upstream-sync-existing/${GITHUB_RUN_ID}"
    remote_fingerprint=$(git show "${remote_sha}:.ccs-fork-upstream.env" | state_value /dev/stdin PLAN_FINGERPRINT)
    [ "${remote_fingerprint}" = "${fingerprint}" ] \
      || die "remote candidate ${branch} has a different plan fingerprint"
  fi
  git push \
    "--force-with-lease=refs/heads/${branch}:${remote_sha}" \
    origin "HEAD:refs/heads/${branch}"
}

cmd_write_candidate_ledger() {
  local output_file=$1
  jq -n \
    --arg base_fork_commit "${BASE_FORK_COMMIT:-}" \
    --arg original_tag "${ORIGINAL_TAG:-}" \
    --arg original_commit "${ORIGINAL_COMMIT:-}" \
    --arg plus_tag "${PLUS_TAG:-}" \
    --arg plus_tag_commit "${PLUS_TAG_COMMIT:-}" \
    --arg plus_head "${PLUS_HEAD:-}" \
    --arg plus_head_included "${PLUS_HEAD_INCLUDED:-}" \
    --arg models_commit "${MODELS_COMMIT:-}" \
    --arg sync_id "${SYNC_ID:-}" \
    --arg plan_fingerprint "${PLAN_FINGERPRINT:-}" \
    --arg candidate_branch "${CANDIDATE_BRANCH:-}" \
    --arg candidate_sha "${CANDIDATE_SHA:-}" \
    --arg expected_fork_tag "${EXPECTED_FORK_TAG:-}" \
    --arg target_drift "${TARGET_DRIFT:-}" \
    --arg blocked "${BLOCKED:-}" \
    --arg repair_enabled "${REPAIR_ENABLED:-}" \
    --arg repair_pr "${REPAIR_PR:-}" \
    --arg repair_sha "${REPAIR_SHA:-}" \
    --arg validation_status "${VALIDATION_STATUS:-}" \
    --arg acceptable "${ACCEPTABLE:-}" '
      {
        schema_version: 1,
        state: (if $acceptable == "true" then "candidate_validated" else "needs-manual-action" end),
        target: {
          base_fork_commit: $base_fork_commit,
          original: {tag: $original_tag, commit: $original_commit},
          plus: {
            tag: $plus_tag,
            tag_commit: $plus_tag_commit,
            head: $plus_head,
            head_included: ($plus_head_included == "true")
          },
          models_commit: $models_commit,
          sync_id: $sync_id,
          plan_fingerprint: $plan_fingerprint,
          expected_fork_tag: $expected_fork_tag,
          target_drift: ($target_drift == "true"),
          blocked: ($blocked == "true")
        },
        candidate: {
          branch: $candidate_branch,
          sha: $candidate_sha,
          acceptable: ($acceptable == "true"),
          validation_status: $validation_status
        },
        repair: {
          imported: ($repair_enabled == "true"),
          pr: (if $repair_pr == "" then null else ($repair_pr | tonumber) end),
          sha: (if $repair_sha == "" then null else $repair_sha end)
        },
        final_plan: {status: "pending_external_verification"},
        runtime_smoke: "not_run",
        vn3_deployed: false
      }
    ' > "${output_file}"
}

cmd_reconcile_candidate_pr() {
  local branch=$1 tag=$2 report_file=$3
  local pr_number
  pr_number=$(gh pr list \
    --repo "${GITHUB_REPOSITORY}" \
    --head "${branch}" \
    --state open \
    --json number \
    --jq '.[0].number // empty')
  if [ -n "${pr_number}" ]; then
    gh pr edit "${pr_number}" \
      --repo "${GITHUB_REPOSITORY}" \
      --body-file "${report_file}"
  else
    gh pr create \
      --repo "${GITHUB_REPOSITORY}" \
      --base main \
      --head "${branch}" \
      --title "Resolve upstream sync ${tag}" \
      --body-file "${report_file}"
  fi
}

cmd_verify_final_plan() {
  local sync_work_dir=$1 promoted_commit=$2 main_policy=$3
  git remote get-url "${ORIGINAL_REMOTE}" >/dev/null 2>&1 || \
    git remote add "${ORIGINAL_REMOTE}" "https://github.com/${ORIGINAL_REPOSITORY}.git"
  git remote get-url "${PLUS_REMOTE}" >/dev/null 2>&1 || \
    git remote add "${PLUS_REMOTE}" "https://github.com/${PLUS_REPOSITORY}.git"
  git fetch --quiet origin main
  case "${main_policy}" in
    exact)
      [ "$(git rev-parse origin/main)" = "${promoted_commit}" ] \
        || die "origin/main moved after promotion"
      ;;
    descendant)
      git merge-base --is-ancestor "${promoted_commit}" origin/main \
        || die "origin/main is not descended from the promoted commit"
      ;;
    *) die "unsupported final main policy ${main_policy}" ;;
  esac
  local final_plan="${sync_work_dir}/final-plan.out"
  GITHUB_OUTPUT="${final_plan}" "${SCRIPT_DIR}/upstream-sync.sh" plan
  cat "${final_plan}"
  if [ "$(state_value "${final_plan}" has_changes)" != false ] || \
     [ "$(state_value "${final_plan}" target_drift)" != false ] || \
     [ "$(state_value "${final_plan}" blocked)" != false ]; then
    die "final planner did not reach clean no-op state"
  fi
}

cmd_finalize_release_ledger() {
  local state_file=$1 final_plan=$2 receipt_file=$3 promoted_commit=$4 tag=$5
  jq \
    --arg promoted_commit "${promoted_commit}" \
    --arg tag "${tag}" \
    --arg final_fingerprint "$(state_value "${final_plan}" plan_fingerprint)" \
    --arg final_has_changes "$(state_value "${final_plan}" has_changes)" \
    --arg final_target_drift "$(state_value "${final_plan}" target_drift)" \
    --arg final_blocked "$(state_value "${final_plan}" blocked)" \
    --slurpfile receipt "${receipt_file}" '
      $receipt[0] as $release |
      .state = "released" |
      .promotion = {commit: $promoted_commit, tag: $tag} |
      .release = {
        url: $release.release_url,
        assets: $release.release_assets,
        image: $release.image,
        image_digest: $release.image_digest,
        platforms: $release.platforms,
        architecture_images: ($release.architecture_images // {})
      } + if ($release | has("release_asset_identities")) then {
        asset_identities: $release.release_asset_identities
      } else {} end |
      .final_plan = {
        status: "clean-noop",
        plan_fingerprint: $final_fingerprint,
        has_changes: ($final_has_changes == "true"),
        target_drift: ($final_target_drift == "true"),
        blocked: ($final_blocked == "true")
      }
    ' "${state_file}" > "${state_file}.tmp"
  mv "${state_file}.tmp" "${state_file}"
}

cmd_attach_receipt() {
  local tag=$1 expected_commit=$2 main_policy=$3 receipt_file=$4
  local assets
  assets=$(gh release view "${tag}" --repo "${GITHUB_REPOSITORY}" --json assets --jq '.assets[].name')
  if grep -Fx upstream-sync-receipt.json <<< "${assets}" >/dev/null || \
     grep -Fx hotfix-release-receipt.json <<< "${assets}" >/dev/null; then
    die "a receipt identity appeared after upstream finalization preflight"
  fi
  "${SCRIPT_DIR}/revalidate-release-target.sh" "${tag}" "${expected_commit}" "${main_policy}"
  gh release upload "${tag}" "${receipt_file}" --repo "${GITHUB_REPOSITORY}"
  RECEIPT_TEMP_DIR=$(mktemp -d)
  trap 'if [ -n "${RECEIPT_TEMP_DIR:-}" ]; then rm -rf "${RECEIPT_TEMP_DIR}"; fi' EXIT
  gh release download "${tag}" \
    --repo "${GITHUB_REPOSITORY}" \
    --pattern "$(basename -- "${receipt_file}")" \
    --dir "${RECEIPT_TEMP_DIR}"
  cmp -s "${receipt_file}" "${RECEIPT_TEMP_DIR}/$(basename -- "${receipt_file}")" \
    || die "published upstream receipt bytes differ"
  rm -rf "${RECEIPT_TEMP_DIR}"
  RECEIPT_TEMP_DIR=""
  trap - EXIT
}

cmd_close_superseded_prs() {
  local accepted_tag=$1 accepted_sha=$2 run_url=$3
  local number head prs
  local failed=false
  prs=$(
    gh api --paginate --slurp \
      "/repos/${GITHUB_REPOSITORY}/pulls?state=open&per_page=100" |
      jq -r --arg repository "${GITHUB_REPOSITORY}" '
        .[][] |
        select(
          .head.repo.full_name == $repository and
          .base.ref == "main" and
          (.head.ref | startswith("upstream-sync/"))
        ) |
        [.number, .head.ref] | @tsv
      '
  ) || die "could not list superseded synchronization PRs"
  while IFS=$'\t' read -r number head; do
    [ -n "${number}" ] || continue
    if ! gh pr close "${number}" \
      --repo "${GITHUB_REPOSITORY}" \
      --comment "Superseded by verified release ${accepted_tag} at ${accepted_sha} (${run_url}). The candidate branch is retained for forensic history."; then
      printf '[FAIL] could not close superseded PR #%s (%s)\n' "${number}" "${head}" >&2
      failed=true
      continue
    fi
    printf '[OK] closed superseded PR #%s (%s)\n' "${number}" "${head}"
  done <<< "${prs}"
  [ "${failed}" = false ]
}

main() {
  local command=${1:-}
  shift || true
  case "${command}" in
    classify-candidate) cmd_classify_candidate "$@" ;;
    select-effective-candidate) cmd_select_effective_candidate "$@" ;;
    publish-candidate) cmd_publish_candidate "$@" ;;
    write-candidate-ledger) cmd_write_candidate_ledger "$@" ;;
    reconcile-candidate-pr) cmd_reconcile_candidate_pr "$@" ;;
    verify-final-plan) cmd_verify_final_plan "$@" ;;
    finalize-release-ledger) cmd_finalize_release_ledger "$@" ;;
    attach-receipt) cmd_attach_receipt "$@" ;;
    close-superseded-prs) cmd_close_superseded_prs "$@" ;;
    *) die "usage: $0 {classify-candidate|select-effective-candidate|publish-candidate|write-candidate-ledger|reconcile-candidate-pr|verify-final-plan|finalize-release-ledger|attach-receipt|close-superseded-prs}" ;;
  esac
}

main "$@"
