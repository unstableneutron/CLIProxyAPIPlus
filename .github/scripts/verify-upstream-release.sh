#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "[upstream-release-verifier] $*" >&2
  exit 1
}

TAG=""
EXPECTED_COMMIT=""
EXPECTED_SYNC_ID=""
EXPECTED_PLAN_FINGERPRINT=""
IMAGE_INPUT=""
REQUIRE_LATEST_PARITY=""
RECEIPT=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --tag)
      [ "$#" -ge 2 ] || die "--tag requires a value"
      TAG=$2
      shift 2
      ;;
    --expected-commit)
      [ "$#" -ge 2 ] || die "--expected-commit requires a value"
      EXPECTED_COMMIT=$2
      shift 2
      ;;
    --expected-sync-id)
      [ "$#" -ge 2 ] || die "--expected-sync-id requires a value"
      EXPECTED_SYNC_ID=$2
      shift 2
      ;;
    --expected-plan-fingerprint)
      [ "$#" -ge 2 ] || die "--expected-plan-fingerprint requires a value"
      EXPECTED_PLAN_FINGERPRINT=$2
      shift 2
      ;;
    --image)
      [ "$#" -ge 2 ] || die "--image requires a value"
      IMAGE_INPUT=$2
      shift 2
      ;;
    --require-latest-parity)
      [ "$#" -ge 2 ] || die "--require-latest-parity requires a value"
      REQUIRE_LATEST_PARITY=$2
      shift 2
      ;;
    --receipt)
      [ "$#" -ge 2 ] || die "--receipt requires a value"
      RECEIPT=$2
      shift 2
      ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ -n "${TAG}" ] || die "--tag is required"
[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] || die "--expected-commit must be a 40-character lowercase commit"
[ -n "${EXPECTED_SYNC_ID}" ] || die "--expected-sync-id is required"
[[ "${EXPECTED_PLAN_FINGERPRINT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-plan-fingerprint must be a 40-character lowercase hash"
[ -n "${IMAGE_INPUT}" ] || die "--image is required"
case "${REQUIRE_LATEST_PARITY}" in
  true|false) ;;
  *) die "--require-latest-parity must be true or false" ;;
esac
[ -n "${RECEIPT}" ] || die "--receipt is required"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

case "${IMAGE_INPUT}" in
  *@*) die "--image must be a repository or tag reference, not a digest reference" ;;
esac
IMAGE_LAST_COMPONENT=${IMAGE_INPUT##*/}
if [[ "${IMAGE_LAST_COMPONENT}" == *:* ]]; then
  [[ "${IMAGE_INPUT}" == *":${TAG}" ]] \
    || die "--image tag must match --tag when a tag is provided"
  IMAGE_REPOSITORY=${IMAGE_INPUT%:*}
  IMAGE_REF=${IMAGE_INPUT}
else
  IMAGE_REPOSITORY=${IMAGE_INPUT}
  IMAGE_REF="${IMAGE_INPUT}:${TAG}"
fi

MAIN_COMMIT=$(gh api "repos/${GITHUB_REPOSITORY}/commits/main" --jq .sha)
[ "${MAIN_COMMIT}" = "${EXPECTED_COMMIT}" ] \
  || die "main resolves to ${MAIN_COMMIT}, expected ${EXPECTED_COMMIT}"

TAG_COMMIT=$(gh api "repos/${GITHUB_REPOSITORY}/commits/${TAG}" --jq .sha)
[ "${TAG_COMMIT}" = "${EXPECTED_COMMIT}" ] \
  || die "Tag ${TAG} resolves to ${TAG_COMMIT}, expected ${EXPECTED_COMMIT}"

RELEASE_JSON=$(gh release view "${TAG}" \
  --repo "${GITHUB_REPOSITORY}" \
  --json url,isDraft,isPrerelease,assets)
if ! jq -e '.isDraft == false and .isPrerelease == false and (.url | length > 0)' \
  <<< "${RELEASE_JSON}" >/dev/null; then
  die "Release ${TAG} is missing, draft, prerelease, or has no URL"
fi
RELEASE_URL=$(jq -r '.url' <<< "${RELEASE_JSON}")
RELEASE_ASSETS=$(jq -c \
  '[.assets[].name | select(. != "upstream-sync-receipt.json")] | sort' \
  <<< "${RELEASE_JSON}")
if ! jq -e 'index("checksums.txt") != null' <<< "${RELEASE_ASSETS}" >/dev/null; then
  die "Release ${TAG} is missing checksums.txt"
fi
if ! jq -e 'any(.[]; startswith("CLIProxyAPIPlus_"))' <<< "${RELEASE_ASSETS}" >/dev/null; then
  die "Release ${TAG} has no CLIProxyAPIPlus-branded archive"
fi
if jq -e 'any(.[]; startswith("CLIProxyAPI_"))' <<< "${RELEASE_ASSETS}" >/dev/null; then
  die "Release ${TAG} contains an upstream-branded archive"
fi

IMAGE_INDEX=$(docker buildx imagetools inspect "${IMAGE_REF}" --format '{{json .Manifest}}')
IMAGE_DIGEST=$(jq -r '.digest // empty' <<< "${IMAGE_INDEX}")
[[ "${IMAGE_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] \
  || die "Image ${IMAGE_REF} did not resolve to a valid index digest"
for ARCH in amd64 arm64; do
  if ! jq -e \
    --arg arch "${ARCH}" \
    'any(.manifests[]?; .platform.os == "linux" and .platform.architecture == $arch)' \
    <<< "${IMAGE_INDEX}" >/dev/null; then
    die "Image ${IMAGE_REF} is missing linux/${ARCH}"
  fi
done

if [ "${REQUIRE_LATEST_PARITY}" = true ]; then
  LATEST_INDEX=$(docker buildx imagetools inspect \
    "${IMAGE_REPOSITORY}:latest" \
    --format '{{json .Manifest}}')
  LATEST_DIGEST=$(jq -r '.digest // empty' <<< "${LATEST_INDEX}")
  if [ "${LATEST_DIGEST}" != "${IMAGE_DIGEST}" ]; then
    die "latest digest ${LATEST_DIGEST} does not match ${TAG} digest ${IMAGE_DIGEST}"
  fi
fi

mkdir -p "$(dirname -- "${RECEIPT}")"
RECEIPT_TEMP=$(mktemp "${RECEIPT}.tmp.XXXXXX")
trap 'rm -f "${RECEIPT_TEMP}"' EXIT
jq -n \
  --arg sync_id "${EXPECTED_SYNC_ID}" \
  --arg plan_fingerprint "${EXPECTED_PLAN_FINGERPRINT}" \
  --arg main_commit "${MAIN_COMMIT}" \
  --arg tag "${TAG}" \
  --arg tag_commit "${TAG_COMMIT}" \
  --arg release_url "${RELEASE_URL}" \
  --argjson release_assets "${RELEASE_ASSETS}" \
  --arg image "${IMAGE_REF}" \
  --arg image_digest "${IMAGE_DIGEST}" \
  --arg workflow_run_id "${GITHUB_RUN_ID:-local}" \
  '{
    schema_version: 1,
    sync_id: $sync_id,
    plan_fingerprint: $plan_fingerprint,
    main_commit: $main_commit,
    tag: $tag,
    tag_commit: $tag_commit,
    release_url: $release_url,
    release_assets: $release_assets,
    image: $image,
    image_digest: $image_digest,
    platforms: ["linux/amd64", "linux/arm64"],
    workflow_run_id: $workflow_run_id
  }' > "${RECEIPT_TEMP}"
mv "${RECEIPT_TEMP}" "${RECEIPT}"
trap - EXIT

echo "[OK] verified release ${TAG} at ${EXPECTED_COMMIT}; receipt=${RECEIPT}"
