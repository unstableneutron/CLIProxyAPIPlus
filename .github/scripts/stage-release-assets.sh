#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "${SCRIPT_DIR}/release-assets.sh"

die() {
  echo "[release-stager] $*" >&2
  exit 1
}

[ "$#" -eq 5 ] \
  || die "usage: $0 <tag> <commit> <allowed-receipt-name> <dist-directory> <manifest>"
TAG=$1
EXPECTED_COMMIT=$2
ALLOWED_RECEIPT_NAME=$3
DIST=$4
MANIFEST=$5
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"
: "${GITHUB_RUN_ATTEMPT:?GITHUB_RUN_ATTEMPT is required}"
[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "commit must be a lowercase 40-character SHA"
[[ "${GITHUB_RUN_ID}" =~ ^[1-9][0-9]*$ ]] \
  || die "workflow run ID must be a positive decimal integer"
[[ "${GITHUB_RUN_ATTEMPT}" =~ ^[1-9][0-9]*$ ]] \
  || die "workflow run attempt must be a positive decimal integer"
case "${ALLOWED_RECEIPT_NAME}" in
  upstream-sync-receipt.json|hotfix-release-receipt.json) ;;
  *) die "unsupported receipt kind ${ALLOWED_RECEIPT_NAME}" ;;
esac
[ -d "${DIST}" ] || die "staged release directory is missing"

EXPECTED_ASSETS=$(expected_release_assets_json "${TAG}") \
  || die "could not derive expected assets for ${TAG}"
ACTUAL_ASSETS=$(find "${DIST}" -maxdepth 1 -type f \
  \( -name '*.tar.gz' -o -name '*.zip' -o -name checksums.txt \) \
  -printf '%f\n' | LC_ALL=C sort | jq -Rsc 'split("\n") | map(select(length > 0))')
[ "${ACTUAL_ASSETS}" = "${EXPECTED_ASSETS}" ] \
  || die "staged release asset set differs"

ASSET_IDENTITIES='{}'
while IFS= read -r asset_name; do
  asset_path="${DIST}/${asset_name}"
  [ -f "${asset_path}" ] || die "staged asset ${asset_name} is missing"
  asset_size=$(stat -c %s "${asset_path}")
  if [ "${asset_size}" -le 0 ] || [ "${asset_size}" -gt 2000000000 ]; then
    die "staged asset ${asset_name} has an invalid size"
  fi
  asset_digest="sha256:$(sha256sum "${asset_path}" | awk '{ print $1 }')"
  ASSET_IDENTITIES=$(jq -ce \
    --arg name "${asset_name}" \
    --arg digest "${asset_digest}" \
    --argjson size "${asset_size}" \
    '. + {($name): {size: $size, digest: $digest}}' \
    <<< "${ASSET_IDENTITIES}")
done < <(jq -r '.[]' <<< "${EXPECTED_ASSETS}")

SEEN=$(mktemp)
TEMP=$(mktemp "${MANIFEST}.tmp.XXXXXX")
trap 'rm -f "${SEEN}" "${TEMP}"' EXIT
: > "${SEEN}"
CHECKSUM_PATTERN='^([0-9a-f]{64})  ([A-Za-z0-9][A-Za-z0-9._+-]*\.(tar\.gz|zip))$'
while IFS= read -r line || [ -n "${line}" ]; do
  [[ "${line}" =~ ${CHECKSUM_PATTERN} ]] \
    || die "staged checksums.txt is malformed"
  digest="sha256:${BASH_REMATCH[1]}"
  name=${BASH_REMATCH[2]}
  grep -Fxq "${name}" "${SEEN}" && die "staged checksums.txt has duplicate entries"
  echo "${name}" >> "${SEEN}"
  [ "$(jq -r --arg name "${name}" '.[$name].digest // empty' <<< "${ASSET_IDENTITIES}")" = "${digest}" ] \
    || die "staged checksum for ${name} differs"
done < "${DIST}/checksums.txt"
[ "$(wc -l < "${SEEN}" | tr -d ' ')" -eq "$(jq 'length - 1' <<< "${EXPECTED_ASSETS}")" ] \
  || die "staged checksums.txt does not cover every archive"

jq -S -n \
  --arg repository "${GITHUB_REPOSITORY}" \
  --arg tag "${TAG}" \
  --arg commit "${EXPECTED_COMMIT}" \
  --arg receipt_name "${ALLOWED_RECEIPT_NAME}" \
  --arg workflow_run_id "${GITHUB_RUN_ID}" \
  --arg workflow_run_attempt "${GITHUB_RUN_ATTEMPT}" \
  --argjson assets "${ASSET_IDENTITIES}" '{
    schema_version: 1,
    repository: $repository,
    tag: $tag,
    commit: $commit,
    receipt_name: $receipt_name,
    workflow_run_id: $workflow_run_id,
    workflow_run_attempt: $workflow_run_attempt,
    assets: $assets
  }' > "${TEMP}"
mv "${TEMP}" "${MANIFEST}"
rm -f "${SEEN}"
trap - EXIT
