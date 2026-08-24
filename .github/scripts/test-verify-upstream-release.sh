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
  local expected_assets api_assets='[]' id=1000 name digest size
  # shellcheck disable=SC1091 # SCRIPT_DIR is resolved at runtime.
  expected_assets=$(source "${SCRIPT_DIR}/release-assets.sh"; expected_release_assets_json "${TAG}")
  : > "${root}/checksums.txt"
  while IFS= read -r name; do
    id=$((id + 1))
    if [ "${name}" = checksums.txt ]; then
      continue
    fi
    digest=$(printf '%s' "${name}" | sha256sum | awk '{ print $1 }')
    printf '%s  %s\n' "${digest}" "${name}" >> "${root}/checksums.txt"
  done < <(jq -r '.[]' <<< "${expected_assets}")
  while IFS= read -r name; do
    id=$((id + 1))
    if [ "${name}" = checksums.txt ]; then
      size=$(stat -c %s "${root}/checksums.txt")
      digest=$(sha256sum "${root}/checksums.txt" | awk '{ print $1 }')
    else
      size=1
      digest=$(printf '%s' "${name}" | sha256sum | awk '{ print $1 }')
    fi
    api_assets=$(jq -c \
      --arg name "${name}" \
      --arg digest "sha256:${digest}" \
      --argjson id "${id}" \
      --argjson size "${size}" \
      '. + [{
        id: $id, name: $name, size: $size, digest: $digest,
        state: "uploaded",
        url: ("https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/assets/" + ($id | tostring)),
        uploader: {login: "github-actions[bot]", id: 41898282, type: "Bot"}
      }]' <<< "${api_assets}")
  done < <(jq -r '.[]' <<< "${expected_assets}")
  jq -n --argjson assets "${api_assets}" '{assets: $assets}' > "${root}/release-api.json"
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
  api:*releases/tags/*)
    cat "${STUB_RELEASE_API_JSON}"
    ;;
  release:view)
    cat "${STUB_RELEASE_JSON}"
    ;;
  release:download)
    output=""
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --dir ]; then output=$2; shift 2; else shift; fi
    done
    [ -n "${output}" ] || exit 2
    mkdir -p "${output}"
    cp "${STUB_CHECKSUM_FILE}" "${output}/checksums.txt"
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
  local require_latest=$3
  local main_commit=${4:-${COMMIT}}
  local tag_commit=${5:-${COMMIT}}
  local release_json=${6:-${FIXTURES}/release.json}
  local image_json=${7:-${FIXTURES}/image-index.json}
  local latest_json=${8:-${image_json}}
  local main_policy=${9:-exact}
  local compare_status=${10:-ahead}
  local require_architecture_tags=${11:-false}
  local amd64_image_json=${12:-${FIXTURES}/image-amd64.json}
  local arm64_image_json=${13:-${FIXTURES}/image-arm64.json}

  PATH="${root}/bin:${PATH}" \
    GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
    GITHUB_RUN_ID=123456789 \
    GITHUB_RUN_ATTEMPT=1 \
    GITHUB_WORKFLOW_REF=unstableneutron/CLIProxyAPIPlus/.github/workflows/upstream-sync-v2.yml@refs/heads/main \
    STUB_MAIN_COMMIT="${main_commit}" \
    STUB_TAG_COMMIT="${tag_commit}" \
    STUB_RELEASE_JSON="${release_json}" \
    STUB_RELEASE_API_JSON="${root}/release-api.json" \
    STUB_CHECKSUM_FILE="${root}/checksums.txt" \
    STUB_IMAGE_INDEX_JSON="${image_json}" \
    STUB_LATEST_INDEX_JSON="${latest_json}" \
    STUB_AMD64_IMAGE_JSON="${amd64_image_json}" \
    STUB_ARM64_IMAGE_JSON="${arm64_image_json}" \
    STUB_COMPARE_STATUS="${compare_status}" \
    "${VERIFIER}" \
      --tag "${TAG}" \
      --expected-commit "${COMMIT}" \
      --expected-sync-id "${SYNC_ID}" \
      --expected-plan-fingerprint "${FINGERPRINT}" \
      --image "${IMAGE}" \
      --main-policy "${main_policy}" \
      --require-architecture-tags "${require_architecture_tags}" \
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

  run_verifier \
    "${root}" "${receipt}" true \
    "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" \
    "${FIXTURES}/image-index.json" "${FIXTURES}/image-index.json" \
    exact ahead true

  jq -e \
    --arg commit "${COMMIT}" \
    --arg fingerprint "${FINGERPRINT}" \
    --arg sync_id "${SYNC_ID}" \
    --arg tag "${TAG}" \
    '.schema_version == 3 and
     .main_commit == $commit and
     .tag_commit == $commit and
     .plan_fingerprint == $fingerprint and
     .sync_id == $sync_id and
     .tag == $tag and
     .image_digest == "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" and
     .platforms == ["linux/amd64", "linux/arm64"] and
     .architecture_images["linux/amd64"].digest == "sha256:1111111111111111111111111111111111111111111111111111111111111111" and
     .architecture_images["linux/arm64"].digest == "sha256:2222222222222222222222222222222222222222222222222222222222222222" and
     .workflow_run_id == "123456789" and
     .release_workflow == {
       path: ".github/workflows/upstream-sync-v2.yml",
       ref: "unstableneutron/CLIProxyAPIPlus/.github/workflows/upstream-sync-v2.yml@refs/heads/main",
       commit: $commit,
       run_id: "123456789",
       run_attempt: "1"
     } and
     (.release_asset_identities | keys) == .release_assets and
     all(.release_asset_identities[];
       keys == ["digest", "id", "size"] and
       (.id | type) == "number" and (.size | type) == "number" and
       (.digest | test("^sha256:[0-9a-f]{64}$")))' \
    "${receipt}" >/dev/null || fail "receipt did not contain the verified identity"
  rm -rf "${root}"
}

test_rejects_mismatched_architecture_tag() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  jq '.digest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"' \
    "${FIXTURES}/image-amd64.json" > "${root}/wrong-amd64.json"

  expect_failure \
    "${root}" "${root}/receipt.json" "Architecture tag" \
    true "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" \
    "${FIXTURES}/image-index.json" "${FIXTURES}/image-index.json" \
    exact ahead true "${root}/wrong-amd64.json" "${FIXTURES}/image-arm64.json"
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
    "${root}" "${root}/receipt.json" "asset set differs from the release contract" \
    false "${COMMIT}" "${COMMIT}" "${root}/wrong-brand.json"
  rm -rf "${root}"
}

test_rejects_incomplete_or_conflicting_release_asset_matrix() {
  local root mutation
  root=$(mktemp -d)
  make_stubs "${root}"
  for mutation in missing extra duplicate renamed; do
    case "${mutation}" in
      missing)
        jq 'del(.assets[1])' "${FIXTURES}/release.json" > "${root}/${mutation}.json"
        ;;
      extra)
        jq '.assets += [{name: "notes.txt"}]' "${FIXTURES}/release.json" > "${root}/${mutation}.json"
        ;;
      duplicate)
        jq '.assets += [.assets[1]]' "${FIXTURES}/release.json" > "${root}/${mutation}.json"
        ;;
      renamed)
        jq '(.assets[1].name) |= sub("darwin_aarch64"; "darwin_arm64")' \
          "${FIXTURES}/release.json" > "${root}/${mutation}.json"
        ;;
    esac
    expect_failure \
      "${root}" "${root}/${mutation}-receipt.json" \
      "asset set differs from the release contract" \
      false "${COMMIT}" "${COMMIT}" "${root}/${mutation}.json"
  done
  rm -rf "${root}"
}

test_rejects_semantically_wrong_or_duplicate_receipts() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  jq '.assets += [{name: "hotfix-release-receipt.json"}]' \
    "${FIXTURES}/release.json" > "${root}/wrong-receipt.json"
  expect_failure \
    "${root}" "${root}/wrong-receipt-output.json" \
    "duplicate or semantically wrong receipt" \
    false "${COMMIT}" "${COMMIT}" "${root}/wrong-receipt.json"

  jq '.assets += [
      {name: "upstream-sync-receipt.json"},
      {name: "upstream-sync-receipt.json"}
    ]' "${FIXTURES}/release.json" > "${root}/duplicate-receipt.json"
  expect_failure \
    "${root}" "${root}/duplicate-receipt-output.json" \
    "duplicate or semantically wrong receipt" \
    false "${COMMIT}" "${COMMIT}" "${root}/duplicate-receipt.json"
  rm -rf "${root}"
}

test_rejects_missing_required_platforms() {
  local root
  root=$(mktemp -d)
  make_stubs "${root}"
  jq 'del(.manifests[] | select(.platform.architecture == "amd64"))' \
    "${FIXTURES}/image-index.json" > "${root}/missing-amd64.json"
  expect_failure \
    "${root}" "${root}/receipt-amd64.json" "invalid platform or attestation descriptor set" \
    false "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" "${root}/missing-amd64.json"

  jq 'del(.manifests[] | select(.platform.architecture == "arm64"))' \
    "${FIXTURES}/image-index.json" > "${root}/missing-arm64.json"
  expect_failure \
    "${root}" "${root}/receipt-arm64.json" "invalid platform or attestation descriptor set" \
    false "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" "${root}/missing-arm64.json"
  rm -rf "${root}"
}

test_rejects_extra_duplicate_and_malformed_descriptors() {
  local root mutation
  root=$(mktemp -d)
  make_stubs "${root}"
  jq '.manifests += [{
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      digest: ("sha256:" + ("4" * 64)),
      platform: {architecture: "amd64", os: "windows"}
    }]' "${FIXTURES}/image-index.json" > "${root}/extra.json"
  jq '.manifests += [.manifests[0]]' \
    "${FIXTURES}/image-index.json" > "${root}/duplicate.json"
  jq 'del(.manifests[2].annotations["vnd.docker.reference.digest"])' \
    "${FIXTURES}/image-index.json" > "${root}/malformed-attestation.json"
  jq '.manifests += [(.manifests[2] | .digest = ("sha256:" + ("5" * 64)))]' \
    "${FIXTURES}/image-index.json" > "${root}/duplicate-attestation.json"
  for mutation in extra duplicate malformed-attestation duplicate-attestation; do
    expect_failure \
      "${root}" "${root}/${mutation}-receipt.json" \
      "invalid platform or attestation descriptor set" \
      false "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" \
      "${root}/${mutation}.json"
  done

  jq '.mediaType = "application/octet-stream"' \
    "${FIXTURES}/image-amd64.json" > "${root}/bad-architecture.json"
  expect_failure \
    "${root}" "${root}/bad-architecture-receipt.json" "Architecture tag" \
    false "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" \
    "${FIXTURES}/image-index.json" "${FIXTURES}/image-index.json" \
    exact ahead true "${root}/bad-architecture.json" "${FIXTURES}/image-arm64.json"

  expect_failure \
    "${root}" "${root}/bad-latest-receipt.json" "latest digest" \
    true "${COMMIT}" "${COMMIT}" "${FIXTURES}/release.json" \
    "${FIXTURES}/image-index.json" "${root}/extra.json"
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
  test_rejects_incomplete_or_conflicting_release_asset_matrix
  test_rejects_semantically_wrong_or_duplicate_receipts
  test_rejects_missing_required_platforms
  test_rejects_extra_duplicate_and_malformed_descriptors
  test_rejects_mismatched_architecture_tag
  test_latest_parity_is_conditional
  echo "[OK] upstream release verifier tests passed"
}

main "$@"
