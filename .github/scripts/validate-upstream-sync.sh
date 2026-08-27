#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
HELPER="${SCRIPT_DIR}/upstream-sync.sh"
ACTIONLINT_VERSION=${UPSTREAM_SYNC_ACTIONLINT_VERSION:-v1.7.12}


die() {
  echo "[upstream-sync-validation] $*" >&2
  exit 1
}

plan_value() {
  local file=$1
  local key=$2
  awk -F= -v key="${key}" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "${file}"
}

require_plan_value() {
  local file=$1
  local key=$2
  local value
  value=$(plan_value "${file}" "${key}")
  [ -n "${value}" ] || die "plan is missing required field: ${key}"
  printf '%s\n' "${value}"
}

run_gate() {
  local name=$1
  local command=$2
  local status_variable=$3
  local log_file="${REPORT_DIR}/${name}.log"
  local exit_code

  set +e
  bash -c "${command}" > "${log_file}" 2>&1
  exit_code=$?
  set -e

  if [ "${exit_code}" -eq 0 ]; then
    printf -v "${status_variable}" '%s' passed
    printf '[OK] %s\n' "${name}"
  else
    printf -v "${status_variable}" '%s' failed
    printf '[FAIL] %s (see %s)\n' "${name}" "${log_file}" >&2
  fi
}

skip_gate() {
  local name=$1
  local status_variable=$2
  printf 'skipped\n' > "${REPORT_DIR}/${name}.log"
  printf -v "${status_variable}" '%s' skipped
}

tooling_changed_since() {
  local base_ref=$1
  local path

  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    case "${path}" in
      .amp/plugins/release-published-webhook/*|\
      .agents/skills/validating-upstream-sync/*|\
      .github/release-asset-contract.json|\
      .github/scripts/*|\
      .github/upstream-sync-*.tsv|\
      .github/workflows-disabled/*|\
      .github/workflows/*|\
      .goreleaser.yml|\
      README-ccs-fork.md)
        return 0
        ;;
    esac
  done < <(
    {
      git diff --name-only "${base_ref}" HEAD
      git diff --name-only
      git diff --cached --name-only
    } | sort -u
  )
  return 1
}

MODE=""
PLAN_FILE=""
REPORT_DIR=""
TOOLING_MODE=${UPSTREAM_SYNC_TOOLING_MODE:-auto}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --mode)
      [ "$#" -ge 2 ] || die "--mode requires a value"
      MODE=$2
      shift 2
      ;;
    --plan)
      [ "$#" -ge 2 ] || die "--plan requires a value"
      PLAN_FILE=$2
      shift 2
      ;;
    --report-dir)
      [ "$#" -ge 2 ] || die "--report-dir requires a value"
      REPORT_DIR=$2
      shift 2
      ;;
    --tooling)
      [ "$#" -ge 2 ] || die "--tooling requires a value"
      TOOLING_MODE=$2
      shift 2
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

case "${MODE}" in
  quick|full|tooling) ;;
  *) die "--mode must be quick, full, or tooling" ;;
esac
case "${TOOLING_MODE}" in
  auto|always|never) ;;
  *) die "--tooling must be auto, always, or never" ;;
esac
[ -n "${REPORT_DIR}" ] || die "--report-dir is required"
mkdir -p "${REPORT_DIR}"

BASE_FORK_COMMIT=""
ORIGINAL_COMMIT=""
PLAN_FINGERPRINT=""
CANDIDATE_SHA=$(git rev-parse HEAD)
if [ "${MODE}" != tooling ]; then
  [ -n "${PLAN_FILE}" ] || die "--plan is required for ${MODE} mode"
  [ -f "${PLAN_FILE}" ] || die "plan file not found: ${PLAN_FILE}"
  BASE_FORK_COMMIT=$(require_plan_value "${PLAN_FILE}" base_fork_commit)
  ORIGINAL_COMMIT=$(require_plan_value "${PLAN_FILE}" original_head)
  PLAN_FINGERPRINT=$(require_plan_value "${PLAN_FILE}" plan_fingerprint)
fi

INVARIANTS_STATUS=skipped
SYMBOL_SURVIVAL_STATUS=skipped
BUILD_STATUS=skipped
TESTS_STATUS=skipped
DOCTOR_STATUS=skipped
HELPER_TESTS_STATUS=skipped
SHELLCHECK_STATUS=skipped
ACTIONLINT_STATUS=skipped
WEBHOOK_TESTS_STATUS=skipped
TOOLING_REQUIRED=false

if [ "${MODE}" != tooling ]; then
  run_gate invariants \
    "${UPSTREAM_SYNC_INVARIANT_CMD:-\"${HELPER}\" check-invariants}" \
    INVARIANTS_STATUS
  run_gate symbol-survival \
    "${UPSTREAM_SYNC_SYMBOL_CMD:-UPSTREAM_SYNC_PLAN_FINGERPRINT=\"${PLAN_FINGERPRINT}\" \"${HELPER}\" check-symbol-survival \"${BASE_FORK_COMMIT}\" \"${ORIGINAL_COMMIT}\"}" \
    SYMBOL_SURVIVAL_STATUS
else
  skip_gate invariants INVARIANTS_STATUS
  skip_gate symbol-survival SYMBOL_SURVIVAL_STATUS
fi

if [ "${MODE}" = full ]; then
  run_gate build \
    "${UPSTREAM_SYNC_BUILD_CMD:-go build -o test-output ./cmd/server}" \
    BUILD_STATUS
  rm -f test-output
  run_gate tests \
    "${UPSTREAM_SYNC_TEST_CMD:-go test -count=1 -timeout=10m ./...}" \
    TESTS_STATUS
else
  skip_gate build BUILD_STATUS
  skip_gate tests TESTS_STATUS
fi

case "${MODE}:${TOOLING_MODE}" in
  tooling:*) TOOLING_REQUIRED=true ;;
  full:always) TOOLING_REQUIRED=true ;;
  full:never) TOOLING_REQUIRED=false ;;
  full:auto)
    if tooling_changed_since "${BASE_FORK_COMMIT}"; then
      TOOLING_REQUIRED=true
    fi
    ;;
esac

if [ "${TOOLING_REQUIRED}" = true ]; then
  run_gate doctor \
    "${UPSTREAM_SYNC_DOCTOR_CMD:-\"${SCRIPT_DIR}/check-upstream-sync-tools.sh\"}" \
    DOCTOR_STATUS
  run_gate helper-tests \
    "${UPSTREAM_SYNC_HELPER_TEST_CMD:-UPSTREAM_SYNC_TOOLING_MODE=auto \"${SCRIPT_DIR}/test-upstream-sync.sh\" && UPSTREAM_SYNC_TOOLING_MODE=auto \"${SCRIPT_DIR}/test-verify-upstream-release.sh\" && \"${SCRIPT_DIR}/test-hotfix-release.sh\" && \"${SCRIPT_DIR}/test-verify-hotfix-chain.sh\" && \"${SCRIPT_DIR}/test-verify-hotfix-release.sh\" && \"${SCRIPT_DIR}/test-publish-staged-release.sh\" && \"${SCRIPT_DIR}/test-select-docker-digest-evidence.sh\" && \"${SCRIPT_DIR}/test-revalidate-release-target.sh\" && \"${SCRIPT_DIR}/test-upstream-sync-workflow.sh\" && \"${SCRIPT_DIR}/test-upstream-sync-dispatch-plan.sh\"}" \
    HELPER_TESTS_STATUS
  run_gate shellcheck \
    "${UPSTREAM_SYNC_SHELLCHECK_CMD:-shellcheck .github/scripts/*.sh}" \
    SHELLCHECK_STATUS
  run_gate actionlint \
    "${UPSTREAM_SYNC_ACTIONLINT_CMD:-go run github.com/rhysd/actionlint/cmd/actionlint@${ACTIONLINT_VERSION}}" \
    ACTIONLINT_STATUS
  run_gate webhook-tests \
    "${UPSTREAM_SYNC_WEBHOOK_TEST_CMD:-bun test ./.amp/plugins/release-published-webhook}" \
    WEBHOOK_TESTS_STATUS
else
  skip_gate doctor DOCTOR_STATUS
  skip_gate helper-tests HELPER_TESTS_STATUS
  skip_gate shellcheck SHELLCHECK_STATUS
  skip_gate actionlint ACTIONLINT_STATUS
  skip_gate webhook-tests WEBHOOK_TESTS_STATUS
fi

OVERALL_STATUS=passed
for status in \
  "${INVARIANTS_STATUS}" \
  "${SYMBOL_SURVIVAL_STATUS}" \
  "${BUILD_STATUS}" \
  "${TESTS_STATUS}" \
  "${DOCTOR_STATUS}" \
  "${HELPER_TESTS_STATUS}" \
  "${SHELLCHECK_STATUS}" \
  "${ACTIONLINT_STATUS}" \
  "${WEBHOOK_TESTS_STATUS}"; do
  if [ "${status}" = failed ]; then
    OVERALL_STATUS=failed
  fi
done

cat > "${REPORT_DIR}/validation.env" <<EOF
VALIDATION_SCHEMA_VERSION=1
MODE=${MODE}
PLAN_FINGERPRINT=${PLAN_FINGERPRINT}
CANDIDATE_SHA=${CANDIDATE_SHA}
OVERALL_STATUS=${OVERALL_STATUS}
TOOLING_REQUIRED=${TOOLING_REQUIRED}
INVARIANTS_STATUS=${INVARIANTS_STATUS}
SYMBOL_SURVIVAL_STATUS=${SYMBOL_SURVIVAL_STATUS}
BUILD_STATUS=${BUILD_STATUS}
TESTS_STATUS=${TESTS_STATUS}
DOCTOR_STATUS=${DOCTOR_STATUS}
HELPER_TESTS_STATUS=${HELPER_TESTS_STATUS}
SHELLCHECK_STATUS=${SHELLCHECK_STATUS}
ACTIONLINT_STATUS=${ACTIONLINT_STATUS}
WEBHOOK_TESTS_STATUS=${WEBHOOK_TESTS_STATUS}
EOF

jq -n \
  --arg mode "${MODE}" \
  --arg plan_fingerprint "${PLAN_FINGERPRINT}" \
  --arg candidate_sha "${CANDIDATE_SHA}" \
  --arg overall_status "${OVERALL_STATUS}" \
  --argjson tooling_required "${TOOLING_REQUIRED}" \
  --arg invariants_status "${INVARIANTS_STATUS}" \
  --arg symbol_survival_status "${SYMBOL_SURVIVAL_STATUS}" \
  --arg build_status "${BUILD_STATUS}" \
  --arg tests_status "${TESTS_STATUS}" \
  --arg doctor_status "${DOCTOR_STATUS}" \
  --arg helper_tests_status "${HELPER_TESTS_STATUS}" \
  --arg shellcheck_status "${SHELLCHECK_STATUS}" \
  --arg actionlint_status "${ACTIONLINT_STATUS}" \
  --arg webhook_tests_status "${WEBHOOK_TESTS_STATUS}" \
  '{
    schema_version: 1,
    mode: $mode,
    plan_fingerprint: $plan_fingerprint,
    candidate_sha: $candidate_sha,
    overall_status: $overall_status,
    tooling_required: $tooling_required,
    gates: {
      invariants: $invariants_status,
      symbol_survival: $symbol_survival_status,
      build: $build_status,
      tests: $tests_status,
      doctor: $doctor_status,
      helper_tests: $helper_tests_status,
      shellcheck: $shellcheck_status,
      actionlint: $actionlint_status,
      webhook_tests: $webhook_tests_status
    }
  }' > "${REPORT_DIR}/validation.json"

[ "${OVERALL_STATUS}" = passed ]
