#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
VERIFIER="${SCRIPT_DIR}/verify-hotfix-release.sh"
FIXTURES="${SCRIPT_DIR}/testdata/upstream-release"
# shellcheck source=/dev/null
source "${SCRIPT_DIR}/release-assets.sh"
BASE_TAG=v7.2.131-unstableneutron.0
TAG=v7.2.131-unstableneutron.1
ORIGINAL_SOURCE_TAG=v7.2.131
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
ORIGINAL_TAG=${ORIGINAL_SOURCE_TAG}
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
  local expected_assets archive archive_sha assets='[]'
  expected_assets=$(expected_release_assets_json "${TAG}") \
    || fail "could not derive expected release assets"
  : > "${root}/checksums.txt"
  while IFS= read -r archive; do
    [ "${archive}" != checksums.txt ] || continue
    printf 'archive fixture for %s\n' "${archive}" > "${root}/${archive}"
    archive_sha=$(sha256sum "${root}/${archive}" | awk '{ print $1 }')
    printf '%s  %s\n' "${archive_sha}" "${archive}" >> "${root}/checksums.txt"
    assets=$(jq -c \
      --arg name "${archive}" \
      --arg digest "sha256:${archive_sha}" \
      '. + [{name: $name, digest: $digest}]' <<< "${assets}")
  done < <(jq -r '.[]' <<< "${expected_assets}")
  local checksums_sha
  checksums_sha=$(sha256sum "${root}/checksums.txt" | awk '{ print $1 }')
  assets=$(jq -c \
    --arg digest "sha256:${checksums_sha}" \
    '. + [{name: "checksums.txt", digest: $digest}] | sort_by(.name)' <<< "${assets}")
  jq -n \
    --arg tag "${TAG}" \
    --argjson assets "${assets}" \
    '{
      url: ("https://github.com/unstableneutron/CLIProxyAPIPlus/releases/tag/" + $tag),
      isDraft: false,
      isPrerelease: false,
      assets: [$assets[].name | {name: .}]
    }' > "${root}/release.json"
  jq -n \
    --argjson assets "${assets}" \
    '{assets: $assets}' > "${root}/release-api.json"
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
  api:*commits/${STUB_BASE_TAG})
    printf '%s\n' "${STUB_BASE_COMMIT}"
    ;;
  api:*commits/${STUB_HOTFIX_TAG})
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
  local current_attempt=${6:-1}
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
      GITHUB_RUN_ATTEMPT="${current_attempt}" \
      GITHUB_WORKFLOW_REF=unstableneutron/CLIProxyAPIPlus/.github/workflows/hotfix-release.yml@refs/heads/main \
      STUB_BASE_COMMIT="${base_commit}" \
      STUB_BASE_TAG="${BASE_TAG}" \
      STUB_HOTFIX_COMMIT="${hotfix_commit}" \
      STUB_HOTFIX_TAG="${TAG}" \
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
  run_verifier \
    "${root}" "${root}/recovered.json" "${receipt}" \
    "${root}/release-api.json" "${FIXTURES}/image-index.json" 2 >/dev/null
  cmp -s "${receipt}" "${root}/recovered.json" \
    || fail "later attempt did not preserve immutable receipt provenance"

  jq '.workflow_run_id = "987654321" | .release_workflow.run_id = "987654321"' \
    "${receipt}" > "${root}/wrong-run.json"
  expect_failure \
    "${root}" "attached receipt is not from this workflow run" \
    "${root}/wrong-run-output.json" "${root}/wrong-run.json"

  printf '{not json\n' > "${root}/malformed.json"
  expect_failure \
    "${root}" "attached hotfix receipt is not valid JSON" \
    "${root}/malformed-output.json" "${root}/malformed.json"
  jq '.image_digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"' \
    "${receipt}" > "${root}/mismatched-receipt.json"
  expect_failure \
    "${root}" "attached hotfix receipt does not match independent verification" \
    "${root}/mismatched-output.json" "${root}/mismatched-receipt.json"

  cp "${root}/release.json" "${root}/release-without-wrong-receipt.json"
  jq '.assets += [{name: "upstream-sync-receipt.json"}]' \
    "${root}/release.json" > "${root}/wrong-receipt-release.json"
  mv "${root}/wrong-receipt-release.json" "${root}/release.json"
  expect_failure \
    "${root}" "duplicate or semantically wrong receipt" \
    "${root}/wrong-receipt-output.json"
  mv "${root}/release-without-wrong-receipt.json" "${root}/release.json"

  jq '.assets[1].digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"' \
    "${root}/release-api.json" > "${root}/wrong-asset.json"
  expect_failure \
    "${root}" "checksum for" \
    "${root}/asset-output.json" "" "${root}/wrong-asset.json"

  sed -i 's/  / */' "${root}/checksums.txt"
  checksums_sha=$(sha256sum "${root}/checksums.txt" | awk '{ print $1 }')
  jq --arg digest "sha256:${checksums_sha}" \
    '(.assets[] | select(.name == "checksums.txt") | .digest) = $digest' \
    "${root}/release-api.json" > "${root}/starred-checksum-api.json"
  expect_failure \
    "${root}" "checksums.txt contains a malformed line" \
    "${root}/starred-output.json" "" "${root}/starred-checksum-api.json"
  make_fixtures "${root}"

  local separator_case
  for separator_case in single-space tab; do
    if [ "${separator_case}" = single-space ]; then
      sed -i 's/  / /' "${root}/checksums.txt"
    else
      sed -i $'s/  /\t/' "${root}/checksums.txt"
    fi
    checksums_sha=$(sha256sum "${root}/checksums.txt" | awk '{ print $1 }')
    jq --arg digest "sha256:${checksums_sha}" \
      '(.assets[] | select(.name == "checksums.txt") | .digest) = $digest' \
      "${root}/release-api.json" > "${root}/${separator_case}-checksum-api.json"
    expect_failure \
      "${root}" "checksums.txt contains a malformed line" \
      "${root}/${separator_case}-output.json" "" \
      "${root}/${separator_case}-checksum-api.json"
    make_fixtures "${root}"
  done

  jq 'del(.manifests[] | select(.platform.architecture == "arm64"))' \
    "${FIXTURES}/image-index.json" > "${root}/missing-platform.json"
  expect_failure \
    "${root}" "invalid platform or attestation descriptor set" \
    "${root}/platform-output.json" "" "${root}/release-api.json" \
    "${root}/missing-platform.json"

  rm -rf "${root}"
}

test_nonzero_and_prerelease_roots_still_generate_schema_one() {
  local root receipt

  for release_line in nonzero prerelease; do
    if [ "${release_line}" = nonzero ]; then
      local BASE_TAG=v7.2.131-unstableneutron.4
      local TAG=v7.2.131-unstableneutron.5
      local ORIGINAL_SOURCE_TAG=v7.2.131
    else
      local BASE_TAG=v7.1.45-0.unstableneutron.2
      local TAG=v7.1.45-0.unstableneutron.3
      local ORIGINAL_SOURCE_TAG=v7.1.45-0
    fi
    root=$(mktemp -d)
    setup_repo "${root}"
    make_fixtures "${root}"
    make_stubs "${root}"
    receipt=${root}/receipt.json
    run_verifier "${root}" "${receipt}" >/dev/null
    jq -e \
      --arg root_tag "${BASE_TAG}" '
        .hotfix_schema_version == 1 and .previous_release.tag == $root_tag and
        (has("accepted_upstream_root") | not)
      ' "${receipt}" >/dev/null \
      || fail "first hotfix above ${BASE_TAG} was not schema one"
    rm -rf "${root}"
  done
}

main() {
  [ -x "${VERIFIER}" ] || fail "verifier is missing or not executable"
  test_receipt_binds_release_and_upstream_state
  test_nonzero_and_prerelease_roots_still_generate_schema_one
  echo "[OK] hotfix release verifier tests passed"
}

main "$@"
