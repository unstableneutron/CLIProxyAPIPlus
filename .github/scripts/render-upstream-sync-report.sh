#!/usr/bin/env bash
# shellcheck disable=SC2016 # Markdown code spans intentionally use backticks.
set -euo pipefail

die() {
  echo "[upstream-sync-report] $*" >&2
  exit 1
}

output_value() {
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

require_value() {
  local file=$1
  local key=$2
  local value
  value=$(output_value "${file}" "${key}")
  [ -n "${value}" ] || die "${file} is missing required field: ${key}"
  printf '%s\n' "${value}"
}

PLAN_FILE=""
VALIDATION_FILE=""
PROVENANCE_FILE=""
OUTPUT_FILE=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --plan)
      [ "$#" -ge 2 ] || die "--plan requires a value"
      PLAN_FILE=$2
      shift 2
      ;;
    --validation)
      [ "$#" -ge 2 ] || die "--validation requires a value"
      VALIDATION_FILE=$2
      shift 2
      ;;
    --provenance)
      [ "$#" -ge 2 ] || die "--provenance requires a value"
      PROVENANCE_FILE=$2
      shift 2
      ;;
    --output)
      [ "$#" -ge 2 ] || die "--output requires a value"
      OUTPUT_FILE=$2
      shift 2
      ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ -f "${PLAN_FILE}" ] || die "plan file not found: ${PLAN_FILE}"
[ -f "${VALIDATION_FILE}" ] || die "validation file not found: ${VALIDATION_FILE}"
[ -f "${PROVENANCE_FILE}" ] || die "provenance file not found: ${PROVENANCE_FILE}"
[ -n "${OUTPUT_FILE}" ] || die "--output is required"

SYNC_ID=$(require_value "${PLAN_FILE}" safe_sync_id)
BASE_FORK_COMMIT=$(require_value "${PLAN_FILE}" base_fork_commit)
ORIGINAL_TAG=$(require_value "${PLAN_FILE}" original_tag)
ORIGINAL_COMMIT=$(require_value "${PLAN_FILE}" original_head)
PLUS_TAG=$(require_value "${PLAN_FILE}" plus_tag)
PLUS_TAG_COMMIT=$(require_value "${PLAN_FILE}" plus_tag_head)
PLUS_HEAD_COMMIT=$(require_value "${PLAN_FILE}" plus_head)
PLUS_HEAD_INCLUDED=$(require_value "${PLAN_FILE}" plus_head_included)
MODELS_COMMIT=$(require_value "${PLAN_FILE}" models_commit)
PLAN_FINGERPRINT=$(require_value "${PLAN_FILE}" plan_fingerprint)
CANDIDATE_BRANCH=$(require_value "${PLAN_FILE}" candidate_branch)
EXPECTED_FORK_TAG=$(require_value "${PLAN_FILE}" expected_fork_tag)

FRESH=$(output_value "${PLAN_FILE}" fresh)
STALE_REASONS=$(output_value "${PLAN_FILE}" stale_reasons)
CONFLICTS=$(output_value "${PLAN_FILE}" conflicts)
CONFLICT_FILES=$(output_value "${PLAN_FILE}" conflict_files)
WORKFLOW_URL=${UPSTREAM_SYNC_WORKFLOW_URL:-$(output_value "${PLAN_FILE}" workflow_url)}
[ -n "${FRESH}" ] || FRESH=unknown
[ -n "${CONFLICTS}" ] || CONFLICTS=false

OVERALL_STATUS=$(require_value "${VALIDATION_FILE}" OVERALL_STATUS)
INVARIANTS_STATUS=$(require_value "${VALIDATION_FILE}" INVARIANTS_STATUS)
SYMBOL_SURVIVAL_STATUS=$(require_value "${VALIDATION_FILE}" SYMBOL_SURVIVAL_STATUS)
BUILD_STATUS=$(require_value "${VALIDATION_FILE}" BUILD_STATUS)
TESTS_STATUS=$(require_value "${VALIDATION_FILE}" TESTS_STATUS)
HELPER_TESTS_STATUS=$(require_value "${VALIDATION_FILE}" HELPER_TESTS_STATUS)
SHELLCHECK_STATUS=$(require_value "${VALIDATION_FILE}" SHELLCHECK_STATUS)
ACTIONLINT_STATUS=$(require_value "${VALIDATION_FILE}" ACTIONLINT_STATUS)

MANUAL_COMPOSITION_REQUIRED=false
if awk -F'\t' 'NR > 1 && $5 == "true" { found = 1 } END { exit !found }' "${PROVENANCE_FILE}"; then
  MANUAL_COMPOSITION_REQUIRED=true
fi

mkdir -p "$(dirname -- "${OUTPUT_FILE}")"
{
  echo '# Upstream sync candidate'
  echo
  printf -- '- Sync ID: `%s`\n' "${SYNC_ID}"
  printf -- '- Candidate branch: `%s`\n' "${CANDIDATE_BRANCH}"
  printf -- '- Expected fork tag: `%s`\n' "${EXPECTED_FORK_TAG}"
  printf -- '- Plan fingerprint: `%s`\n' "${PLAN_FINGERPRINT}"
  if [ -n "${WORKFLOW_URL}" ]; then
    printf -- '- Workflow: [%s](%s)\n' "${WORKFLOW_URL}" "${WORKFLOW_URL}"
  else
    echo '- Workflow: None'
  fi
  echo
  echo '## Exact snapshot'
  echo
  echo '| Source | Ref | Commit |'
  echo '|---|---|---|'
  printf '| Fork base | `main` | `%s` |\n' "${BASE_FORK_COMMIT}"
  printf '| Original | `%s` | `%s` |\n' "${ORIGINAL_TAG}" "${ORIGINAL_COMMIT}"
  printf '| Plus release | `%s` | `%s` |\n' "${PLUS_TAG}" "${PLUS_TAG_COMMIT}"
  printf '| Plus head (included: `%s`) | `main` | `%s` |\n' "${PLUS_HEAD_INCLUDED}" "${PLUS_HEAD_COMMIT}"
  printf '| Models | `main` | `%s` |\n' "${MODELS_COMMIT}"
  echo
  echo '## Freshness and conflicts'
  echo
  printf -- '- Fresh: **%s**\n' "${FRESH}"
  if [ -n "${STALE_REASONS}" ]; then
    printf -- '- Stale reasons: `%s`\n' "${STALE_REASONS}"
  else
    echo '- Stale reasons: None'
  fi
  if [ "${CONFLICTS}" = true ] || [ -n "${CONFLICT_FILES}" ]; then
    echo '- Conflicts:'
    printf '%s\n' "${CONFLICT_FILES}" \
      | tr ',' '\n' \
      | while IFS= read -r path; do
          [ -n "${path}" ] || continue
          printf '  - `%s`\n' "${path}"
        done
  else
    echo '- Conflicts: **None**'
  fi
  echo
  echo '## Provenance guidance'
  echo
  if [ "${MANUAL_COMPOSITION_REQUIRED}" = true ]; then
    echo 'Manual composition required: **yes**'
  else
    echo 'Manual composition required: **no**'
  fi
  echo
  echo '| Path | Static owner | Actual provenance | Recommended action | Manual |'
  echo '|---|---|---|---|---|'
  tail -n +2 "${PROVENANCE_FILE}" \
    | while IFS=$'\t' read -r path owner provenance action manual; do
        # shellcheck disable=SC2016
        printf '| `%s` | `%s` | `%s` | `%s` | `%s` |\n' \
          "${path}" "${owner}" "${provenance}" "${action}" "${manual}"
      done
  echo
  echo '## Validation'
  echo
  printf 'Overall status: **%s**\n\n' "${OVERALL_STATUS}"
  echo '| Gate | Status |'
  echo '|---|---|'
  printf '| Invariants | `%s` |\n' "${INVARIANTS_STATUS}"
  printf '| Symbol survival | `%s` |\n' "${SYMBOL_SURVIVAL_STATUS}"
  printf '| Build | `%s` |\n' "${BUILD_STATUS}"
  printf '| Tests | `%s` |\n' "${TESTS_STATUS}"
  printf '| Helper tests | `%s` |\n' "${HELPER_TESTS_STATUS}"
  printf '| ShellCheck | `%s` |\n' "${SHELLCHECK_STATUS}"
  printf '| actionlint | `%s` |\n' "${ACTIONLINT_STATUS}"
} > "${OUTPUT_FILE}"
