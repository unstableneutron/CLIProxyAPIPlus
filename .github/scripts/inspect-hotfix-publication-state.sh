#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "[hotfix-publication-state] $*" >&2
  exit 1
}

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <tag> <repository> <image> <tag-state>" >&2
  exit 2
fi
TAG=$1
REPOSITORY=$2
IMAGE=$3
TAG_STATE=$4
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

case "${TAG_STATE}" in
  absent|exact) ;;
  *) die "tag state must be absent or exact" ;;
esac

ROOT=$(mktemp -d)
trap 'rm -rf "${ROOT}"' EXIT
RELEASE_STATE=absent
RECEIPT_STATE=absent
RELEASE_RESPONSE=${ROOT}/release-response
if gh api --include "/repos/${REPOSITORY}/releases/tags/${TAG}" > "${RELEASE_RESPONSE}" 2>&1; then
  RELEASE=${ROOT}/release.json
  gh api "/repos/${REPOSITORY}/releases/tags/${TAG}" > "${RELEASE}"
  jq -e \
    --arg tag "${TAG}" \
    --arg repo "${REPOSITORY}" '
      .tag_name == $tag and .draft == false and .prerelease == false and
      .target_commitish == "main" and
      .html_url == ("https://github.com/" + $repo + "/releases/tag/" + $tag) and
      .author.login == "github-actions[bot]" and .author.id == 41898282 and
      (.assets | type) == "array" and
      ([.assets[].name] | length) == ([.assets[].name] | unique | length) and
      ([.assets[] | select(.name == "checksums.txt")] | length) == 1 and
      ([.assets[] | select(.name | test("^CLIProxyAPIPlus_[A-Za-z0-9._+-]+\\.(tar\\.gz|zip)$"))] | length) > 0 and
      ([.assets[] | select(.name == "hotfix-release-receipt.json")] | length) <= 1 and
      ([.assets[] | select(.name == "upstream-sync-receipt.json")] | length) == 0 and
      ([.assets[] | select(
        .name != "checksums.txt" and
        .name != "hotfix-release-receipt.json" and
        (.name | test("^CLIProxyAPIPlus_[A-Za-z0-9._+-]+\\.(tar\\.gz|zip)$") | not)
      )] | length) == 0 and
      all(.assets[];
        (.id | type) == "number" and (.id | floor) == .id and .id > 0 and .id <= 9007199254740991 and
        (.size | type) == "number" and (.size | floor) == .size and .size > 0 and .size <= 2000000000 and
        .state == "uploaded" and
        .url == ("https://api.github.com/repos/" + $repo + "/releases/assets/" + (.id | tostring)) and
        .uploader.login == "github-actions[bot]" and .uploader.id == 41898282 and .uploader.type == "Bot" and
        (.digest | type) == "string" and (.digest | test("^sha256:[0-9a-f]{64}$")))
    ' "${RELEASE}" >/dev/null \
    || die "existing release ${TAG} is incomplete or has a conflicting identity"
  RELEASE_STATE=exact
  if jq -e '[.assets[] | select(.name == "hotfix-release-receipt.json")] | length == 1' \
    "${RELEASE}" >/dev/null; then
    RECEIPT_STATE=exact
  fi
else
  mapfile -t HTTP_STATUSES < <(sed -nE 's/^HTTP\/[0-9.]+ ([0-9]{3})( .*)?\r?$/\1/p' "${RELEASE_RESPONSE}")
  if [ "${#HTTP_STATUSES[@]}" -ne 1 ] || [ "${HTTP_STATUSES[0]}" != 404 ]; then
    echo "Could not classify release ${TAG}:" >&2
    cat "${RELEASE_RESPONSE}" >&2
    exit 1
  fi
fi

CANONICAL_IMAGE_STATE=$("${SCRIPT_DIR}/inspect-ghcr-image-state.sh" "${IMAGE}:${TAG}")
AMD64_IMAGE_STATE=$("${SCRIPT_DIR}/inspect-ghcr-image-state.sh" "${IMAGE}:${TAG}-amd64")
ARM64_IMAGE_STATE=$("${SCRIPT_DIR}/inspect-ghcr-image-state.sh" "${IMAGE}:${TAG}-arm64")

if [ "${TAG_STATE}" = absent ]; then
  if [ "${RELEASE_STATE}" != absent ] || [ "${CANONICAL_IMAGE_STATE}" != absent ] || \
     [ "${AMD64_IMAGE_STATE}" != absent ] || [ "${ARM64_IMAGE_STATE}" != absent ]; then
    die "candidate identities exist without the exact annotated tag"
  fi
  PUBLICATION_STATE=absent
else
  if [ "${CANONICAL_IMAGE_STATE}" = present ] && \
     { [ "${RELEASE_STATE}" != exact ] || [ "${AMD64_IMAGE_STATE}" != present ] || [ "${ARM64_IMAGE_STATE}" != present ]; }; then
    die "canonical image exists without its complete release and architecture identities"
  fi
  if [ "${RECEIPT_STATE}" = exact ] && \
     { [ "${CANONICAL_IMAGE_STATE}" != present ] || [ "${AMD64_IMAGE_STATE}" != present ] || [ "${ARM64_IMAGE_STATE}" != present ]; }; then
    die "receipt exists before the complete image identity"
  fi
  if [ "${RECEIPT_STATE}" = exact ]; then
    PUBLICATION_STATE=receipt-published
  elif [ "${RELEASE_STATE}" = exact ] || [ "${CANONICAL_IMAGE_STATE}" = present ] || \
       [ "${AMD64_IMAGE_STATE}" = present ] || [ "${ARM64_IMAGE_STATE}" = present ]; then
    PUBLICATION_STATE=publishing
  else
    PUBLICATION_STATE=tagged
  fi
fi

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "publication_state=${PUBLICATION_STATE}"
    echo "release_state=${RELEASE_STATE}"
    echo "receipt_state=${RECEIPT_STATE}"
    echo "canonical_image_state=${CANONICAL_IMAGE_STATE}"
    echo "amd64_image_state=${AMD64_IMAGE_STATE}"
    echo "arm64_image_state=${ARM64_IMAGE_STATE}"
  } >> "${GITHUB_OUTPUT}"
fi
echo "[OK] hotfix ${TAG} publication state is ${PUBLICATION_STATE}"
