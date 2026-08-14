#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
VERIFIER="${SCRIPT_DIR}/verify-hotfix-release.sh"
FIXTURES="${SCRIPT_DIR}/testdata/upstream-release"
BASE_TAG=v7.2.131-unstableneutron.0
TAG=v7.2.131-unstableneutron.1
SYNC_ID=original-v7.2.131_plus-v7.2.127-3
FINGERPRINT=eeef3819ca9dfb38b4528fc5dabc3324d538b19b
IMAGE=ghcr.io/unstableneutron/cli-proxy-api-plus

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

run_git() {
  git -c init.defaultBranch=main "$@"
}

assert_contains() {
  local file=$1
  local expected=$2
  grep -Fq -- "${expected}" "${file}" \
    || fail "expected ${file} to contain: ${expected}"
}

setup_repo() {
  local root=$1
  local repo=${root}/repo
  mkdir -p "${repo}"
  run_git -C "${repo}" init -q
  run_git -C "${repo}" config user.name verifier-test
  run_git -C "${repo}" config user.email verifier-test@example.invalid
  cat > "${repo}/.ccs-fork-upstream.env" <<EOF
SCHEMA_VERSION=2
SYNC_ID=${SYNC_ID}
PLAN_FINGERPRINT=${FINGERPRINT}
EXPECTED_FORK_TAG=${BASE_TAG}
EOF
  echo base > "${repo}/app.txt"
  run_git -C "${repo}" add .
  run_git -C "${repo}" commit -m base >/dev/null
  run_git -C "${repo}" tag -a "${BASE_TAG}" -m "Release ${BASE_TAG}"
  echo fixed > "${repo}/app.txt"
  run_git -C "${repo}" commit -am hotfix >/dev/null
  run_git -C "${repo}" tag -a "${TAG}" -m "Release ${TAG}"
}

make_fixtures() {
  local root=$1
  local archive=CLIProxyAPIPlus_7.2.131-unstableneutron.1_linux_amd64_no-plugin.tar.gz
  printf 'archive fixture\n' > "${root}/${archive}"
  local archive_sha
  archive_sha=$(sha256sum "${root}/${archive}" | awk '{ print $1 }')
  printf '%s  %s\n' "${archive_sha}" "${archive}" > "${root}/checksums.txt"
  local checksums_sha
  checksums_sha=$(sha256sum "${root}/checksums.txt" | awk '{ print $1 }')
  jq -n \
    --arg tag "${TAG}" \
    --arg archive "${archive}" \
    '{
      url: ("https://github.com/unstableneutron/CLIProxyAPIPlus/releases/tag/" + $tag),
      isDraft: false,
      isPrerelease: false,
      assets: [{name: "checksums.txt"}, {name: $archive}]
    }' > "${root}/release.json"
  jq -n \
    --arg archive "${archive}" \
    --arg archive_digest "sha256:${archive_sha}" \
    --arg checksums_digest "sha256:${checksums_sha}" \
    '{assets: [
      {name: "checksums.txt", digest: $checksums_digest},
      {name: $archive, digest: $archive_digest}
    ]}' > "${root}/release-api.json"
}

make_stubs() {
  local root=$1
  mkdir -p "${root}/bin"
  cat > "${root}/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}:${2:-}" in
  api:*commits/main)
    printf '%s\n' "${STUB_HOTFIX_COMMIT}"
    ;;
  api:*commits/*unstableneutron.0)
    printf '%s\n' "${STUB_BASE_COMMIT}"
    ;;
  api:*commits/*unstableneutron.1)
    printf '%s\n' "${STUB_HOTFIX_COMMIT}"
    ;;
  api:*compare/*)
    printf '%s\n' "${STUB_COMPARE_STATUS:-ahead}"
    ;;
  api:*releases/tags/*)
    cat "${STUB_RELEASE_API_JSON}"
    ;;
  release:view)
    cat "${STUB_RELEASE_JSON}"
    ;;
  release:download)
    destination=""
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --dir ]; then
        destination=$2
        break
      fi
      shift
    done
    [ -n "${destination}" ]
    mkdir -p "${destination}"
    cp "${STUB_CHECKSUM_FILE}" "${destination}/checksums.txt"
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
  *-amd64) cat "${STUB_AMD64_IMAGE_JSON}" ;;
  *-arm64) cat "${STUB_ARM64_IMAGE_JSON}" ;;
  *) cat "${STUB_IMAGE_INDEX_JSON}" ;;
esac
EOF
  chmod +x "${root}/bin/gh" "${root}/bin/docker"
}

run_verifier() {
  local root=$1
  local receipt=$2
  local attached_receipt=${3:-}
  local release_api=${4:-${root}/release-api.json}
  local image_index=${5:-${FIXTURES}/image-index.json}
  local base_commit hotfix_commit
  base_commit=$(run_git -C "${root}/repo" rev-parse "${BASE_TAG}^{}")
  hotfix_commit=$(run_git -C "${root}/repo" rev-parse "${TAG}^{}")
  local attached_args=()
  if [ -n "${attached_receipt}" ]; then
    attached_args=(--attached-receipt "${attached_receipt}")
  fi
  (
    cd "${root}/repo"
    PATH="${root}/bin:${PATH}" \
      GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
      GITHUB_RUN_ID=123456789 \
      GITHUB_RUN_ATTEMPT=1 \
      GITHUB_WORKFLOW_REF=unstableneutron/CLIProxyAPIPlus/.github/workflows/hotfix-release.yml@refs/heads/main \
      STUB_BASE_COMMIT="${base_commit}" \
      STUB_HOTFIX_COMMIT="${hotfix_commit}" \
      STUB_RELEASE_JSON="${root}/release.json" \
      STUB_RELEASE_API_JSON="${release_api}" \
      STUB_CHECKSUM_FILE="${root}/checksums.txt" \
      STUB_IMAGE_INDEX_JSON="${image_index}" \
      STUB_LATEST_INDEX_JSON="${image_index}" \
      STUB_AMD64_IMAGE_JSON="${FIXTURES}/image-amd64.json" \
      STUB_ARM64_IMAGE_JSON="${FIXTURES}/image-arm64.json" \
      "${VERIFIER}" \
        --tag "${TAG}" \
        --expected-commit "${hotfix_commit}" \
        --base-tag "${BASE_TAG}" \
        --expected-base-commit "${base_commit}" \
        --expected-sync-id "${SYNC_ID}" \
        --expected-plan-fingerprint "${FINGERPRINT}" \
        --image "${IMAGE}" \
        --main-policy exact \
        --require-latest-parity true \
        "${attached_args[@]}" \
        --receipt "${receipt}"
  )
}

expect_failure() {
  local root=$1
  local expected=$2
  shift 2
  local output=${root}/failure.log
  rm -f "${output}"
  if run_verifier "${root}" "$@" > "${output}" 2>&1; then
    fail "hotfix verifier unexpectedly succeeded"
  fi
  assert_contains "${output}" "${expected}"
}

test_receipt_binds_release_and_upstream_state() {
  local root
  root=$(mktemp -d)
  setup_repo "${root}"
  make_fixtures "${root}"
  make_stubs "${root}"
  local receipt=${root}/receipt.json
  run_verifier "${root}" "${receipt}" >/dev/null
  local base_commit hotfix_commit state_sha
  base_commit=$(run_git -C "${root}/repo" rev-parse "${BASE_TAG}^{}")
  hotfix_commit=$(run_git -C "${root}/repo" rev-parse "${TAG}^{}")
  state_sha=$(sha256sum "${root}/repo/.ccs-fork-upstream.env" | awk '{ print $1 }')
  jq -e \
    --arg base_commit "${base_commit}" \
    --arg hotfix_commit "${hotfix_commit}" \
    --arg state_sha "${state_sha}" '
      .receipt_type == "hotfix-release" and
      .hotfix_schema_version == 1 and
      .main_commit == $hotfix_commit and
      .tag_commit == $hotfix_commit and
      .previous_release.tag == "v7.2.131-unstableneutron.0" and
      .previous_release.commit == $base_commit and
      .upstream_state.sha256 == $state_sha and
      (.release_asset_digests["checksums.txt"] | startswith("sha256:")) and
      .architecture_images["linux/amd64"].digest == "sha256:1111111111111111111111111111111111111111111111111111111111111111" and
      .architecture_images["linux/arm64"].digest == "sha256:2222222222222222222222222222222222222222222222222222222222222222" and
      .release_workflow.run_id == "123456789"
    ' "${receipt}" >/dev/null || fail "hotfix receipt omitted required identity"

  run_verifier "${root}" "${root}/verified.json" "${receipt}" >/dev/null
  cmp -s "${receipt}" "${root}/verified.json" \
    || fail "attached receipt verification changed a valid receipt"

  printf '{not json\n' > "${root}/malformed.json"
  expect_failure \
    "${root}" "attached hotfix receipt is not valid JSON" \
    "${root}/malformed-output.json" "${root}/malformed.json"
  jq '.image_digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"' \
    "${receipt}" > "${root}/mismatched-receipt.json"
  expect_failure \
    "${root}" "attached hotfix receipt does not match independent verification" \
    "${root}/mismatched-output.json" "${root}/mismatched-receipt.json"

  jq '.assets[1].digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"' \
    "${root}/release-api.json" > "${root}/wrong-asset.json"
  expect_failure \
    "${root}" "checksum for" \
    "${root}/asset-output.json" "" "${root}/wrong-asset.json"

  jq 'del(.manifests[] | select(.platform.architecture == "arm64"))' \
    "${FIXTURES}/image-index.json" > "${root}/missing-platform.json"
  expect_failure \
    "${root}" "missing linux/arm64" \
    "${root}/platform-output.json" "" "${root}/release-api.json" \
    "${root}/missing-platform.json"

  rm -rf "${root}"
}

main() {
  [ -x "${VERIFIER}" ] || fail "verifier is missing or not executable"
  test_receipt_binds_release_and_upstream_state
  echo "[OK] hotfix release verifier tests passed"
}

main "$@"
