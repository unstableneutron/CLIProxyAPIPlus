#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
HELPER="${SCRIPT_DIR}/upstream-sync-workflow.sh"
CLEANUP_ROOT=""

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

assert_contains() {
  local path=$1 text=$2
  grep -Fq -- "${text}" "${path}" || fail "expected ${path} to contain: ${text}"
}

main() {
  local root output validation report sha
  root=$(mktemp -d)
  CLEANUP_ROOT=${root}
  trap 'rm -rf "${CLEANUP_ROOT}"' EXIT
  output=${root}/output
  validation=${root}/validation.env
  report=${root}/candidate.md
  sha=$(git rev-parse HEAD)
  printf 'candidate evidence\n' > "${report}"

  printf 'OVERALL_STATUS=failed\n' > "${validation}"
  GITHUB_OUTPUT="${output}" \
    BLOCKED=true CONFLICTS=false FRESH=true MANUAL_COMPOSITION=false REPAIR_VALIDATED=false \
    "${HELPER}" classify-candidate "${validation}" "${report}"
  assert_contains "${output}" "acceptable=false"
  assert_contains "${output}" "outcome=needs-manual-action"
  assert_contains "${output}" "candidate_sha=${sha}"

  : > "${output}"
  printf 'OVERALL_STATUS=passed\n' > "${validation}"
  GITHUB_OUTPUT="${output}" \
    BLOCKED=false CONFLICTS=false FRESH=true MANUAL_COMPOSITION=false REPAIR_VALIDATED=false \
    "${HELPER}" classify-candidate "${validation}" "${report}"
  assert_contains "${output}" "acceptable=true"
  assert_contains "${output}" "outcome=candidate-validated"

  mkdir -p "${root}/bin"
  cat > "${root}/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}:${2:-}" = release:view ]; then
  exit 0
fi
if [ "${1:-}" = api ]; then
  if [[ "${2:-}" == */commits/* ]]; then
    printf '%s\n' "${STUB_SHA}"
    exit 0
  fi
  printf '%s\n' "$*" >> "${STUB_API_LOG}"
  cat <<'JSON'
[[
  {"number":66,"head":{"ref":"upstream-sync/old-one","repo":{"full_name":"unstableneutron/CLIProxyAPIPlus"}},"base":{"ref":"main"}},
  {"number":90,"head":{"ref":"upstream-sync/old-two","repo":{"full_name":"unstableneutron/CLIProxyAPIPlus"}},"base":{"ref":"main"}},
  {"number":77,"head":{"ref":"upstream-sync/foreign","repo":{"full_name":"other/fork"}},"base":{"ref":"main"}},
  {"number":78,"head":{"ref":"upstream-sync/wrong-base","repo":{"full_name":"unstableneutron/CLIProxyAPIPlus"}},"base":{"ref":"dev"}}
]]
JSON
  exit 0
fi
if [ "${1:-}:${2:-}" = release:upload ]; then
  cp "${4}" "${STUB_UPLOADED_RECEIPT}"
  exit 0
fi
if [ "${1:-}:${2:-}" = release:download ]; then
  [ "${STUB_FAIL_DOWNLOAD:-}" != true ] || exit 1
  output_dir=""
  pattern=""
  while [ "$#" -gt 0 ]; do
    if [ "$1" = --dir ]; then
      output_dir=$2
      shift 2
      continue
    fi
    if [ "$1" = --pattern ]; then
      pattern=$2
      shift 2
      continue
    fi
    shift
  done
  [ -n "${output_dir}" ] && [ -n "${pattern}" ]
  cp "${STUB_UPLOADED_RECEIPT}" "${output_dir}/${pattern}"
  exit 0
fi
if [ "${1:-}:${2:-}" = pr:close ]; then
  printf '%s\n' "$*" >> "${STUB_CLOSE_LOG}"
  if [ "${STUB_FAIL_CLOSE:-}" = "${3:-}" ]; then exit 1; fi
  exit 0
fi
echo "unexpected gh arguments: $*" >&2
exit 2
EOF
  chmod +x "${root}/bin/gh"
  cat > "${root}/bin/mktemp" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = -d ]
mkdir -p "${STUB_TEMP_DIR}"
printf '%s\n' "${STUB_TEMP_DIR}"
EOF
  chmod +x "${root}/bin/mktemp"
  PATH="${root}/bin:${PATH}" \
    GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
    STUB_CLOSE_LOG="${root}/closed" \
    STUB_API_LOG="${root}/api-calls" \
    "${HELPER}" close-superseded-prs \
      v7.2.142-unstableneutron.0 "${sha}" https://example.invalid/run >/dev/null
  [ "$(wc -l < "${root}/closed" | tr -d ' ')" = 2 ] || fail "did not close both superseded PRs"
  assert_contains "${root}/closed" "pr close 66"
  assert_contains "${root}/closed" "pr close 90"
  assert_contains "${root}/closed" "v7.2.142-unstableneutron.0"
  assert_contains "${root}/api-calls" "--paginate"
  assert_contains "${root}/api-calls" "per_page=100"
  assert_contains "${root}/api-calls" "--slurp"
  if grep -Eq 'pr close (77|78)' "${root}/closed"; then
    fail "cleanup closed a foreign-repository or non-main PR"
  fi

  : > "${root}/closed"
  if PATH="${root}/bin:${PATH}" \
    GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
    STUB_CLOSE_LOG="${root}/closed" \
    STUB_FAIL_CLOSE=66 \
    STUB_API_LOG="${root}/api-calls" \
    "${HELPER}" close-superseded-prs \
      v7.2.142-unstableneutron.0 "${sha}" https://example.invalid/run >/dev/null 2>&1; then
    fail "partial PR cleanup unexpectedly succeeded"
  fi
  [ "$(wc -l < "${root}/closed" | tr -d ' ')" = 2 ] \
    || fail "cleanup stopped after the first close failure"
  assert_contains "${root}/closed" "pr close 90"

  receipt=${root}/upstream-sync-receipt.json
  printf '{"schema_version":3}\n' > "${receipt}"
  if ! PATH="${root}/bin:${PATH}" \
    GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
    STUB_SHA="${sha}" \
    STUB_TEMP_DIR="${root}/receipt-download" \
    STUB_UPLOADED_RECEIPT="${root}/uploaded-upstream-sync-receipt.json" \
    "${HELPER}" attach-receipt \
      v7.2.143-unstableneutron.0 "${sha}" exact "${receipt}"; then
    fail "receipt attachment failed after publishing matching bytes"
  fi
  [ ! -e "${root}/receipt-download" ] \
    || fail "receipt attachment left its temporary download directory"

  if PATH="${root}/bin:${PATH}" \
    GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
    STUB_FAIL_DOWNLOAD=true \
    STUB_SHA="${sha}" \
    STUB_TEMP_DIR="${root}/failed-receipt-download" \
    STUB_UPLOADED_RECEIPT="${root}/uploaded-upstream-sync-receipt.json" \
    "${HELPER}" attach-receipt \
      v7.2.143-unstableneutron.0 "${sha}" exact "${receipt}"; then
    fail "receipt attachment unexpectedly survived a download failure"
  fi
  [ ! -e "${root}/failed-receipt-download" ] \
    || fail "failed receipt attachment left its temporary download directory"

  echo "[OK] upstream-sync workflow helper tests passed"
}

main "$@"
