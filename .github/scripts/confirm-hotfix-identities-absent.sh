#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <tag> <repository> <image>" >&2
  exit 2
fi
TAG=$1
REPOSITORY=$2
IMAGE=$3
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

set +e
git ls-remote --exit-code --tags origin "refs/tags/${TAG}" >/dev/null
TAG_STATUS=$?
set -e
case "${TAG_STATUS}" in
  0)
    echo "Tag ${TAG} already exists remotely." >&2
    exit 1
    ;;
  2) ;;
  *)
    echo "Could not confirm tag ${TAG} is absent (git status ${TAG_STATUS})." >&2
    exit 1
    ;;
esac

RELEASE_OUTPUT=$(mktemp)
trap 'rm -f "${RELEASE_OUTPUT}"' EXIT
if gh api --include "/repos/${REPOSITORY}/releases/tags/${TAG}" > "${RELEASE_OUTPUT}" 2>&1; then
  echo "Release ${TAG} already exists." >&2
  exit 1
fi
mapfile -t HTTP_STATUSES < <(sed -nE 's/^HTTP\/[0-9.]+ ([0-9]{3})( .*)?\r?$/\1/p' "${RELEASE_OUTPUT}")
if [ "${#HTTP_STATUSES[@]}" -ne 1 ] || [ "${HTTP_STATUSES[0]}" != 404 ]; then
  echo "Could not confirm release ${TAG} is absent:" >&2
  cat "${RELEASE_OUTPUT}" >&2
  exit 1
fi

for REF in "${IMAGE}:${TAG}" "${IMAGE}:${TAG}-amd64" "${IMAGE}:${TAG}-arm64"; do
  IMAGE_STATE="$("${SCRIPT_DIR}"/inspect-ghcr-image-state.sh "${REF}")"
  if [ "${IMAGE_STATE}" != absent ]; then
    echo "Image identity ${REF} already exists." >&2
    exit 1
  fi
done

echo "[OK] tag, release, and GHCR identities for ${TAG} are absent"
