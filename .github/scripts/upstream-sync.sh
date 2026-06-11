#!/usr/bin/env bash
set -euo pipefail

ORIGIN_REMOTE=${ORIGIN_REMOTE:-origin}
ORIGINAL_REMOTE=${ORIGINAL_REMOTE:-original-upstream}
PLUS_REMOTE=${PLUS_REMOTE:-plus-upstream}
ORIGINAL_REPOSITORY=${ORIGINAL_REPOSITORY:-router-for-me/CLIProxyAPI}
PLUS_REPOSITORY=${PLUS_REPOSITORY:-kaitranntt/CLIProxyAPIPlus}
TRACKING_ISSUE_LABEL=${TRACKING_ISSUE_LABEL:-upstream-sync-blocked}

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
  for prefix in "${PLUS_OWNED_PREFIXES[@]}"; do
    [[ "${path}" == "${prefix}"* ]] && return 0
  done
  return 1
}

is_fork_owned_path() {
  local path=$1
  local owned
  for owned in "${FORK_OWNED_PATHS[@]}"; do
    [[ "${path}" == "${owned}" ]] && return 0
  done
  for owned in "${FORK_OWNED_PREFIXES[@]}"; do
    [[ "${path}" == "${owned}"* ]] && return 0
  done
  return 1
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

  git add -A -- "${FORK_OWNED_PATHS[@]}" "${FORK_OWNED_PREFIXES[@]}" 2>/dev/null || true
}

cmd_plan() {
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
  write_kv has_changes "${has_changes}"

  echo "[i] original ${original_tag} (${original_commit})"
  echo "[i] plus tag ${plus_tag} (${plus_tag_commit}); plus head ${plus_head_commit}; include_head=${plus_head_included}"
  echo "[i] next fork tag ${next_fork_tag}; has_changes=${has_changes}; blocked=${blocked}${block_reason:+ (${block_reason})}"
}

cmd_merge_ref() {
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
  local key
  key=$(phase_key "${phase}")

  if [ -z "${unmerged}" ] && [ ${merge_exit} -eq 0 ]; then
    restore_fork_owned_paths "${pre_merge_head}"
    if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
      git commit \
        -m "chore(upstream-sync): merge ${phase} ref" \
        -m "Automated upstream-sync merge for ${phase}: ${ref}. Fork-owned files are restored from the fork side before committing."
    fi
    write_kv conflicts false
    write_env "${key}_CONFLICT_FILES" ""
    write_env "${key}_CONFLICT_TABLE" ""
    echo "[OK] ${phase} merge completed without conflicts."
    return 0
  fi

  write_kv conflicts true
  write_kv conflict_files "${unmerged}"
  write_kv conflict_table "$(classify_paths_table "${unmerged}")"
  write_env "${key}_CONFLICT_FILES" "${unmerged}"
  write_env "${key}_CONFLICT_TABLE" "$(classify_paths_table "${unmerged}")"

  if [ -z "${unmerged}" ]; then
    echo "[!] ${phase} merge failed without unmerged files; leaving branch for inspection."
    return 0
  fi

  echo "[!] ${phase} merge conflicts detected; creating blocked preview commit."
  echo "${unmerged}"
  local path side
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    side=$(preferred_conflict_side "${phase}" "${path}")
    checkout_conflict_side "${side}" "${ref}" "${path}"
  done <<< "${unmerged}"

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

cmd_classify_paths() {
  local paths=${1:-}
  if [ -n "${paths}" ]; then
    classify_paths_table "${paths}"
  else
    classify_paths_table "$(cat)"
  fi
}

main() {
  local cmd=${1:-}
  shift || true
  case "${cmd}" in
    plan) cmd_plan "$@" ;;
    merge-ref) cmd_merge_ref "$@" ;;
    record-state) cmd_record_state "$@" ;;
    classify-paths) cmd_classify_paths "$@" ;;
    *) die "usage: $0 {plan|merge-ref|record-state|classify-paths}" ;;
  esac
}

main "$@"
