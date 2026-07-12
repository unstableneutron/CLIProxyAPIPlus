#!/usr/bin/env bash
set -euo pipefail

ORIGIN_REMOTE=${ORIGIN_REMOTE:-origin}
ORIGINAL_REMOTE=${ORIGINAL_REMOTE:-original-upstream}
PLUS_REMOTE=${PLUS_REMOTE:-plus-upstream}
ORIGINAL_REPOSITORY=${ORIGINAL_REPOSITORY:-router-for-me/CLIProxyAPI}
PLUS_REPOSITORY=${PLUS_REPOSITORY:-kaitranntt/CLIProxyAPIPlus}
MODELS_REPOSITORY=${MODELS_REPOSITORY:-router-for-me/models}
MODELS_REMOTE=${MODELS_REMOTE:-https://github.com/${MODELS_REPOSITORY}.git}
MODELS_BRANCH=${MODELS_BRANCH:-main}
FORK_BRANCH=${FORK_BRANCH:-main}
PENDING_OVERLAY_BRANCH=${PENDING_OVERLAY_BRANCH:-upstream-sync/pending-overlay}
OWNERSHIP_FILE=${UPSTREAM_SYNC_OWNERSHIP_FILE:-.github/upstream-sync-ownership.tsv}
INVARIANTS_FILE=${UPSTREAM_SYNC_INVARIANTS_FILE:-.github/upstream-sync-invariants.tsv}
DROPPED_SYMBOLS_FILE=${UPSTREAM_SYNC_DROPPED_SYMBOLS_FILE:-.github/upstream-sync-dropped-symbols.tsv}

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

remote_ref_commit() {
  local remote=$1
  local ref=$2
  local refs

  refs=$(git ls-remote "${remote}" "${ref}" "${ref}^{}") || return 1
  awk -v ref="${ref}" '
    $2 == ref { direct = $1 }
    $2 == ref "^{}" { peeled = $1 }
    END {
      if (peeled != "") print peeled
      else if (direct != "") print direct
      else exit 1
    }
  ' <<< "${refs}"
}

snapshot_ref() {
  local fingerprint=$1
  local slot=$2
  printf 'refs/upstream-sync/%s/%s\n' "$(safe_ref_component "${fingerprint}")" "$(safe_ref_component "${slot}")"
}

fetch_snapshot_ref() {
  local remote=$1
  local source_ref=$2
  local fingerprint=$3
  local slot=$4
  local target commit

  target=$(snapshot_ref "${fingerprint}" "${slot}")
  git fetch -q --force --no-tags "${remote}" "${source_ref}:${target}"
  commit=$(git rev-parse "${target}^{commit}")
  git update-ref "${target}" "${commit}"
  printf '%s\n' "${target}"
}

delete_snapshot_namespace() {
  local fingerprint=$1
  local ref

  [ -n "${fingerprint}" ] || return 0
  while IFS= read -r ref; do
    [ -n "${ref}" ] || continue
    git update-ref -d "${ref}"
  done < <(git for-each-ref --format='%(refname)' "refs/upstream-sync/$(safe_ref_component "${fingerprint}")/")
}

copy_snapshot_ref() {
  local source_fingerprint=$1
  local target_fingerprint=$2
  local slot=$3
  local source target commit

  source=$(snapshot_ref "${source_fingerprint}" "${slot}")
  target=$(snapshot_ref "${target_fingerprint}" "${slot}")
  commit=$(git rev-parse "${source}^{commit}")
  git update-ref "${target}" "${commit}"
}

plan_fingerprint() {
  git hash-object --stdin
}

candidate_branch_for_plan() {
  local safe_sync_id=$1
  local fingerprint=$2
  printf 'upstream-sync/%s-%s\n' "$(safe_ref_component "${safe_sync_id}")" "${fingerprint:0:12}"
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

is_plus_owned_path() {
  local path=$1
  manifest_path_has_class plus-owned "${path}"
}

is_fork_owned_path() {
  local path=$1
  manifest_path_has_class fork-owned "${path}"
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
  local attrs
  attrs=$(git rev-parse --git-path info/attributes)
  mkdir -p "$(dirname -- "${attrs}")"
  [ ! -f "${attrs}" ] || cp "${attrs}" "${attrs}.upstream-sync.bak"
  {
    echo '# upstream-sync original merge protections'
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
  local attrs
  attrs=$(git rev-parse --git-path info/attributes)
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
  echo manual
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
  local path

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

overlay_at_risk_report_path() {
  local phase=$1
  local report_dir=${UPSTREAM_SYNC_REPORT_DIR:-/tmp}

  mkdir -p "${report_dir}"
  printf '%s/overlay-at-risk-%s.diff\n' "${report_dir%/}" "$(safe_ref_component "${phase}")"
}

write_overlay_at_risk_report() {
  local phase=$1
  local pre_merge_head=$2
  local ref=$3
  local conflict_paths=$4
  local upstream_base path class report tmp hunk_count summary wrote

  upstream_base=$(git merge-base "${pre_merge_head}" "${ref}")
  report=$(overlay_at_risk_report_path "${phase}")
  tmp=$(mktemp)
  summary=""
  wrote=false
  : > "${report}"

  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    class=$(classify_path "${path}")
    [ "${class}" = shared-hotspot ] || continue

    git diff "${upstream_base}" "${pre_merge_head}" -- "${path}" > "${tmp}" || true
    [ -s "${tmp}" ] || continue
    hunk_count=$(grep -c '^@@ ' "${tmp}" || true)
    {
      printf '## %s (%s hunk%s)\n\n' "${path}" "${hunk_count}" "$([ "${hunk_count}" = 1 ] || printf s)"
      cat "${tmp}"
      printf '\n'
    } >> "${report}"
    # shellcheck disable=SC2016
    summary="${summary}| \`${path}\` | \`${hunk_count}\` |"$'\n'
    wrote=true
  done <<< "${conflict_paths}"

  rm -f "${tmp}"
  if [ "${wrote}" != true ]; then
    rm -f "${report}"
    report=""
    summary=""
  fi

  printf '%s\t%s\n' "${report}" "${summary}"
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
  awk -v key="${key}" '
    index($0, key "=") == 1 {
      print substr($0, length(key) + 2)
      exit
    }
    index($0, key "<<") == 1 {
      marker = substr($0, length(key) + 3)
      capture = 1
      next
    }
    capture {
      if ($0 == marker) exit
      print
    }
  ' "${file}"
}

cmd_plan() {
  require_ownership_manifest

  local force_rebuild=${FORCE_REBUILD:-false}
  local pre_sync_head planning_key
  pre_sync_head=$(git rev-parse HEAD)
  planning_key="planning-$$-${RANDOM}"
  PLAN_TEMP_NAMESPACE=${planning_key}
  trap 'if [ -n "${PLAN_TEMP_NAMESPACE:-}" ]; then delete_snapshot_namespace "${PLAN_TEMP_NAMESPACE}"; fi' EXIT

  local original_tag plus_tag
  original_tag=$(latest_release_tag "${ORIGINAL_REMOTE}")
  plus_tag=$(latest_release_tag "${PLUS_REMOTE}")

  write_kv original_tag "${original_tag}"
  write_kv plus_tag "${plus_tag}"
  write_kv pre_sync_head "${pre_sync_head}"
  write_kv base_fork_commit "${pre_sync_head}"

  if [ -z "${original_tag}" ] || [ -z "${plus_tag}" ]; then
    write_kv has_changes false
    write_kv blocked true
    write_kv block_reason missing-release-tag
    echo "[i] Missing release tag: original=${original_tag:-<none>} plus=${plus_tag:-<none>}"
    delete_snapshot_namespace "${planning_key}"
    PLAN_TEMP_NAMESPACE=""
    trap - EXIT
    return 0
  fi

  fetch_snapshot_ref "${ORIGINAL_REMOTE}" "refs/tags/${original_tag}" "${planning_key}" original >/dev/null
  fetch_snapshot_ref "${PLUS_REMOTE}" "refs/tags/${plus_tag}" "${planning_key}" plus-tag >/dev/null
  fetch_snapshot_ref "${PLUS_REMOTE}" refs/heads/main "${planning_key}" plus-head >/dev/null
  fetch_snapshot_ref "${MODELS_REMOTE}" "refs/heads/${MODELS_BRANCH}" "${planning_key}" models >/dev/null

  local original_commit plus_tag_commit plus_head_commit models_commit
  original_commit=$(git rev-parse "$(snapshot_ref "${planning_key}" original)^{commit}")
  plus_tag_commit=$(git rev-parse "$(snapshot_ref "${planning_key}" plus-tag)^{commit}")
  plus_head_commit=$(git rev-parse "$(snapshot_ref "${planning_key}" plus-head)^{commit}")
  models_commit=$(git rev-parse "$(snapshot_ref "${planning_key}" models)^{commit}")

  local fork_tag_prefix latest_fork_tag latest_fork_suffix next_fork_suffix next_fork_tag
  fork_tag_prefix=$(fork_tag_prefix_for_original_tag "${original_tag}")
  latest_fork_tag=$(latest_fork_tag_for_prefix "${fork_tag_prefix}")
  latest_fork_suffix=""
  if [ -n "${latest_fork_tag}" ]; then
    latest_fork_suffix=${latest_fork_tag#"${fork_tag_prefix}."}
  fi
  if [ -n "${latest_fork_suffix}" ]; then
    next_fork_suffix=$((latest_fork_suffix + 1))
  else
    next_fork_suffix=0
  fi
  next_fork_tag="${fork_tag_prefix}.${next_fork_suffix}"

  local latest_fork_commit=""
  if [ -n "${latest_fork_tag}" ]; then
    fetch_snapshot_ref "${ORIGIN_REMOTE}" "refs/tags/${latest_fork_tag}" "${planning_key}" latest-fork >/dev/null
    latest_fork_commit=$(git rev-parse "$(snapshot_ref "${planning_key}" latest-fork)^{commit}")
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

  local expected_fork_tag
  if [ "${has_changes}" = false ] && [ -n "${latest_fork_tag}" ]; then
    expected_fork_tag=${latest_fork_tag}
  else
    expected_fork_tag=${next_fork_tag}
  fi

  local fingerprint candidate_branch
  fingerprint=$(
    printf '%s\n' \
      "base_fork_commit=${pre_sync_head}" \
      "original_tag=${original_tag}" \
      "original_commit=${original_commit}" \
      "plus_tag=${plus_tag}" \
      "plus_tag_commit=${plus_tag_commit}" \
      "plus_head_commit=${plus_head_commit}" \
      "plus_head_included=${plus_head_included}" \
      "models_commit=${models_commit}" \
      "expected_fork_tag=${expected_fork_tag}" \
      | plan_fingerprint
  )
  candidate_branch=$(candidate_branch_for_plan "${safe_sync_id}" "${fingerprint}")

  local slot
  for slot in original plus-tag plus-head models; do
    copy_snapshot_ref "${planning_key}" "${fingerprint}" "${slot}"
  done
  if [ -n "${latest_fork_commit}" ]; then
    copy_snapshot_ref "${planning_key}" "${fingerprint}" latest-fork
  fi
  delete_snapshot_namespace "${planning_key}"
  PLAN_TEMP_NAMESPACE=""
  trap - EXIT

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
  write_kv models_repository "${MODELS_REPOSITORY}"
  write_kv original_head "${original_commit}"
  write_kv plus_tag_head "${plus_tag_commit}"
  write_kv plus_head "${plus_head_commit}"
  write_kv models_commit "${models_commit}"
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
  write_kv expected_fork_tag "${expected_fork_tag}"
  write_kv safe_sync_id "${safe_sync_id}"
  write_kv plan_fingerprint "${fingerprint}"
  write_kv candidate_branch "${candidate_branch}"
  write_kv snapshot_namespace "refs/upstream-sync/${fingerprint}"
  write_kv original_snapshot_ref "$(snapshot_ref "${fingerprint}" original)"
  write_kv plus_tag_snapshot_ref "$(snapshot_ref "${fingerprint}" plus-tag)"
  write_kv plus_head_snapshot_ref "$(snapshot_ref "${fingerprint}" plus-head)"
  write_kv models_snapshot_ref "$(snapshot_ref "${fingerprint}" models)"
  write_kv target_drift "${target_drift}"
  write_kv target_drift_summary "${drift_summary}"
  write_kv has_changes "${has_changes}"

  echo "[i] original ${original_tag} (${original_commit})"
  echo "[i] plus tag ${plus_tag} (${plus_tag_commit}); plus head ${plus_head_commit}; include_head=${plus_head_included}"
  echo "[i] expected fork tag ${expected_fork_tag}; has_changes=${has_changes}; blocked=${blocked}${block_reason:+ (${block_reason})}"
}

require_plan_value() {
  local plan_file=$1
  local key=$2
  local value

  value=$(phase_output_value "${plan_file}" "${key}")
  [ -n "${value}" ] || die "plan is missing required field: ${key}"
  printf '%s\n' "${value}"
}

verify_snapshot_commit() {
  local fingerprint=$1
  local slot=$2
  local expected=$3
  local ref actual

  ref=$(snapshot_ref "${fingerprint}" "${slot}")
  git rev-parse --verify "${ref}^{commit}" >/dev/null 2>&1 || die "snapshot ref not found: ${ref}"
  actual=$(git rev-parse "${ref}^{commit}")
  [ "${actual}" = "${expected}" ] || die "snapshot ref ${ref} moved: expected ${expected}, got ${actual}"
}

apply_models_snapshot() {
  local fingerprint=$1
  local target=internal/registry/models/models.json

  mkdir -p "$(dirname -- "${target}")"
  git show "$(snapshot_ref "${fingerprint}" models):models.json" > "${target}"
  git add "${target}"
  if ! git diff --cached --quiet -- "${target}"; then
    git commit \
      -m "Update snapshotted model catalog" \
      -m "Record the exact models repository snapshot selected by this upstream sync plan before validation."
  fi
}

cmd_materialize() {
  require_ownership_manifest

  local plan_file=${1:-}
  [ -n "${plan_file}" ] || die "materialize requires plan-output-file"
  [ -f "${plan_file}" ] || die "plan output not found: ${plan_file}"

  local base_fork_commit fingerprint candidate_branch
  local original_commit plus_tag_commit plus_head_commit models_commit plus_head_included
  base_fork_commit=$(require_plan_value "${plan_file}" base_fork_commit)
  fingerprint=$(require_plan_value "${plan_file}" plan_fingerprint)
  candidate_branch=$(require_plan_value "${plan_file}" candidate_branch)
  original_commit=$(require_plan_value "${plan_file}" original_head)
  plus_tag_commit=$(require_plan_value "${plan_file}" plus_tag_head)
  plus_head_commit=$(require_plan_value "${plan_file}" plus_head)
  models_commit=$(require_plan_value "${plan_file}" models_commit)
  plus_head_included=$(require_plan_value "${plan_file}" plus_head_included)

  [ "$(git rev-parse HEAD)" = "${base_fork_commit}" ] \
    || die "materialize must start at base fork commit ${base_fork_commit}"
  [ -z "$(git status --porcelain)" ] || die "materialize requires a clean worktree"

  verify_snapshot_commit "${fingerprint}" original "${original_commit}"
  verify_snapshot_commit "${fingerprint}" plus-tag "${plus_tag_commit}"
  verify_snapshot_commit "${fingerprint}" plus-head "${plus_head_commit}"
  verify_snapshot_commit "${fingerprint}" models "${models_commit}"

  git checkout -B "${candidate_branch}" "${base_fork_commit}" >/dev/null

  local root original_out plus_tag_out plus_head_out
  root=$(mktemp -d)
  MATERIALIZE_TEMP_ROOT=${root}
  trap 'if [ -n "${MATERIALIZE_TEMP_ROOT:-}" ]; then rm -rf "${MATERIALIZE_TEMP_ROOT}"; fi' EXIT
  original_out="${root}/original.out"
  plus_tag_out="${root}/plus-tag.out"
  plus_head_out="${root}/plus-head.out"

  GITHUB_OUTPUT="${original_out}" "${BASH_SOURCE[0]}" merge-ref original "$(snapshot_ref "${fingerprint}" original)" >/dev/null
  GITHUB_OUTPUT="${plus_tag_out}" "${BASH_SOURCE[0]}" merge-ref plus-tag "$(snapshot_ref "${fingerprint}" plus-tag)" >/dev/null

  local plus_head_ran=false
  if [ "${plus_head_included}" = true ] && [ "${plus_head_commit}" != "${plus_tag_commit}" ]; then
    GITHUB_OUTPUT="${plus_head_out}" "${BASH_SOURCE[0]}" merge-ref plus-head "$(snapshot_ref "${fingerprint}" plus-head)" >/dev/null
    plus_head_ran=true
  fi

  local original_conflicts plus_tag_conflicts plus_head_conflicts conflicts
  local original_conflict_files plus_tag_conflict_files plus_head_conflict_files conflict_files
  original_conflicts=$(phase_output_value "${original_out}" conflicts)
  plus_tag_conflicts=$(phase_output_value "${plus_tag_out}" conflicts)
  original_conflict_files=$(phase_output_value "${original_out}" conflict_files)
  plus_tag_conflict_files=$(phase_output_value "${plus_tag_out}" conflict_files)
  plus_head_conflicts=false
  plus_head_conflict_files=""
  if [ "${plus_head_ran}" = true ]; then
    plus_head_conflicts=$(phase_output_value "${plus_head_out}" conflicts)
    plus_head_conflict_files=$(phase_output_value "${plus_head_out}" conflict_files)
  fi
  conflict_files=$(printf '%s\n%s\n%s\n' \
    "${original_conflict_files}" \
    "${plus_tag_conflict_files}" \
    "${plus_head_conflict_files}" \
    | merge_lines)
  conflicts=false
  if [ "${original_conflicts}" = true ] || [ "${plus_tag_conflicts}" = true ] || [ "${plus_head_conflicts}" = true ]; then
    conflicts=true
  fi

  apply_models_snapshot "${fingerprint}"

  write_kv candidate_branch "${candidate_branch}"
  write_kv candidate_sha "$(git rev-parse HEAD)"
  write_kv conflicts "${conflicts}"
  write_kv conflict_files "${conflict_files}"
  write_kv original_conflicts "${original_conflicts:-false}"
  write_kv plus_tag_conflicts "${plus_tag_conflicts:-false}"
  write_kv plus_head_conflicts "${plus_head_conflicts:-false}"
  write_kv original_conflict_files "${original_conflict_files}"
  write_kv plus_tag_conflict_files "${plus_tag_conflict_files}"
  write_kv plus_head_conflict_files "${plus_head_conflict_files}"

  rm -rf "${root}"
  MATERIALIZE_TEMP_ROOT=""
  trap - EXIT
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
    write_kv overlay_at_risk_report ""
    write_kv overlay_at_risk_summary ""
    write_env "${key}_CONFLICT_FILES" ""
    write_env "${key}_CONFLICT_TABLE" ""
    write_env "${key}_OVERLAY_AT_RISK_REPORT" ""
    write_env "${key}_OVERLAY_AT_RISK_SUMMARY" ""
    echo "[OK] ${phase} merge completed without conflicts."
    return 0
  fi

  local overlay_at_risk overlay_at_risk_report overlay_at_risk_summary
  overlay_at_risk=$(write_overlay_at_risk_report "${phase}" "${pre_merge_head}" "${ref}" "${conflict_paths}")
  overlay_at_risk_report=${overlay_at_risk%%$'\t'*}
  overlay_at_risk_summary=${overlay_at_risk#*$'\t'}
  if [ "${overlay_at_risk_report}" = "${overlay_at_risk}" ]; then
    overlay_at_risk_summary=""
  fi

  write_kv conflicts true
  write_kv conflict_files "${conflict_paths}"
  write_kv conflict_table "$(classify_paths_table "${conflict_paths}")"
  write_kv ownership_clobber_files "${ownership_clobbers}"
  write_kv overlay_at_risk_report "${overlay_at_risk_report}"
  write_kv overlay_at_risk_summary "${overlay_at_risk_summary}"
  write_env "${key}_CONFLICT_FILES" "${conflict_paths}"
  write_env "${key}_CONFLICT_TABLE" "$(classify_paths_table "${conflict_paths}")"
  write_env "${key}_OVERLAY_AT_RISK_REPORT" "${overlay_at_risk_report}"
  write_env "${key}_OVERLAY_AT_RISK_SUMMARY" "${overlay_at_risk_summary}"

  if [ -z "${conflict_paths}" ]; then
    git merge --abort 2>/dev/null || true
    echo "[!] ${phase} merge failed without conflict paths; aborted to the pre-merge commit."
    return 0
  fi

  echo "[!] ${phase} merge conflicts or owned clobbers detected; creating blocked preview commit."
  echo "${conflict_paths}"
  if [ -n "${ownership_clobbers}" ]; then
    echo "[!] ${phase} owned clobbers:"
    echo "${ownership_clobbers}"
  fi
  local path side manual_composition=false
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    git ls-files -u -- "${path}" | grep -q . || continue
    side=$(preferred_conflict_side "${phase}" "${path}")
    if [ "${side}" = manual ]; then
      echo "[!] ${path} requires manual composition; neither side was checked out."
      manual_composition=true
      continue
    fi
    checkout_conflict_side "${side}" "${ref}" "${path}"
  done <<< "${conflict_paths}"

  if [ "${manual_composition}" = true ]; then
    git merge --abort 2>/dev/null || true
    echo "[!] ${phase} merge aborted at the pre-merge commit for manual composition."
    return 0
  fi

  restore_fork_owned_paths "${pre_merge_head}"
  git add -A
  if ! git commit \
    -m "chore(upstream-sync): preview ${phase} merge" \
    -m "Auto-resolved a blocked ${phase} merge to make the sync branch inspectable. The tracking issue lists conflict files and ownership guidance before this can land."; then
    echo "[!] Failed to create blocked preview commit; aborting merge state."
    git merge --abort 2>/dev/null || true
  fi
}

cmd_record_state_v2() {
  local plan_file=$1
  local sync_id fingerprint base_fork_commit
  local original_repository original_tag original_commit
  local plus_repository plus_tag plus_tag_commit plus_head_commit plus_head_included
  local models_repository models_commit expected_fork_tag candidate_branch

  sync_id=$(require_plan_value "${plan_file}" safe_sync_id)
  fingerprint=$(require_plan_value "${plan_file}" plan_fingerprint)
  base_fork_commit=$(require_plan_value "${plan_file}" base_fork_commit)
  original_repository=$(require_plan_value "${plan_file}" original_repository)
  original_tag=$(require_plan_value "${plan_file}" original_tag)
  original_commit=$(require_plan_value "${plan_file}" original_head)
  plus_repository=$(require_plan_value "${plan_file}" plus_repository)
  plus_tag=$(require_plan_value "${plan_file}" plus_tag)
  plus_tag_commit=$(require_plan_value "${plan_file}" plus_tag_head)
  plus_head_commit=$(require_plan_value "${plan_file}" plus_head)
  plus_head_included=$(require_plan_value "${plan_file}" plus_head_included)
  models_repository=$(require_plan_value "${plan_file}" models_repository)
  models_commit=$(require_plan_value "${plan_file}" models_commit)
  expected_fork_tag=$(require_plan_value "${plan_file}" expected_fork_tag)
  candidate_branch=$(require_plan_value "${plan_file}" candidate_branch)

  [ "$(git branch --show-current)" = "${candidate_branch}" ] \
    || die "record-state must run on candidate branch ${candidate_branch}"

  {
    echo "SCHEMA_VERSION=2"
    echo "SYNC_ID=${sync_id}"
    echo "PLAN_FINGERPRINT=${fingerprint}"
    echo "BASE_FORK_COMMIT=${base_fork_commit}"
    echo "ORIGINAL_REPOSITORY=${original_repository}"
    echo "ORIGINAL_TAG=${original_tag}"
    echo "ORIGINAL_COMMIT=${original_commit}"
    echo "PLUS_REPOSITORY=${plus_repository}"
    echo "PLUS_TAG=${plus_tag}"
    echo "PLUS_TAG_COMMIT=${plus_tag_commit}"
    echo "PLUS_HEAD_COMMIT=${plus_head_commit}"
    echo "PLUS_HEAD_INCLUDED=${plus_head_included}"
    echo "MODELS_REPOSITORY=${models_repository}"
    echo "MODELS_COMMIT=${models_commit}"
    echo "EXPECTED_FORK_TAG=${expected_fork_tag}"
    echo "CANDIDATE_BRANCH=${candidate_branch}"
  } > .ccs-fork-upstream.env

  git add .ccs-fork-upstream.env
  if ! git diff --cached --quiet -- .ccs-fork-upstream.env; then
    git commit \
      -m "Record upstream sync candidate state" \
      -m "Commit the exact upstream, Plus, and model-catalog snapshot before validation so promotion can reuse this candidate SHA without a post-validation state change."
  fi
}

cmd_record_state() {
  local plan_file=${1:-}
  if [ -n "${plan_file}" ]; then
    [ -f "${plan_file}" ] || die "plan output not found: ${plan_file}"
    cmd_record_state_v2 "${plan_file}"
    return 0
  fi

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

cmd_check_freshness() {
  local plan_file=${1:-}
  local allow_fork_base_drift=${UPSTREAM_SYNC_ALLOW_FORK_BASE_DRIFT:-false}
  [ -n "${plan_file}" ] || die "check-freshness requires plan-output-file"
  [ -f "${plan_file}" ] || die "plan output not found: ${plan_file}"
  case "${allow_fork_base_drift}" in
    true|false) ;;
    *) die "UPSTREAM_SYNC_ALLOW_FORK_BASE_DRIFT must be true or false" ;;
  esac

  local original_tag plus_tag
  local expected_original expected_plus_tag expected_plus_head expected_models expected_fork_main
  local actual_original actual_plus_tag actual_plus_head actual_models actual_fork_main
  original_tag=$(require_plan_value "${plan_file}" original_tag)
  plus_tag=$(require_plan_value "${plan_file}" plus_tag)
  expected_original=$(require_plan_value "${plan_file}" original_head)
  expected_plus_tag=$(require_plan_value "${plan_file}" plus_tag_head)
  expected_plus_head=$(require_plan_value "${plan_file}" plus_head)
  expected_models=$(require_plan_value "${plan_file}" models_commit)
  expected_fork_main=$(require_plan_value "${plan_file}" base_fork_commit)

  actual_original=$(remote_ref_commit "${ORIGINAL_REMOTE}" "refs/tags/${original_tag}" || true)
  actual_plus_tag=$(remote_ref_commit "${PLUS_REMOTE}" "refs/tags/${plus_tag}" || true)
  actual_plus_head=$(remote_ref_commit "${PLUS_REMOTE}" refs/heads/main || true)
  actual_models=$(remote_ref_commit "${MODELS_REMOTE}" "refs/heads/${MODELS_BRANCH}" || true)
  actual_fork_main=$(remote_ref_commit "${ORIGIN_REMOTE}" "refs/heads/${FORK_BRANCH}" || true)

  local reasons=""
  [ "${actual_original}" = "${expected_original}" ] || reasons="${reasons}original-tag-moved"$'\n'
  [ "${actual_plus_tag}" = "${expected_plus_tag}" ] || reasons="${reasons}plus-tag-moved"$'\n'
  [ "${actual_plus_head}" = "${expected_plus_head}" ] || reasons="${reasons}plus-head-moved"$'\n'
  [ "${actual_models}" = "${expected_models}" ] || reasons="${reasons}models-head-moved"$'\n'
  if [ "${allow_fork_base_drift}" != true ] && [ "${actual_fork_main}" != "${expected_fork_main}" ]; then
    reasons="${reasons}fork-main-moved"$'\n'
  fi

  write_kv actual_original_commit "${actual_original}"
  write_kv actual_plus_tag_commit "${actual_plus_tag}"
  write_kv actual_plus_head_commit "${actual_plus_head}"
  write_kv actual_models_commit "${actual_models}"
  write_kv actual_fork_main_commit "${actual_fork_main}"

  if [ -n "${reasons}" ]; then
    write_kv fresh false
    write_kv stale_reasons "$(printf '%s' "${reasons}" | join_csv)"
    return 1
  fi

  write_kv fresh true
  write_kv stale_reasons ""
}

provenance_contains_path() {
  local file=$1
  local path=$2
  grep -Fxq -- "${path}" "${file}"
}

append_provenance_source() {
  local current=$1
  local source=$2
  if [ -n "${current}" ]; then
    printf '%s,%s\n' "${current}" "${source}"
  else
    printf '%s\n' "${source}"
  fi
}

cmd_report_provenance() {
  require_ownership_manifest

  local plan_file=${1:-}
  local candidate_ref=${2:-}
  [ -n "${plan_file}" ] || die "report-provenance requires plan-output-file"
  [ -f "${plan_file}" ] || die "plan output not found: ${plan_file}"
  [ -n "${candidate_ref}" ] || die "report-provenance requires candidate-ref"
  git rev-parse --verify "${candidate_ref}^{commit}" >/dev/null 2>&1 \
    || die "candidate ref not found: ${candidate_ref}"

  local base_fork_commit original_commit plus_tag_commit plus_head_commit plus_head_included plus_selected
  base_fork_commit=$(require_plan_value "${plan_file}" base_fork_commit)
  original_commit=$(require_plan_value "${plan_file}" original_head)
  plus_tag_commit=$(require_plan_value "${plan_file}" plus_tag_head)
  plus_head_commit=$(require_plan_value "${plan_file}" plus_head)
  plus_head_included=$(require_plan_value "${plan_file}" plus_head_included)
  plus_selected=${plus_tag_commit}
  if [ "${plus_head_included}" = true ]; then
    plus_selected=${plus_head_commit}
  fi

  local root original_paths plus_paths original_delta_paths plus_delta_paths
  local fork_paths candidate_paths manual_paths all_paths
  local shared_base original_base plus_base conflict_files unsafe_plus_head_paths
  root=$(mktemp -d)
  PROVENANCE_TEMP_ROOT=${root}
  trap 'if [ -n "${PROVENANCE_TEMP_ROOT:-}" ]; then rm -rf "${PROVENANCE_TEMP_ROOT}"; fi' EXIT
  original_paths="${root}/original.paths"
  plus_paths="${root}/plus.paths"
  original_delta_paths="${root}/original-delta.paths"
  plus_delta_paths="${root}/plus-delta.paths"
  fork_paths="${root}/fork.paths"
  candidate_paths="${root}/candidate.paths"
  manual_paths="${root}/manual.paths"
  all_paths="${root}/all.paths"
  shared_base=$(git merge-base "${original_commit}" "${plus_selected}")
  original_base=$(git merge-base "${base_fork_commit}" "${original_commit}")
  plus_base=$(git merge-base "${base_fork_commit}" "${plus_selected}")

  git -c diff.renames=false diff --name-only "${shared_base}" "${original_commit}" > "${original_paths}"
  git -c diff.renames=false diff --name-only "${shared_base}" "${plus_selected}" > "${plus_paths}"
  git -c diff.renames=false diff --name-only "${original_base}" "${original_commit}" > "${original_delta_paths}"
  git -c diff.renames=false diff --name-only "${plus_base}" "${plus_selected}" > "${plus_delta_paths}"
  git rev-list --no-merges "${base_fork_commit}" --not "${original_commit}" "${plus_selected}" \
    | while IFS= read -r commit; do
        [ -n "${commit}" ] || continue
        git -c diff.renames=false diff-tree --root --no-commit-id --name-only -r "${commit}"
      done \
    | sort -u > "${fork_paths}"
  git -c diff.renames=false diff --name-only "${base_fork_commit}" "${candidate_ref}" > "${candidate_paths}"
  conflict_files=$(phase_output_value "${plan_file}" conflict_files)
  unsafe_plus_head_paths=$(phase_output_value "${plan_file}" unsafe_plus_head_delta_paths)
  printf '%s\n%s\n' "${conflict_files}" "${unsafe_plus_head_paths}" \
    | tr ',' '\n' \
    | sed '/^[[:space:]]*$/d' \
    | sort -u > "${manual_paths}"
  sort -u \
    "${original_delta_paths}" \
    "${plus_delta_paths}" \
    "${candidate_paths}" \
    "${manual_paths}" > "${all_paths}"

  local report_dir tsv markdown
  report_dir=${UPSTREAM_SYNC_REPORT_DIR:-/tmp/upstream-sync-report}
  mkdir -p "${report_dir}"
  tsv="${report_dir%/}/provenance.tsv"
  markdown="${report_dir%/}/provenance.md"
  printf 'path\towner\tprovenance\taction\tmanual_composition\n' > "${tsv}"

  local path owner provenance action manual
  local original_changed plus_changed fork_changed candidate_changed
  local original_delta plus_delta manual_path
  local manual_required=false
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    owner=$(classify_path "${path}")
    provenance=""
    original_changed=false
    plus_changed=false
    fork_changed=false
    candidate_changed=false
    original_delta=false
    plus_delta=false
    manual_path=false

    if provenance_contains_path "${original_paths}" "${path}"; then
      provenance=$(append_provenance_source "${provenance}" original)
      original_changed=true
    fi
    if provenance_contains_path "${plus_paths}" "${path}"; then
      provenance=$(append_provenance_source "${provenance}" plus)
      plus_changed=true
    fi
    if provenance_contains_path "${fork_paths}" "${path}"; then
      provenance=$(append_provenance_source "${provenance}" fork)
      fork_changed=true
    fi
    if provenance_contains_path "${candidate_paths}" "${path}"; then
      candidate_changed=true
      if [ -z "${provenance}" ]; then
        provenance=candidate
      fi
    fi
    if provenance_contains_path "${original_delta_paths}" "${path}"; then
      original_delta=true
    fi
    if provenance_contains_path "${plus_delta_paths}" "${path}"; then
      plus_delta=true
    fi
    if provenance_contains_path "${manual_paths}" "${path}"; then
      manual_path=true
    fi
    [ -n "${provenance}" ] || provenance=unchanged

    action="verify-candidate"
    manual=false
    case "${owner}" in
      fork-owned)
        action="preserve-fork"
        ;;
      plus-owned)
        action="accept-plus"
        ;;
      *)
        if [ "${manual_path}" = true ] \
          || { [ "${original_delta}" = true ] && { [ "${plus_changed}" = true ] || [ "${fork_changed}" = true ]; }; } \
          || { [ "${plus_delta}" = true ] && { [ "${original_changed}" = true ] || [ "${fork_changed}" = true ]; }; }; then
          action="manual-compose"
          manual=true
          manual_required=true
        elif [ "${original_delta}" = true ]; then
          action="review-original-update"
        elif [ "${plus_delta}" = true ]; then
          action="inherited-plus"
        elif [ "${fork_changed}" = true ]; then
          action="preserve-fork"
        elif [ "${candidate_changed}" = true ]; then
          action="verify-candidate"
        fi
        ;;
    esac

    printf '%s\t%s\t%s\t%s\t%s\n' \
      "${path}" "${owner}" "${provenance}" "${action}" "${manual}" >> "${tsv}"
  done < "${all_paths}"

  {
    echo '# Upstream sync provenance'
    echo
    if [ "${manual_required}" = true ]; then
      echo 'Manual composition required: **yes**'
    else
      echo 'Manual composition required: **no**'
    fi
    echo
    echo '| Path | Static owner | Actual provenance | Recommended action | Manual |'
    echo '|---|---|---|---|---|'
    tail -n +2 "${tsv}" \
      | while IFS=$'\t' read -r path owner provenance action manual; do
          # shellcheck disable=SC2016
          printf '| `%s` | `%s` | `%s` | `%s` | `%s` |\n' \
            "${path}" "${owner}" "${provenance}" "${action}" "${manual}"
        done
  } > "${markdown}"

  write_kv provenance_tsv "${tsv}"
  write_kv provenance_markdown "${markdown}"
  write_kv manual_composition_required "${manual_required}"

  rm -rf "${root}"
  PROVENANCE_TEMP_ROOT=""
  trap - EXIT
}

cmd_replay_plan() {
  require_ownership_manifest

  local root replay_dir validator
  root=$(mktemp -d)
  REPLAY_TEMP_ROOT=${root}
  trap 'if [ -n "${REPLAY_TEMP_ROOT:-}" ]; then rm -rf "${REPLAY_TEMP_ROOT}"; fi' EXIT
  replay_dir="${root}/repo"
  validator="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/validate-upstream-sync.sh"
  git clone -q "$(pwd)" "${replay_dir}"

  local remote_name remote_url
  for remote_name in "${ORIGIN_REMOTE}" "${ORIGINAL_REMOTE}" "${PLUS_REMOTE}" "${MODELS_REMOTE}"; do
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
    local plan_out materialize_out validation_dir validation_env
    plan_out="${root}/plan.out"
    materialize_out="${root}/materialize.out"
    validation_dir="${root}/validation"
    validation_env="${validation_dir}/validation.env"

    FORCE_REBUILD="${FORCE_REBUILD:-false}" GITHUB_OUTPUT="${plan_out}" "${BASH_SOURCE[0]}" plan >/dev/null

    local original_tag plus_tag plus_head plus_tag_head plus_head_included
    original_tag=$(phase_output_value "${plan_out}" original_tag)
    plus_tag=$(phase_output_value "${plan_out}" plus_tag)
    plus_tag_head=$(phase_output_value "${plan_out}" plus_tag_head)
    plus_head=$(phase_output_value "${plan_out}" plus_head)
    plus_head_included=$(phase_output_value "${plan_out}" plus_head_included)

    printf 'Original tag: %s\n' "${original_tag}"
    printf 'Plus tag: %s\n' "${plus_tag}"

    GITHUB_OUTPUT="${materialize_out}" "${BASH_SOURCE[0]}" materialize "${plan_out}" >/dev/null
    printf 'Original merge: conflicts=%s\n' "$(phase_output_value "${materialize_out}" original_conflicts)"
    phase_output_value "${materialize_out}" original_conflict_files
    printf 'Plus release overlay: conflicts=%s\n' "$(phase_output_value "${materialize_out}" plus_tag_conflicts)"
    phase_output_value "${materialize_out}" plus_tag_conflict_files

    if [ "${plus_head_included}" = true ] && [ "${plus_head}" != "${plus_tag_head}" ]; then
      printf 'Plus head delta: conflicts=%s\n' "$(phase_output_value "${materialize_out}" plus_head_conflicts)"
      phase_output_value "${materialize_out}" plus_head_conflict_files
    else
      printf 'Plus head delta: skipped\n'
    fi

    "${BASH_SOURCE[0]}" record-state "${plan_out}" >/dev/null

    local validation_status=0
    set +e
    UPSTREAM_SYNC_BUILD_CMD="${UPSTREAM_SYNC_BUILD_CMD:-${UPSTREAM_SYNC_REPLAY_BUILD_CMD:-go build -o test-output ./cmd/server}}" \
      UPSTREAM_SYNC_TEST_CMD="${UPSTREAM_SYNC_TEST_CMD:-${UPSTREAM_SYNC_REPLAY_TEST_CMD:-go test -count=1 -timeout=10m ./...}}" \
      "${validator}" --mode full --plan "${plan_out}" --report-dir "${validation_dir}"
    validation_status=$?
    set -e

    printf 'Invariant status: %s\n' "$(phase_output_value "${validation_env}" INVARIANTS_STATUS)"
    printf 'Symbol survival status: %s\n' "$(phase_output_value "${validation_env}" SYMBOL_SURVIVAL_STATUS)"
    printf 'Build status: %s\n' "$(phase_output_value "${validation_env}" BUILD_STATUS)"
    printf 'Test status: %s\n' "$(phase_output_value "${validation_env}" TESTS_STATUS)"
    return "${validation_status}"
  )

  rm -rf "${root}"
  REPLAY_TEMP_ROOT=""
  trap - EXIT
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

shared_hotspot_go_paths_at_ref() {
  local ref=$1
  local path class

  git ls-tree -r --name-only "${ref}" -- internal sdk cmd 2>/dev/null \
    | while IFS= read -r path; do
        [ -n "${path}" ] || continue
        [[ "${path}" == *.go ]] || continue
        class=$(classify_path "${path}")
        case "${class}" in
          plus-owned|fork-owned) ;;
          *) printf '%s\n' "${path}" ;;
        esac
      done
}

extract_go_symbols_from_stream() {
  awk '
    /^func[[:space:]]+\(/ {
      line = $0
      sub(/^func[[:space:]]*\(/, "", line)
      recv = line
      sub(/\).*/, "", recv)
      n = split(recv, recv_parts, /[[:space:]]+/)
      receiver = recv_parts[n]
      sub(/^\*/, "", receiver)

      name = $0
      sub(/^func[[:space:]]*\([^)]*\)[[:space:]]*/, "", name)
      sub(/\(.*/, "", name)
      if (receiver != "" && name != "") {
        print receiver "." name
      }
      next
    }
    /^func[[:space:]]+[A-Za-z_][A-Za-z0-9_]*/ {
      name = $0
      sub(/^func[[:space:]]+/, "", name)
      sub(/[[:space:]\[\(].*/, "", name)
      if (name != "") {
        print name
      }
      next
    }
    /^type[[:space:]]+[A-Za-z_][A-Za-z0-9_]*/ {
      name = $0
      sub(/^type[[:space:]]+/, "", name)
      split(name, parts, /[[:space:]]+/)
      sub(/\[.*/, "", parts[1])
      if (parts[1] != "" && parts[1] != "(") {
        print parts[1]
      }
    }
  '
}

extract_shared_hotspot_symbols_from_ref() {
  local ref=$1
  local path

  shared_hotspot_go_paths_at_ref "${ref}" \
    | while IFS= read -r path; do
        [ -n "${path}" ] || continue
        git show "${ref}:${path}" 2>/dev/null || true
        printf '\n'
      done \
    | extract_go_symbols_from_stream \
    | sort -u
}

extract_shared_hotspot_test_symbols_from_ref() {
  local ref=$1
  local path

  shared_hotspot_go_paths_at_ref "${ref}" \
    | while IFS= read -r path; do
        [ -n "${path}" ] || continue
        [[ "${path}" == *_test.go ]] || continue
        git show "${ref}:${path}" 2>/dev/null || true
        printf '\n'
      done \
    | extract_go_symbols_from_stream \
    | awk '/^Test[A-Za-z0-9_]*$/ { print }' \
    | sort -u
}

extract_worktree_go_symbols() {
  local path

  git ls-files -- internal sdk cmd 2>/dev/null \
    | while IFS= read -r path; do
        [ -f "${path}" ] || continue
        [[ "${path}" == *.go ]] || continue
        cat "${path}"
        printf '\n'
      done \
    | extract_go_symbols_from_stream \
    | sort -u
}

allowlisted_dropped_symbol_reason() {
  local symbol=$1
  local configured_file=${DROPPED_SYMBOLS_FILE}
  local resolved_file=""
  local reason

  if [ -f "${configured_file}" ]; then
    resolved_file=${configured_file}
  elif [ -f "$(dirname -- "${OWNERSHIP_FILE}")/$(basename -- "${configured_file}")" ]; then
    resolved_file="$(dirname -- "${OWNERSHIP_FILE}")/$(basename -- "${configured_file}")"
  fi
  [ -n "${resolved_file}" ] || return 1

  reason=$(awk -F'\t' -v symbol="${symbol}" '
    $1 == symbol { sub(/^[^\t]*\t?/, ""); print; found = 1; exit }
    END { if (!found) exit 1 }
  ' "${resolved_file}") || return 1
  printf '%s\n' "${reason:-allowlisted}"
}

default_symbol_survival_upstream_ref() {
  local recorded
  recorded=$(recorded_state_value ORIGINAL_COMMIT)
  if [ -n "${recorded}" ]; then
    printf '%s\n' "${recorded}"
  else
    printf 'refs/remotes/%s/main\n' "${ORIGINAL_REMOTE}"
  fi
}

cmd_check_symbol_survival() {
  require_ownership_manifest

  local baseline_ref=${1:-}
  local upstream_ref=${2:-}
  [ -n "${baseline_ref}" ] || die "check-symbol-survival requires baseline-ref"
  if [ -z "${upstream_ref}" ]; then
    upstream_ref=$(default_symbol_survival_upstream_ref)
  fi

  local root baseline_symbols upstream_symbols overlay_symbols current_symbols
  local baseline_tests upstream_tests overlay_tests missing_tests
  local symbol reason failed skipped
  root=$(mktemp -d)
  baseline_symbols="${root}/baseline-symbols.txt"
  upstream_symbols="${root}/upstream-symbols.txt"
  overlay_symbols="${root}/overlay-symbols.txt"
  current_symbols="${root}/current-symbols.txt"
  baseline_tests="${root}/baseline-tests.txt"
  upstream_tests="${root}/upstream-tests.txt"
  overlay_tests="${root}/overlay-tests.txt"
  missing_tests="${root}/missing-tests.txt"

  extract_shared_hotspot_symbols_from_ref "${baseline_ref}" > "${baseline_symbols}"
  extract_shared_hotspot_symbols_from_ref "${upstream_ref}" > "${upstream_symbols}"
  comm -23 "${baseline_symbols}" "${upstream_symbols}" > "${overlay_symbols}"
  extract_worktree_go_symbols > "${current_symbols}"

  extract_shared_hotspot_test_symbols_from_ref "${baseline_ref}" > "${baseline_tests}"
  extract_shared_hotspot_test_symbols_from_ref "${upstream_ref}" > "${upstream_tests}"
  comm -23 "${baseline_tests}" "${upstream_tests}" > "${overlay_tests}"

  failed=false
  skipped=false
  : > "${missing_tests}"

  while IFS= read -r symbol; do
    [ -n "${symbol}" ] || continue
    if grep -Fxq -- "${symbol}" "${current_symbols}"; then
      continue
    fi
    if reason=$(allowlisted_dropped_symbol_reason "${symbol}"); then
      printf '[SKIP] dropped overlay symbol allowlisted: %s — %s\n' "${symbol}" "${reason}"
      skipped=true
      continue
    fi
    printf '[FAIL] missing overlay symbol: %s\n' "${symbol}"
    failed=true
  done < "${overlay_symbols}"

  while IFS= read -r symbol; do
    [ -n "${symbol}" ] || continue
    grep -Fxq -- "${symbol}" "${current_symbols}" && continue
    allowlisted_dropped_symbol_reason "${symbol}" >/dev/null && continue
    printf '%s\n' "${symbol}" >> "${missing_tests}"
  done < "${overlay_tests}"

  if [ -s "${missing_tests}" ]; then
    printf '\nDELETED FORK TESTS\n'
    while IFS= read -r symbol; do
      [ -n "${symbol}" ] || continue
      printf '[FAIL] deleted fork test: %s\n' "${symbol}"
    done < "${missing_tests}"
  fi

  if [ "${failed}" = true ]; then
    rm -rf "${root}"
    return 1
  fi
  if [ "${skipped}" = true ]; then
    printf '[OK] symbol-survival gate passed with allowlisted removals.\n'
  else
    printf '[OK] symbol-survival gate passed.\n'
  fi
  rm -rf "${root}"
}

cmd_pending_overlay_branch() {
  printf '%s\n' "${PENDING_OVERLAY_BRANCH}"
}

cmd_check_invariants() {
  local file=${INVARIANTS_FILE}
  local feature check path pattern description first second third fourth fifth
  local failed=0

  [ -f "${file}" ] || die "invariants file not found: ${file}"
  while IFS=$'\t' read -r first second third fourth fifth; do
    [[ -n "${first}" && "${first}" != \#* ]] || continue
    if [ "${first}" = contains ]; then
      feature=legacy
      check=${first}
      path=${second}
      pattern=${third}
      description=${fourth}
    else
      feature=${first}
      check=${second}
      path=${third}
      pattern=${fourth}
      description=${fifth}
    fi
    case "${check}" in
      contains)
        if [ ! -f "${path}" ]; then
          echo "[FAIL] invariant [${feature}]: ${description:-${path}} (${path} missing)" >&2
          failed=1
        elif ! grep -Fq -- "${pattern}" "${path}"; then
          echo "[FAIL] invariant [${feature}]: ${description:-${path}} (${path} missing pattern: ${pattern})" >&2
          failed=1
        else
          echo "[OK] invariant [${feature}]: ${description:-${path}}"
        fi
        ;;
      *)
        echo "[FAIL] invariant [${feature}]: unsupported check ${check} for ${path}" >&2
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
    materialize) cmd_materialize "$@" ;;
    merge-ref) cmd_merge_ref "$@" ;;
    replay-plan) cmd_replay_plan "$@" ;;
    record-state) cmd_record_state "$@" ;;
    check-freshness) cmd_check_freshness "$@" ;;
    report-provenance) cmd_report_provenance "$@" ;;
    classify-paths) cmd_classify_paths "$@" ;;
    check-symbol-survival) cmd_check_symbol_survival "$@" ;;
    check-invariants) cmd_check_invariants "$@" ;;
    pending-overlay-branch) cmd_pending_overlay_branch "$@" ;;
    *) die "usage: $0 {plan|materialize|merge-ref|replay-plan|record-state|check-freshness|report-provenance|classify-paths|check-symbol-survival|check-invariants|pending-overlay-branch}" ;;
  esac
}

main "$@"
