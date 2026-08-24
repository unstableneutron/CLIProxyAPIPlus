#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "${SCRIPT_DIR}/release-assets.sh"

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
ALLOWED_RECEIPT_NAME=upstream-sync-receipt.json
RECEIPT_SCHEMA_VERSION=""

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
    --allowed-receipt-name)
      [ "$#" -ge 2 ] || die "--allowed-receipt-name requires a value"
      ALLOWED_RECEIPT_NAME=$2
      shift 2
      ;;
    --receipt-schema-version)
      [ "$#" -ge 2 ] || die "--receipt-schema-version requires a value"
      RECEIPT_SCHEMA_VERSION=$2
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
case "${ALLOWED_RECEIPT_NAME}" in
  upstream-sync-receipt.json|hotfix-release-receipt.json) ;;
  *) die "--allowed-receipt-name must identify an upstream or hotfix receipt" ;;
esac
case "${RECEIPT_SCHEMA_VERSION:-auto}" in
  auto|1|2|3) ;;
  *) die "--receipt-schema-version must be 1, 2, or 3" ;;
esac
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
if ! jq -e --arg allowed "${ALLOWED_RECEIPT_NAME}" '
    ([.assets[] | select(.name == $allowed)] | length) <= 1 and
    ([.assets[] |
      select(
        (.name == "upstream-sync-receipt.json" or .name == "hotfix-release-receipt.json") and
        .name != $allowed
      )] | length) == 0
  ' <<< "${RELEASE_JSON}" >/dev/null; then
  die "Release ${TAG} contains a duplicate or semantically wrong receipt"
fi
RELEASE_ASSETS=$(jq -c \
  '[.assets[].name |
    select(. != "upstream-sync-receipt.json" and . != "hotfix-release-receipt.json")] |
    sort' \
  <<< "${RELEASE_JSON}")
EXPECTED_RELEASE_ASSETS=$(expected_release_assets_json "${TAG}") \
  || die "could not derive the expected release assets for ${TAG}"
[ "${RELEASE_ASSETS}" = "${EXPECTED_RELEASE_ASSETS}" ] \
  || die "Release ${TAG} asset set differs from the release contract"

IMAGE_INDEX=$(docker buildx imagetools inspect "${IMAGE_REF}" --format '{{json .Manifest}}')
IMAGE_DIGEST=$(jq -r '.digest // empty' <<< "${IMAGE_INDEX}")
[[ "${IMAGE_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] \
  || die "Image ${IMAGE_REF} did not resolve to a valid index digest"
jq -e -f "${SCRIPT_DIR}/verify-registry-index.jq" <<< "${IMAGE_INDEX}" >/dev/null \
  || die "Image ${IMAGE_REF} has an invalid platform or attestation descriptor set"

ARCHITECTURE_IMAGES='{}'
RECEIPT_PLATFORMS='["linux/amd64","linux/arm64"]'
if [ "${REQUIRE_ARCHITECTURE_TAGS}" = true ]; then
  if [ -z "${RECEIPT_SCHEMA_VERSION}" ]; then
    RECEIPT_SCHEMA_VERSION=3
  fi
  [ "${RECEIPT_SCHEMA_VERSION}" -ge 2 ] \
    || die "receipt schema 1 cannot bind architecture tags"
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
    if [ "${ARCHITECTURE_DIGEST}" != "${PLATFORM_DIGEST}" ] || \
       ! jq -e '
         .schemaVersion == 2 and
         (.mediaType == "application/vnd.oci.image.manifest.v1+json" or
          .mediaType == "application/vnd.docker.distribution.manifest.v2+json")
       ' <<< "${ARCHITECTURE_MANIFEST}" >/dev/null; then
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
else
  if [ -z "${RECEIPT_SCHEMA_VERSION}" ]; then
    RECEIPT_SCHEMA_VERSION=1
  fi
  [ "${RECEIPT_SCHEMA_VERSION}" -eq 1 ] \
    || die "receipt schemas 2 and 3 require architecture tags"
fi

if [ "${REQUIRE_LATEST_PARITY}" = true ]; then
  LATEST_INDEX=$(docker buildx imagetools inspect \
    "${IMAGE_REPOSITORY}:latest" \
    --format '{{json .Manifest}}')
  LATEST_DIGEST=$(jq -r '.digest // empty' <<< "${LATEST_INDEX}")
  if [ "${LATEST_DIGEST}" != "${IMAGE_DIGEST}" ] || \
     ! jq -e -f "${SCRIPT_DIR}/verify-registry-index.jq" \
       <<< "${LATEST_INDEX}" >/dev/null; then
    die "latest digest ${LATEST_DIGEST} does not match ${TAG} digest ${IMAGE_DIGEST}"
  fi
fi

RELEASE_ASSET_IDENTITIES='{}'
RELEASE_WORKFLOW='{}'
if [ "${RECEIPT_SCHEMA_VERSION}" -eq 3 ]; then
  : "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required for receipt schema 3}"
  : "${GITHUB_RUN_ATTEMPT:?GITHUB_RUN_ATTEMPT is required for receipt schema 3}"
  : "${GITHUB_WORKFLOW_REF:?GITHUB_WORKFLOW_REF is required for receipt schema 3}"
  [[ "${GITHUB_RUN_ID}" =~ ^[1-9][0-9]*$ ]] \
    || die "GITHUB_RUN_ID must be a positive decimal integer"
  [[ "${GITHUB_RUN_ATTEMPT}" =~ ^[1-9][0-9]*$ ]] \
    || die "GITHUB_RUN_ATTEMPT must be a positive decimal integer"
  if [ "${#GITHUB_RUN_ID}" -gt 16 ] || \
     { [ "${#GITHUB_RUN_ID}" -eq 16 ] && [ "${GITHUB_RUN_ID}" -gt 9007199254740991 ]; }; then
    die "GITHUB_RUN_ID exceeds the safe integer bound"
  fi
  if [ "${#GITHUB_RUN_ATTEMPT}" -gt 16 ] || \
     { [ "${#GITHUB_RUN_ATTEMPT}" -eq 16 ] && [ "${GITHUB_RUN_ATTEMPT}" -gt 9007199254740991 ]; }; then
    die "GITHUB_RUN_ATTEMPT exceeds the safe integer bound"
  fi
  EXPECTED_WORKFLOW_REF="${GITHUB_REPOSITORY}/.github/workflows/upstream-sync-v2.yml@refs/heads/main"
  [ "${GITHUB_WORKFLOW_REF}" = "${EXPECTED_WORKFLOW_REF}" ] \
    || die "upstream release workflow ref differs"

  RELEASE_API=$(gh api "repos/${GITHUB_REPOSITORY}/releases/tags/${TAG}")
  RELEASE_ASSET_IDENTITIES=$(jq -ce \
    --arg repo "${GITHUB_REPOSITORY}" \
    --argjson expected "${EXPECTED_RELEASE_ASSETS}" '
      [.assets[] |
        select(.name != "upstream-sync-receipt.json" and .name != "hotfix-release-receipt.json") |
        .name as $name |
        if ($name | type) != "string" or ($expected | index($name)) == null or
           (.id | type) != "number" or (.id | floor) != .id or .id <= 0 or .id > 9007199254740991 or
           (.size | type) != "number" or (.size | floor) != .size or .size <= 0 or .size > 2000000000 or
           .state != "uploaded" or
           .url != ("https://api.github.com/repos/" + $repo + "/releases/assets/" + (.id | tostring)) or
           .uploader.login != "github-actions[bot]" or .uploader.id != 41898282 or .uploader.type != "Bot" or
           (.digest | type) != "string" or (.digest | test("^sha256:[0-9a-f]{64}$") | not)
        then error("release asset identity differs")
        else {key: .name, value: {id: .id, size: .size, digest: .digest}}
        end
      ] |
      if length == ($expected | length) and length == ([.[].key] | unique | length)
      then from_entries
      else error("release asset identity set differs")
      end
    ' <<< "${RELEASE_API}") \
    || die "release asset identities are incomplete or invalid"
  if ! jq -e --argjson expected "${EXPECTED_RELEASE_ASSETS}" \
    'keys == $expected' <<< "${RELEASE_ASSET_IDENTITIES}" >/dev/null; then
    die "release asset identity set differs from the release contract"
  fi

  CHECKSUM_DIR=$(mktemp -d)
  trap 'rm -rf "${CHECKSUM_DIR}"' EXIT
  gh release download "${TAG}" \
    --repo "${GITHUB_REPOSITORY}" \
    --pattern checksums.txt \
    --dir "${CHECKSUM_DIR}"
  CHECKSUM_FILE="${CHECKSUM_DIR}/checksums.txt"
  [ -f "${CHECKSUM_FILE}" ] || die "could not download checksums.txt"
  [ "$(stat -c %s "${CHECKSUM_FILE}")" -eq \
    "$(jq -r '."checksums.txt".size' <<< "${RELEASE_ASSET_IDENTITIES}")" ] \
    || die "checksums.txt bytes differ from the release asset size"
  [ "sha256:$(sha256sum "${CHECKSUM_FILE}" | awk '{ print $1 }')" = \
    "$(jq -r '."checksums.txt".digest' <<< "${RELEASE_ASSET_IDENTITIES}")" ] \
    || die "checksums.txt bytes differ from the release asset digest"
  SEEN_CHECKSUMS=$(mktemp)
  : > "${SEEN_CHECKSUMS}"
  while IFS= read -r checksum_line || [ -n "${checksum_line}" ]; do
    [[ "${checksum_line}" =~ ^([0-9a-f]{64})\ \ ([A-Za-z0-9._+-]+)$ ]] \
      || die "checksums.txt contains a malformed line"
    CHECKSUM_DIGEST="sha256:${BASH_REMATCH[1]}"
    CHECKSUM_NAME=${BASH_REMATCH[2]}
    [ "${CHECKSUM_NAME}" != checksums.txt ] \
      || die "checksums.txt must not contain itself"
    grep -Fxq "${CHECKSUM_NAME}" "${SEEN_CHECKSUMS}" \
      && die "checksums.txt contains duplicate entries"
    echo "${CHECKSUM_NAME}" >> "${SEEN_CHECKSUMS}"
    [ "$(jq -r --arg name "${CHECKSUM_NAME}" '.[$name].digest // empty' \
      <<< "${RELEASE_ASSET_IDENTITIES}")" = "${CHECKSUM_DIGEST}" ] \
      || die "checksum for ${CHECKSUM_NAME} differs from its release asset digest"
  done < "${CHECKSUM_FILE}"
  [ "$(wc -l < "${SEEN_CHECKSUMS}" | tr -d ' ')" -eq \
    "$(jq 'length - 1' <<< "${EXPECTED_RELEASE_ASSETS}")" ] \
    || die "checksums.txt does not cover every archive"
  rm -rf "${CHECKSUM_DIR}"
  trap - EXIT

  RELEASE_WORKFLOW=$(jq -cn \
    --arg path .github/workflows/upstream-sync-v2.yml \
    --arg ref "${GITHUB_WORKFLOW_REF}" \
    --arg commit "${EXPECTED_COMMIT}" \
    --arg run_id "${GITHUB_RUN_ID}" \
    --arg run_attempt "${GITHUB_RUN_ATTEMPT}" '{
      path: $path,
      ref: $ref,
      commit: $commit,
      run_id: $run_id,
      run_attempt: $run_attempt
    }')
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
  --argjson release_asset_identities "${RELEASE_ASSET_IDENTITIES}" \
  --argjson release_workflow "${RELEASE_WORKFLOW}" \
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
  } else {} end + if $schema_version >= 3 then {
    release_asset_identities: $release_asset_identities,
    release_workflow: $release_workflow
  } else {} end' > "${RECEIPT_TEMP}"
mv "${RECEIPT_TEMP}" "${RECEIPT}"
trap - EXIT

echo "[OK] verified release ${TAG} at ${EXPECTED_COMMIT}; receipt=${RECEIPT}"
