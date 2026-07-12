#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
VERIFIER="${SCRIPT_DIR}/verify-upstream-release.sh"
FIXTURES="${SCRIPT_DIR}/testdata/upstream-release"
TAG=v7.2.67-unstableneutron.0
COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
FINGERPRINT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
SYNC_ID=original-v7.2.67_plus-v7.2.62-5
IMAGE=ghcr.io/unstableneutron/cli-proxy-api-plus

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

assert_contains() {
  local file=$1
  local expected=$2
  grep -Fq -- "${expected}" "${file}" || fail "expected ${file} to contain: ${expected}"
}

make_stubs() {
  local root=$1
  mkdir -p "${root}/bin"
  cat > "${root}/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}:${2:-}" in
  api:*commits/main)
    printf '%s\n' "${STUB_MAIN_COMMIT}"
    ;;
  api:*commits/*)
    printf '%s\n' "${STUB_TAG_COMMIT}"
    ;;
  api:*compare/*)
    printf '%s\n' "${STUB_COMPARE_STATUS}"
    ;;
  release:view)
    cat "${STUB_RELEASE_JSON}"
    ;;
  *)
    echo "unexpected gh arguments: $*" >&2
    exit 2
    ;;
esac
EOF
  cat > "${root}/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != buildx ] || [ "${2:-}" != imagetools ] || [ "${3:-}" != inspect ]; then
  echo "unexpected docker arguments: $*" >&2
  exit 2
fi
case "${4:-}" in
  *:latest) cat "${STUB_LATEST_INDEX_JSON}" ;;
  *) cat "${STUB_IMAGE_INDEX_JSON}" ;;
esac
EOF
  chmod +x "${root}/bin/gh" "${root}/bin/docker"
}

run_verifier() {
  local root=$1
  local receipt=$2
  local require_latest=$3
  local main_commit=${4:-${COMMIT}}
  local tag_commit=${5:-${COMMIT}}
  local release_json=${6:-${FIXTURES}/release.json}
  local image_json=${7:-${FIXTURES}/image-index.json}
  local latest_json=${8:-${image_json}}
  local main_policy=${9:-exact}
  local compare_status=${10:-ahead}

  PATH="${root}/bin:${PATH}" \
    GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
    GITHUB_RUN_ID=123456789 \
    STUB_MAIN_COMMIT="${main_commit}" \
    STUB_TAG_COMMIT="${tag_commit}" \
    STUB_RELEASE_JSON="${release_json}" \
    STUB_IMAGE_INDEX_JSON="${image_json}" \
    STUB_LATEST_INDEX_JSON="${latest_json}" \
    STUB_COMPARE_STATUS="${compare_status}" \
    "${VERIFIER}" \
      --tag "${TAG}" \
      --expected-commit "${COMMIT}" \
      --expected-sync-id "${SYNC_ID}" \
      --expected-plan-fingerprint "${FINGERPRINT}" \
      --image "${IMAGE}" \
      --main-policy "${main_policy}" \
      --require-latest-parity "${require_latest}" \
      --receipt "${receipt}"
}

test_allows_verified_main_descendant_when_requested() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  local descendant=cccccccccccccccccccccccccccccccccccccccc
  local receipt=${root}/descendant.json

  run_verifier \
    "${root}" "${receipt}" false \
    "${descendant}" "${COMMIT}" "${FIXTURES}/release.json" \
    "${FIXTURES}/image-index.json" "${FIXTURES}/image-index.json" \
    descendant ahead
  jq -e --arg commit "${COMMIT}" \
    '.main_commit == $commit and .tag_commit == $commit' \
    "${receipt}" >/dev/null || fail "descendant verification rewrote release identity"

  expect_failure \
    "${root}" "${root}/diverged.json" "does not descend from" \
    false "${descendant}" "${COMMIT}" "${FIXTURES}/release.json" \
    "${FIXTURES}/image-index.json" "${FIXTURES}/image-index.json" \
    descendant diverged
  rm -rf "${root}"
}

expect_failure() {
  local root=$1
  local receipt=$2
  local expected=$3
  shift 3
  local output=${root}/failure.log
  if run_verifier "${root}" "${receipt}" "$@" > "${output}" 2>&1; then
    fail "verifier unexpectedly succeeded"
  fi
  [ ! -e "${receipt}" ] || fail "failed verification wrote a receipt"
  assert_contains "${output}" "${expected}"
}

test_writes_receipt_after_success() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  local receipt=${root}/receipt.json

  run_verifier "${root}" "${receipt}" true

  jq -e \
    --arg commit "${COMMIT}" \
    --arg fingerprint "${FINGERPRINT}" \
    --arg sync_id "${SYNC_ID}" \
    --arg tag "${TAG}" \
    '.schema_version == 1 and
     .main_commit == $commit and
     .tag_commit == $commit and
     .plan_fingerprint == $fingerprint and
     .sync_id == $sync_id and
     .tag == $tag and
     .image_digest == "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" and
     .platforms == ["linux/amd64", "linux/arm64"] and
     .workflow_run_id == "123456789"' \
    "${receipt}" >/dev/null || fail "receipt did not contain the verified identity"
  rm -rf "${root}"
}

test_rejects_main_or_tag_mismatch() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  expect_failure \
    "${root}" "${root}/receipt.json" "main resolves" \
    false cccccccccccccccccccccccccccccccccccccccc
  expect_failure \
    "${root}" "${root}/receipt-tag.json" "Tag ${TAG} resolves" \
    false "${COMMIT}" dddddddddddddddddddddddddddddddddddddddd
  rm -rf "${root}"
}

test_rejects_wrong_release_branding() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  jq '.assets = [{"name":"checksums.txt"},{"name":"CLIProxyAPI_v7.2.67_linux_amd64.tar.gz"}]' \
    "${FIXTURES}/release.json" > "${root}/wrong-brand.json"
  expect_failure \
    "${root}" "${root}/receipt.json" "CLIProxyAPIPlus-branded" \
    false "${COMMIT}" "${COMMIT}" "${root}/wrong-brand.json"
  rm -rf "${root}"
}

test_rejects_missing_required_platforms() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  jq 'del(.manifests[] | select(.platform.architecture == "amd64"))' \
    "${FIXTURES}/image-index.json" > "${root}/missing-amd64.json"
  expect_failure \
    "${root}" "${root}/receipt-amd64.json" "linux/amd64" \
    false "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" "${root}/missing-amd64.json"

  jq 'del(.manifests[] | select(.platform.architecture == "arm64"))' \
    "${FIXTURES}/image-index.json" > "${root}/missing-arm64.json"
  expect_failure \
    "${root}" "${root}/receipt-arm64.json" "linux/arm64" \
    false "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" "${root}/missing-arm64.json"
  rm -rf "${root}"
}

test_latest_parity_is_conditional() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  jq '.digest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"' \
    "${FIXTURES}/image-index.json" > "${root}/latest.json"

  run_verifier \
    "${root}" "${root}/without-latest.json" false \
    "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" \
    "${FIXTURES}/image-index.json" "${root}/latest.json"
  expect_failure \
    "${root}" "${root}/with-latest.json" "latest digest" \
    true "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" \
    "${FIXTURES}/image-index.json" "${root}/latest.json"
  rm -rf "${root}"
}

main() {
  [ -x "${VERIFIER}" ] || fail "verifier is missing or not executable: ${VERIFIER}"
  test_writes_receipt_after_success
  test_rejects_main_or_tag_mismatch
  test_allows_verified_main_descendant_when_requested
  test_rejects_wrong_release_branding
  test_rejects_missing_required_platforms
  test_latest_parity_is_conditional
  echo "[OK] upstream release verifier tests passed"
}

main "$@"
