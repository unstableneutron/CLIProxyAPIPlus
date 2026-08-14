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
MAIN_POLICY=exact
REQUIRE_LATEST_PARITY=""
REQUIRE_ARCHITECTURE_TAGS=false
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
    --main-policy)
      [ "$#" -ge 2 ] || die "--main-policy requires a value"
      MAIN_POLICY=$2
      shift 2
      ;;
    --require-latest-parity)
      [ "$#" -ge 2 ] || die "--require-latest-parity requires a value"
      REQUIRE_LATEST_PARITY=$2
      shift 2
      ;;
    --require-architecture-tags)
      [ "$#" -ge 2 ] || die "--require-architecture-tags requires a value"
      REQUIRE_ARCHITECTURE_TAGS=$2
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
case "${MAIN_POLICY}" in
  exact|descendant) ;;
  *) die "--main-policy must be exact or descendant" ;;
esac
case "${REQUIRE_LATEST_PARITY}" in
  true|false) ;;
  *) die "--require-latest-parity must be true or false" ;;
esac
case "${REQUIRE_ARCHITECTURE_TAGS}" in
  true|false) ;;
  *) die "--require-architecture-tags must be true or false" ;;
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
if [ "${MAIN_POLICY}" = exact ]; then
  [ "${MAIN_COMMIT}" = "${EXPECTED_COMMIT}" ] \
    || die "main resolves to ${MAIN_COMMIT}, expected ${EXPECTED_COMMIT}"
else
  COMPARE_STATUS=$(gh api \
    "repos/${GITHUB_REPOSITORY}/compare/${EXPECTED_COMMIT}...${MAIN_COMMIT}" \
    --jq .status)
  case "${COMPARE_STATUS}" in
    identical|ahead) ;;
    *) die "main ${MAIN_COMMIT} does not descend from ${EXPECTED_COMMIT}" ;;
  esac
fi

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
  '[.assets[].name |
    select(. != "upstream-sync-receipt.json" and . != "hotfix-release-receipt.json")] |
    sort' \
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

RECEIPT_SCHEMA_VERSION=1
ARCHITECTURE_IMAGES='{}'
RECEIPT_PLATFORMS='["linux/amd64","linux/arm64"]'
if [ "${REQUIRE_ARCHITECTURE_TAGS}" = true ]; then
  RECEIPT_SCHEMA_VERSION=2
  ARCHITECTURE_ROWS=()
  while IFS= read -r ROW; do
    ARCHITECTURE_ROWS+=("${ROW}")
  done < <(jq -r '
      .manifests[]? |
      select(.platform.os == "linux" and .platform.architecture != "unknown") |
      [
        .platform.os,
        .platform.architecture,
        (.platform.variant // ""),
        .digest
      ] | join("|")
    ' <<< "${IMAGE_INDEX}")
  [ "${#ARCHITECTURE_ROWS[@]}" -gt 0 ] \
    || die "Image ${IMAGE_REF} has no architecture manifests to verify"
  for ROW in "${ARCHITECTURE_ROWS[@]}"; do
    IFS='|' read -r PLATFORM_OS PLATFORM_ARCH PLATFORM_VARIANT PLATFORM_DIGEST <<< "${ROW}"
    TAG_SUFFIX=${PLATFORM_ARCH}
    if [ -n "${PLATFORM_VARIANT}" ]; then
      TAG_SUFFIX+="-${PLATFORM_VARIANT//\//-}"
    fi
    ARCHITECTURE_REF="${IMAGE_REPOSITORY}:${TAG}-${TAG_SUFFIX}"
    ARCHITECTURE_MANIFEST=$(docker buildx imagetools inspect \
      "${ARCHITECTURE_REF}" \
      --format '{{json .Manifest}}')
    ARCHITECTURE_DIGEST=$(jq -r '.digest // empty' <<< "${ARCHITECTURE_MANIFEST}")
    if [ "${ARCHITECTURE_DIGEST}" != "${PLATFORM_DIGEST}" ]; then
      die "Architecture tag ${ARCHITECTURE_REF} resolves to ${ARCHITECTURE_DIGEST}, expected ${PLATFORM_DIGEST}"
    fi
    PLATFORM_KEY="${PLATFORM_OS}/${PLATFORM_ARCH}"
    if [ -n "${PLATFORM_VARIANT}" ]; then
      PLATFORM_KEY+="/${PLATFORM_VARIANT}"
    fi
    ARCHITECTURE_IMAGES=$(jq -c \
      --arg platform "${PLATFORM_KEY}" \
      --arg image "${ARCHITECTURE_REF}" \
      --arg digest "${ARCHITECTURE_DIGEST}" \
      '. + {($platform): {image: $image, digest: $digest}}' \
      <<< "${ARCHITECTURE_IMAGES}")
  done
  RECEIPT_PLATFORMS=$(jq -c 'keys' <<< "${ARCHITECTURE_IMAGES}")
fi

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
  --argjson schema_version "${RECEIPT_SCHEMA_VERSION}" \
  --arg sync_id "${EXPECTED_SYNC_ID}" \
  --arg plan_fingerprint "${EXPECTED_PLAN_FINGERPRINT}" \
  --arg main_commit "${EXPECTED_COMMIT}" \
  --arg tag "${TAG}" \
  --arg tag_commit "${TAG_COMMIT}" \
  --arg release_url "${RELEASE_URL}" \
  --argjson release_assets "${RELEASE_ASSETS}" \
  --arg image "${IMAGE_REF}" \
  --arg image_digest "${IMAGE_DIGEST}" \
  --argjson architecture_images "${ARCHITECTURE_IMAGES}" \
  --argjson platforms "${RECEIPT_PLATFORMS}" \
  --arg workflow_run_id "${GITHUB_RUN_ID:-local}" \
  '{
    schema_version: $schema_version,
    sync_id: $sync_id,
    plan_fingerprint: $plan_fingerprint,
    main_commit: $main_commit,
    tag: $tag,
    tag_commit: $tag_commit,
    release_url: $release_url,
    release_assets: $release_assets,
    image: $image,
    image_digest: $image_digest,
    platforms: $platforms,
    workflow_run_id: $workflow_run_id
  } + if $schema_version >= 2 then {
    architecture_images: $architecture_images
  } else {} end' > "${RECEIPT_TEMP}"
mv "${RECEIPT_TEMP}" "${RECEIPT}"
trap - EXIT

echo "[OK] verified release ${TAG} at ${EXPECTED_COMMIT}; receipt=${RECEIPT}"
