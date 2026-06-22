#!/usr/bin/env bash
set -euo pipefail

ORIGIN_REMOTE=${ORIGIN_REMOTE:-origin}
ORIGINAL_REMOTE=${ORIGINAL_REMOTE:-original-upstream}
PLUS_REMOTE=${PLUS_REMOTE:-plus-upstream}
ORIGINAL_REPOSITORY=${ORIGINAL_REPOSITORY:-router-for-me/CLIProxyAPI}
PLUS_REPOSITORY=${PLUS_REPOSITORY:-kaitranntt/CLIProxyAPIPlus}
TRACKING_ISSUE_LABEL=${TRACKING_ISSUE_LABEL:-upstream-sync-blocked}
PENDING_OVERLAY_BRANCH=${PENDING_OVERLAY_BRANCH:-upstream-sync/pending-overlay}
OWNERSHIP_FILE=${UPSTREAM_SYNC_OWNERSHIP_FILE:-.github/upstream-sync-ownership.tsv}
INVARIANTS_FILE=${UPSTREAM_SYNC_INVARIANTS_FILE:-.github/upstream-sync-invariants.tsv}

PLUS_OWNED_PREFIXES=(
  internal/auth/codebuddy/
  internal/auth/copilot/
  internal/auth/cursor/
  internal/auth/gitlab/
  internal/auth/iflow/
  internal/auth/kilo/
  internal/auth/kiro/
)

FORK_OWNED_PATHS=(
  .gitattributes
  README-ccs-fork.md
  docker-compose.yml
)

FORK_OWNED_PREFIXES=(
  .github/workflows/
)

die() {
  echo "[upstream-sync] $*" >&2
  exit 1
}

write_kv() {
  local target=${GITHUB_OUTPUT:-/dev/stdout}
  local key=$1
  local value=${2:-}

  if [[ "${value}" == *$'\n'* ]]; then
    local marker="EOF_${key}_$$_${RANDOM}"
    {
      echo "${key}<<${marker}"
      printf '%s\n' "${value}"
      echo "${marker}"
    } >> "${target}"
  else
    printf '%s=%s\n' "${key}" "${value}" >> "${target}"
  fi
}

write_env() {
  local target=${GITHUB_ENV:-}
  local key=$1
  local value=${2:-}
  [ -n "${target}" ] || return 0

  if [[ "${value}" == *$'\n'* ]]; then
    local marker="EOF_${key}_$$_${RANDOM}"
    {
      echo "${key}<<${marker}"
      printf '%s\n' "${value}"
      echo "${marker}"
    } >> "${target}"
  else
    printf '%s=%s\n' "${key}" "${value}" >> "${target}"
  fi
}

safe_ref_component() {
  printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '-'
}

latest_release_tag() {
  local remote=$1
  git ls-remote --tags --refs "${remote}" 'v*' \
    | awk '{ sub("refs/tags/", "", $2); print $2 }' \
    | grep -Ev -- '[-.]unstableneutron\.[0-9]+$' \
    | sort -V \
    | tail -n 1 || true
}

fetch_branch() {
  local remote=$1
  local branch=${2:-main}
  git fetch --no-tags "${remote}" "refs/heads/${branch}:refs/remotes/${remote}/${branch}"
}

fetch_tag() {
  local remote=$1
  local tag=$2
  git fetch --force --no-tags "${remote}" "refs/tags/${tag}:refs/tags/${tag}"
}

tag_commit() {
  git rev-list -n 1 "refs/tags/$1"
}

fork_tag_prefix_for_original_tag() {
  local original_tag=$1
  if [[ "${original_tag}" == *-* ]]; then
    printf '%s.unstableneutron' "${original_tag}"
  else
    printf '%s-unstableneutron' "${original_tag}"
  fi
}

latest_fork_tag_for_prefix() {
  local prefix=$1
  git ls-remote --tags --refs "${ORIGIN_REMOTE}" "${prefix}.[0-9]*" \
    | awk '{ sub("refs/tags/", "", $2); print $2 }' \
    | sort -V \
    | tail -n 1 || true
}

latest_fork_suffix_for_prefix() {
  local prefix=$1
  git ls-remote --tags --refs "${ORIGIN_REMOTE}" "${prefix}.[0-9]*" \
    | awk '{ sub("refs/tags/", "", $2); print $2 }' \
    | awk -v prefix="${prefix}." '
        index($0, prefix) == 1 {
          suffix = substr($0, length(prefix) + 1)
          if (suffix ~ /^[0-9]+$/) {
            if (!found || suffix + 0 > max) max = suffix + 0
            found = 1
          }
        }
        END { if (found) print max }
      '
}

is_plus_owned_path() {
  local path=$1
  local prefix
  if manifest_path_has_class plus-owned "${path}"; then
    return 0
  fi
  for prefix in "${PLUS_OWNED_PREFIXES[@]}"; do
    [[ "${path}" == "${prefix}"* ]] && return 0
  done
  return 1
}

is_fork_owned_path() {
  local path=$1
  local owned
  if manifest_path_has_class fork-owned "${path}"; then
    return 0
  fi
  for owned in "${FORK_OWNED_PATHS[@]}"; do
    [[ "${path}" == "${owned}" ]] && return 0
  done
  for owned in "${FORK_OWNED_PREFIXES[@]}"; do
    [[ "${path}" == "${owned}"* ]] && return 0
  done
  return 1
}

path_matches_rule() {
  local path=$1
  local rule=$2

  [ -n "${rule}" ] || return 1
  if [[ "${rule}" == */ ]]; then
    [[ "${path}" == "${rule}"* ]]
  else
    [[ "${path}" == "${rule}" ]]
  fi
}

manifest_path_has_class() {
  local wanted_class=$1
  local path=$2
  local file=${OWNERSHIP_FILE}
  local class rule

  [ -f "${file}" ] || return 1
  while IFS=$'\t' read -r class rule _; do
    [[ -n "${class}" && "${class}" != \#* ]] || continue
    [ "${class}" = "${wanted_class}" ] || continue
    if path_matches_rule "${path}" "${rule}"; then
      return 0
    fi
  done < "${file}"
  return 1
}

manifest_rules_for_class() {
  local wanted_class=$1
  local file=${OWNERSHIP_FILE}
  local class rule

  [ -f "${file}" ] || return 0
  while IFS=$'\t' read -r class rule _; do
    [[ -n "${class}" && "${class}" != \#* ]] || continue
    [ "${class}" = "${wanted_class}" ] || continue
    [ -n "${rule}" ] || continue
    printf '%s\n' "${rule}"
  done < "${file}"
}

require_ownership_manifest() {
  [ -f "${OWNERSHIP_FILE}" ] || die "ownership file not found: ${OWNERSHIP_FILE}"
}

classify_path() {
  local path=$1
  if is_plus_owned_path "${path}"; then
    echo "plus-owned"
  elif is_fork_owned_path "${path}"; then
    echo "fork-owned"
  else
    echo "shared-hotspot"
  fi
}

join_csv() {
  awk 'NF { if (out != "") out = out ","; out = out $0 } END { print out }'
}

classify_paths_table() {
  local paths=$1
  local path class
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    class=$(classify_path "${path}")
    # shellcheck disable=SC2016
    printf '| `%s` | `%s` |\n' "${path}" "${class}"
  done <<< "${paths}"
}

all_paths_plus_owned() {
  local paths=$1
  local path
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    is_plus_owned_path "${path}" || return 1
  done <<< "${paths}"
  return 0
}

unsafe_paths_from() {
  local paths=$1
  local path
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    if ! is_plus_owned_path "${path}"; then
      printf '%s\n' "${path}"
    fi
  done <<< "${paths}"
}

commit_contains_all() {
  local container=$1
  shift
  local commit
  for commit in "$@"; do
    [ -n "${commit}" ] || continue
    git merge-base --is-ancestor "${commit}" "${container}" || return 1
  done
  return 0
}

phase_key() {
  printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_'
}

install_original_merge_attributes() {
  local attrs=.git/info/attributes
  mkdir -p .git/info
  [ ! -f "${attrs}" ] || cp "${attrs}" "${attrs}.upstream-sync.bak"
  {
    echo '# upstream-sync original merge protections'
    local prefix
    for prefix in "${PLUS_OWNED_PREFIXES[@]}"; do
      printf '%s** merge=ours\n' "${prefix}"
    done
    local rule
    while IFS= read -r rule; do
      [ -n "${rule}" ] || continue
      if [[ "${rule}" == */ ]]; then
        printf '%s** merge=ours\n' "${rule}"
      else
        printf '%s merge=ours\n' "${rule}"
      fi
    done < <({ manifest_rules_for_class plus-owned; manifest_rules_for_class fork-owned; } | sort -u)
  } >> "${attrs}"
}

restore_original_merge_attributes() {
  local attrs=.git/info/attributes
  if [ -f "${attrs}.upstream-sync.bak" ]; then
    mv "${attrs}.upstream-sync.bak" "${attrs}"
  else
    sed -i.bak '/# upstream-sync original merge protections/,$d' "${attrs}" 2>/dev/null || true
    rm -f "${attrs}.bak"
  fi
}

preferred_conflict_side() {
  local phase=$1
  local path=$2

  if is_fork_owned_path "${path}"; then
    echo ours
    return 0
  fi
  if [ "${phase}" = "original" ] && is_plus_owned_path "${path}"; then
    echo ours
    return 0
  fi
  echo theirs
}

checkout_conflict_side() {
  local side=$1
  local ref=$2
  local path=$3

  if [ "${side}" = ours ]; then
    if git checkout --ours -- "${path}" 2>/dev/null; then
      git add -- "${path}" 2>/dev/null || true
    else
      git rm -f --ignore-unmatch "${path}" 2>/dev/null || true
    fi
    return 0
  fi

  if git cat-file -e "${ref}:${path}" 2>/dev/null; then
    if git checkout --theirs -- "${path}" 2>/dev/null; then
      git add -- "${path}" 2>/dev/null || true
    fi
  else
    git rm -f --ignore-unmatch "${path}" 2>/dev/null || true
  fi
}

restore_fork_owned_paths() {
  local source_ref=$1
  local path prefix

  for path in "${FORK_OWNED_PATHS[@]}"; do
    if git cat-file -e "${source_ref}:${path}" 2>/dev/null; then
      git checkout "${source_ref}" -- "${path}" 2>/dev/null || true
    else
      git rm -f --ignore-unmatch "${path}" 2>/dev/null || true
    fi
  done

  for prefix in "${FORK_OWNED_PREFIXES[@]}"; do
    git ls-tree -r --name-only "${source_ref}" -- "${prefix}" \
      | while IFS= read -r path; do
          [ -n "${path}" ] || continue
          git checkout "${source_ref}" -- "${path}" 2>/dev/null || true
        done

    git ls-files -- "${prefix}" \
      | while IFS= read -r path; do
          [ -n "${path}" ] || continue
          if ! git cat-file -e "${source_ref}:${path}" 2>/dev/null; then
            git rm -f --ignore-unmatch "${path}" 2>/dev/null || true
          fi
        done
  done

  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    if [[ "${path}" == */ ]]; then
      git ls-tree -r --name-only "${source_ref}" -- "${path}" \
        | while IFS= read -r owned_path; do
            [ -n "${owned_path}" ] || continue
            git checkout "${source_ref}" -- "${owned_path}" 2>/dev/null || true
          done

      git ls-files -- "${path}" \
        | while IFS= read -r owned_path; do
            [ -n "${owned_path}" ] || continue
            if ! git cat-file -e "${source_ref}:${owned_path}" 2>/dev/null; then
              git rm -f --ignore-unmatch "${owned_path}" 2>/dev/null || true
            fi
          done
    elif git cat-file -e "${source_ref}:${path}" 2>/dev/null; then
      git checkout "${source_ref}" -- "${path}" 2>/dev/null || true
    else
      git rm -f --ignore-unmatch "${path}" 2>/dev/null || true
    fi
  done < <(manifest_rules_for_class fork-owned)

  git add -A -- "${FORK_OWNED_PATHS[@]}" "${FORK_OWNED_PREFIXES[@]}" 2>/dev/null || true
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    git add -A -- "${path}" 2>/dev/null || true
  done < <(manifest_rules_for_class fork-owned)
}

owned_clobber_paths() {
  local phase=$1
  local pre_merge_head=$2
  local ref=$3
  local merge_base path class

  merge_base=$(git merge-base "${pre_merge_head}" "${ref}")
  git -c diff.renames=false diff --name-only "${merge_base}" "${ref}" \
    | while IFS= read -r path; do
        [ -n "${path}" ] || continue
        if git diff --quiet --no-ext-diff "${pre_merge_head}" "${ref}" -- "${path}"; then
          continue
        fi
        class=$(classify_path "${path}")
        case "${phase}:${class}" in
          original:fork-owned|original:plus-owned|plus-tag:fork-owned|plus-head:fork-owned)
            printf '%s\n' "${path}"
            ;;
        esac
      done \
    | sort -u
}

merge_lines() {
  awk 'NF && !seen[$0]++ { print }'
}

recorded_state_value() {
  local key=$1
  local file=.ccs-fork-upstream.env
  [ -f "${file}" ] || return 0
  awk -F= -v key="${key}" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "${file}"
}

append_drift_line() {
  local label=$1
  local old=$2
  local new=$3
  [ -n "${old}" ] || return 0
  [ "${old}" != "${new}" ] || return 0
  # shellcheck disable=SC2016
  printf '%s: `%s` -> `%s`\n' "${label}" "${old}" "${new}"
}

phase_output_value() {
  local file=$1
  local key=$2
  awk -F= -v key="${key}" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "${file}"
}

print_phase_report() {
  local label=$1
  local output_file=$2
  local conflicts conflict_files
  conflicts=$(phase_output_value "${output_file}" conflicts)
  conflict_files=$(phase_output_value "${output_file}" conflict_files)

  printf '%s: conflicts=%s\n' "${label}" "${conflicts:-false}"
  if [ -n "${conflict_files}" ]; then
    printf '%s\n' "${conflict_files}"
  fi
}

run_replay_gate() {
  local label=$1
  shift
  local log_file=$1
  shift

  set +e
  "$@" > "${log_file}" 2>&1
  local exit_code=$?
  set -e

  if [ "${exit_code}" -eq 0 ]; then
    printf '%s status: passed\n' "${label}"
  else
    printf '%s status: failed\n' "${label}"
    tail -n 40 "${log_file}" || true
  fi
  return "${exit_code}"
}

cmd_plan() {
  require_ownership_manifest

  local force_rebuild=${FORCE_REBUILD:-false}

  fetch_branch "${ORIGINAL_REMOTE}" main
  fetch_branch "${PLUS_REMOTE}" main

  local original_tag plus_tag
  original_tag=$(latest_release_tag "${ORIGINAL_REMOTE}")
  plus_tag=$(latest_release_tag "${PLUS_REMOTE}")

  write_kv original_tag "${original_tag}"
  write_kv plus_tag "${plus_tag}"

  if [ -z "${original_tag}" ] || [ -z "${plus_tag}" ]; then
    write_kv has_changes false
    write_kv blocked true
    write_kv block_reason missing-release-tag
    echo "[i] Missing release tag: original=${original_tag:-<none>} plus=${plus_tag:-<none>}"
    return 0
  fi

  fetch_tag "${ORIGINAL_REMOTE}" "${original_tag}"
  fetch_tag "${PLUS_REMOTE}" "${plus_tag}"

  local original_commit plus_tag_commit plus_head_commit
  original_commit=$(tag_commit "${original_tag}")
  plus_tag_commit=$(tag_commit "${plus_tag}")
  plus_head_commit=$(git rev-parse "refs/remotes/${PLUS_REMOTE}/main")

  local fork_tag_prefix latest_fork_tag latest_fork_suffix next_fork_suffix next_fork_tag
  fork_tag_prefix=$(fork_tag_prefix_for_original_tag "${original_tag}")
  latest_fork_tag=$(latest_fork_tag_for_prefix "${fork_tag_prefix}")
  latest_fork_suffix=$(latest_fork_suffix_for_prefix "${fork_tag_prefix}")
  if [ -n "${latest_fork_suffix}" ]; then
    next_fork_suffix=$((latest_fork_suffix + 1))
  else
    next_fork_suffix=0
  fi
  next_fork_tag="${fork_tag_prefix}.${next_fork_suffix}"

  local latest_fork_commit=""
  if [ -n "${latest_fork_tag}" ]; then
    fetch_tag "${ORIGIN_REMOTE}" "${latest_fork_tag}"
    latest_fork_commit=$(tag_commit "${latest_fork_tag}")
  fi

  local blocked=false block_reason="" plus_head_included=false
  local plus_head_delta_paths="" unsafe_plus_head_delta_paths=""
  local plus_head_already_represented=false
  if [ -n "${latest_fork_commit}" ] && git merge-base --is-ancestor "${plus_head_commit}" "${latest_fork_commit}"; then
    plus_head_already_represented=true
  fi

  if [ "${plus_head_commit}" != "${plus_tag_commit}" ] && [ "${plus_head_already_represented}" != true ]; then
    if git merge-base --is-ancestor "${plus_tag_commit}" "${plus_head_commit}"; then
      plus_head_delta_paths=$(git diff --name-only "${plus_tag_commit}" "${plus_head_commit}")
      if all_paths_plus_owned "${plus_head_delta_paths}"; then
        plus_head_included=true
      else
        blocked=true
        block_reason=plus-head-delta-touches-shared-paths
        unsafe_plus_head_delta_paths=$(unsafe_paths_from "${plus_head_delta_paths}")
      fi
    else
      blocked=true
      block_reason=plus-head-is-not-descendant-of-plus-tag
    fi
  fi

  local selected_targets=("${original_commit}" "${plus_tag_commit}")
  if [ "${plus_head_included}" = true ]; then
    selected_targets+=("${plus_head_commit}")
  fi

  local has_changes=true
  if [ "${force_rebuild}" != true ] && [ "${blocked}" != true ] && [ -n "${latest_fork_commit}" ]; then
    if commit_contains_all "${latest_fork_commit}" "${selected_targets[@]}"; then
      has_changes=false
    fi
  fi

  local safe_sync_id
  safe_sync_id="original-$(safe_ref_component "${original_tag}")_plus-$(safe_ref_component "${plus_tag}")"

  local drift_summary="" drift_line="" target_drift=false
  for drift_line in \
    "$(append_drift_line "Original tag" "$(recorded_state_value ORIGINAL_TAG)" "${original_tag}")" \
    "$(append_drift_line "Original commit" "$(recorded_state_value ORIGINAL_COMMIT)" "${original_commit}")" \
    "$(append_drift_line "Plus tag" "$(recorded_state_value PLUS_TAG)" "${plus_tag}")" \
    "$(append_drift_line "Plus tag commit" "$(recorded_state_value PLUS_TAG_COMMIT)" "${plus_tag_commit}")" \
    "$(append_drift_line "Plus head commit" "$(recorded_state_value PLUS_HEAD_COMMIT)" "${plus_head_commit}")" \
    "$(append_drift_line "Plus head included" "$(recorded_state_value PLUS_HEAD_INCLUDED)" "${plus_head_included}")"; do
    [ -n "${drift_line}" ] || continue
    drift_summary="${drift_summary}${drift_line}"$'\n'
  done
  if [ -n "${drift_summary}" ]; then
    target_drift=true
  fi

  write_kv original_repository "${ORIGINAL_REPOSITORY}"
  write_kv plus_repository "${PLUS_REPOSITORY}"
  write_kv original_head "${original_commit}"
  write_kv plus_tag_head "${plus_tag_commit}"
  write_kv plus_head "${plus_head_commit}"
  write_kv plus_head_included "${plus_head_included}"
  write_kv plus_head_already_represented "${plus_head_already_represented}"
  write_kv plus_head_delta_paths "$(printf '%s\n' "${plus_head_delta_paths}" | join_csv)"
  write_kv unsafe_plus_head_delta_paths "$(printf '%s\n' "${unsafe_plus_head_delta_paths}" | join_csv)"
  write_kv blocked "${blocked}"
  write_kv block_reason "${block_reason}"
  write_kv fork_tag_prefix "${fork_tag_prefix}"
  write_kv latest_fork_tag "${latest_fork_tag}"
  write_kv latest_fork_suffix "${latest_fork_suffix}"
  write_kv next_fork_tag "${next_fork_tag}"
  write_kv safe_sync_id "${safe_sync_id}"
  write_kv target_drift "${target_drift}"
  write_kv target_drift_summary "${drift_summary}"
  write_kv has_changes "${has_changes}"

  echo "[i] original ${original_tag} (${original_commit})"
  echo "[i] plus tag ${plus_tag} (${plus_tag_commit}); plus head ${plus_head_commit}; include_head=${plus_head_included}"
  echo "[i] next fork tag ${next_fork_tag}; has_changes=${has_changes}; blocked=${blocked}${block_reason:+ (${block_reason})}"
}

cmd_merge_ref() {
  require_ownership_manifest

  local phase=${1:-}
  local ref=${2:-}
  [ -n "${phase}" ] || die "merge-ref requires phase"
  [ -n "${ref}" ] || die "merge-ref requires ref"

  git config merge.ours.driver true
  local pre_merge_head
  pre_merge_head=$(git rev-parse HEAD)

  if [ "${phase}" = original ]; then
    install_original_merge_attributes
  fi

  set +e
  git merge --no-ff --no-commit "${ref}"
  local merge_exit=$?
  set -e

  if [ "${phase}" = original ]; then
    restore_original_merge_attributes
  fi

  local unmerged
  unmerged=$(git ls-files -u | awk '{print $4}' | sort -u || true)
  local ownership_clobbers conflict_paths
  ownership_clobbers=$(owned_clobber_paths "${phase}" "${pre_merge_head}" "${ref}")
  conflict_paths=$(printf '%s\n%s\n' "${unmerged}" "${ownership_clobbers}" | merge_lines)
  local key
  key=$(phase_key "${phase}")

  if [ -z "${conflict_paths}" ] && [ "${merge_exit}" -eq 0 ]; then
    restore_fork_owned_paths "${pre_merge_head}"
    if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
      git commit \
        -m "chore(upstream-sync): merge ${phase} ref" \
        -m "Automated upstream-sync merge for ${phase}: ${ref}. Fork-owned files are restored from the fork side before committing."
    fi
    write_kv conflicts false
    write_kv ownership_clobber_files ""
    write_env "${key}_CONFLICT_FILES" ""
    write_env "${key}_CONFLICT_TABLE" ""
    echo "[OK] ${phase} merge completed without conflicts."
    return 0
  fi

  write_kv conflicts true
  write_kv conflict_files "${conflict_paths}"
  write_kv conflict_table "$(classify_paths_table "${conflict_paths}")"
  write_kv ownership_clobber_files "${ownership_clobbers}"
  write_env "${key}_CONFLICT_FILES" "${conflict_paths}"
  write_env "${key}_CONFLICT_TABLE" "$(classify_paths_table "${conflict_paths}")"

  if [ -z "${conflict_paths}" ]; then
    echo "[!] ${phase} merge failed without conflict paths; leaving branch for inspection."
    return 0
  fi

  echo "[!] ${phase} merge conflicts or owned clobbers detected; creating blocked preview commit."
  echo "${conflict_paths}"
  if [ -n "${ownership_clobbers}" ]; then
    echo "[!] ${phase} owned clobbers:"
    echo "${ownership_clobbers}"
  fi
  local path side
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    git ls-files -u -- "${path}" | grep -q . || continue
    side=$(preferred_conflict_side "${phase}" "${path}")
    checkout_conflict_side "${side}" "${ref}" "${path}"
  done <<< "${conflict_paths}"

  restore_fork_owned_paths "${pre_merge_head}"
  git add -A
  if ! git commit \
    -m "chore(upstream-sync): preview ${phase} merge" \
    -m "Auto-resolved a blocked ${phase} merge to make the sync branch inspectable. The tracking issue lists conflict files and ownership guidance before this can land."; then
    echo "[!] Failed to create blocked preview commit; aborting merge state."
    git merge --abort 2>/dev/null || true
  fi
}

cmd_record_state() {
  : "${ORIGINAL_TAG:?ORIGINAL_TAG is required}"
  : "${ORIGINAL_COMMIT:?ORIGINAL_COMMIT is required}"
  : "${PLUS_TAG:?PLUS_TAG is required}"
  : "${PLUS_TAG_COMMIT:?PLUS_TAG_COMMIT is required}"
  : "${PLUS_HEAD_COMMIT:?PLUS_HEAD_COMMIT is required}"
  : "${PLUS_HEAD_INCLUDED:?PLUS_HEAD_INCLUDED is required}"

  {
    echo "ORIGINAL_REPOSITORY=${ORIGINAL_REPOSITORY}"
    echo "ORIGINAL_TAG=${ORIGINAL_TAG}"
    echo "ORIGINAL_COMMIT=${ORIGINAL_COMMIT}"
    echo "PLUS_REPOSITORY=${PLUS_REPOSITORY}"
    echo "PLUS_TAG=${PLUS_TAG}"
    echo "PLUS_TAG_COMMIT=${PLUS_TAG_COMMIT}"
    echo "PLUS_HEAD_COMMIT=${PLUS_HEAD_COMMIT}"
    echo "PLUS_HEAD_INCLUDED=${PLUS_HEAD_INCLUDED}"
  } > .ccs-fork-upstream.env
}

cmd_replay_plan() {
  require_ownership_manifest

  local root replay_dir
  root=$(mktemp -d)
  replay_dir="${root}/repo"
  git clone -q "$(pwd)" "${replay_dir}"

  local remote_name remote_url
  for remote_name in "${ORIGIN_REMOTE}" "${ORIGINAL_REMOTE}" "${PLUS_REMOTE}"; do
    remote_url=$(git remote get-url "${remote_name}" 2>/dev/null || true)
    [ -n "${remote_url}" ] || continue
    if git -C "${replay_dir}" remote get-url "${remote_name}" >/dev/null 2>&1; then
      git -C "${replay_dir}" remote set-url "${remote_name}" "${remote_url}"
    else
      git -C "${replay_dir}" remote add "${remote_name}" "${remote_url}"
    fi
  done

  (
    cd "${replay_dir}"
    local plan_out original_out plus_tag_out plus_head_out invariant_log build_log test_log
    plan_out="${root}/plan.out"
    original_out="${root}/original.out"
    plus_tag_out="${root}/plus-tag.out"
    plus_head_out="${root}/plus-head.out"
    invariant_log="${root}/invariants.log"
    build_log="${root}/build.log"
    test_log="${root}/test.log"

    FORCE_REBUILD="${FORCE_REBUILD:-false}" GITHUB_OUTPUT="${plan_out}" "${BASH_SOURCE[0]}" plan >/dev/null

    local original_tag plus_tag original_head plus_tag_head plus_head plus_head_included
    original_tag=$(phase_output_value "${plan_out}" original_tag)
    plus_tag=$(phase_output_value "${plan_out}" plus_tag)
    original_head=$(phase_output_value "${plan_out}" original_head)
    plus_tag_head=$(phase_output_value "${plan_out}" plus_tag_head)
    plus_head=$(phase_output_value "${plan_out}" plus_head)
    plus_head_included=$(phase_output_value "${plan_out}" plus_head_included)

    printf 'Original tag: %s\n' "${original_tag}"
    printf 'Plus tag: %s\n' "${plus_tag}"

    GITHUB_OUTPUT="${original_out}" "${BASH_SOURCE[0]}" merge-ref original "${original_head}" >/dev/null
    print_phase_report "Original merge" "${original_out}"

    GITHUB_OUTPUT="${plus_tag_out}" "${BASH_SOURCE[0]}" merge-ref plus-tag "${plus_tag_head}" >/dev/null
    print_phase_report "Plus release overlay" "${plus_tag_out}"

    if [ "${plus_head_included}" = true ] && [ "${plus_head}" != "${plus_tag_head}" ]; then
      GITHUB_OUTPUT="${plus_head_out}" "${BASH_SOURCE[0]}" merge-ref plus-head "${plus_head}" >/dev/null
      print_phase_report "Plus head delta" "${plus_head_out}"
    else
      printf 'Plus head delta: skipped\n'
    fi

    local gate_failed=false
    run_replay_gate "Invariant" "${invariant_log}" "${BASH_SOURCE[0]}" check-invariants || gate_failed=true
    run_replay_gate "Build" "${build_log}" bash -c "${UPSTREAM_SYNC_REPLAY_BUILD_CMD:-go build -o test-output ./cmd/server && rm test-output}" || gate_failed=true
    run_replay_gate "Test" "${test_log}" bash -c "${UPSTREAM_SYNC_REPLAY_TEST_CMD:-go test ./...}" || gate_failed=true
    if [ "${gate_failed}" = true ]; then
      return 1
    fi
  )
}

cmd_classify_paths() {
  require_ownership_manifest

  local paths=${1:-}
  if [ -n "${paths}" ]; then
    classify_paths_table "${paths}"
  else
    classify_paths_table "$(cat)"
  fi
}

cmd_pending_overlay_branch() {
  printf '%s\n' "${PENDING_OVERLAY_BRANCH}"
}

cmd_check_invariants() {
  local file=${INVARIANTS_FILE}
  local check path pattern description
  local failed=0

  [ -f "${file}" ] || die "invariants file not found: ${file}"
  while IFS=$'\t' read -r check path pattern description; do
    [[ -n "${check}" && "${check}" != \#* ]] || continue
    case "${check}" in
      contains)
        if [ ! -f "${path}" ]; then
          echo "[FAIL] invariant: ${description:-${path}} (${path} missing)" >&2
          failed=1
        elif ! grep -Fq -- "${pattern}" "${path}"; then
          echo "[FAIL] invariant: ${description:-${path}} (${path} missing pattern: ${pattern})" >&2
          failed=1
        else
          echo "[OK] invariant: ${description:-${path}}"
        fi
        ;;
      *)
        echo "[FAIL] invariant: unsupported check ${check} for ${path}" >&2
        failed=1
        ;;
    esac
  done < "${file}"

  return "${failed}"
}

main() {
  local cmd=${1:-}
  shift || true
  case "${cmd}" in
    plan) cmd_plan "$@" ;;
    merge-ref) cmd_merge_ref "$@" ;;
    replay-plan) cmd_replay_plan "$@" ;;
    record-state) cmd_record_state "$@" ;;
    classify-paths) cmd_classify_paths "$@" ;;
    check-invariants) cmd_check_invariants "$@" ;;
    pending-overlay-branch) cmd_pending_overlay_branch "$@" ;;
    *) die "usage: $0 {plan|merge-ref|replay-plan|record-state|classify-paths|check-invariants|pending-overlay-branch}" ;;
  esac
}

main "$@"
