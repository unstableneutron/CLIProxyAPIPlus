#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
EXTRACTOR="${SCRIPT_DIR}/extract-staged-release-artifact.py"
# shellcheck source=/dev/null
source "${SCRIPT_DIR}/release-assets.sh"

die() {
  echo "[staged-release-publisher] $*" >&2
  exit 1
}

[ "$#" -eq 8 ] || die "usage: $0 <tag> <commit> <allowed-receipt-name> <artifact-id> <artifact-name> <artifact-digest> <run-id> <run-head-sha>"
TAG=$1
EXPECTED_COMMIT=$2
ALLOWED_RECEIPT_NAME=$3
INPUT_ARTIFACT_ID=$4
INPUT_ARTIFACT_NAME=$5
INPUT_ARTIFACT_DIGEST=$6
WORKFLOW_RUN_ID=$7
WORKFLOW_HEAD_SHA=$8
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] || die "expected commit is invalid"
[[ "${WORKFLOW_HEAD_SHA}" =~ ^[0-9a-f]{40}$ ]] || die "workflow head SHA is invalid"
[[ "${WORKFLOW_RUN_ID}" =~ ^[1-9][0-9]*$ ]] || die "workflow run ID is invalid"
case "${ALLOWED_RECEIPT_NAME}" in
  upstream-sync-receipt.json|hotfix-release-receipt.json) ;;
  *) die "unsupported receipt kind ${ALLOWED_RECEIPT_NAME}" ;;
esac

EXPECTED_ASSETS=$(expected_release_assets_json "${TAG}") \
  || die "could not derive expected assets for ${TAG}"
RELEASE_FILE=$(mktemp)
CANONICAL_FILE=$(mktemp)
RESPONSE=$(mktemp)
ARTIFACT_FILE=$(mktemp)
ARTIFACT_ZIP=$(mktemp)
EXTRACTED=$(mktemp -d)
rm -rf "${EXTRACTED}"
trap 'rm -f "${RELEASE_FILE}" "${CANONICAL_FILE}" "${RESPONSE}" "${ARTIFACT_FILE}" "${ARTIFACT_ZIP}"; rm -rf "${EXTRACTED}"' EXIT

fetch_release() {
  : > "${RESPONSE}"
  if gh api --include "/repos/${GITHUB_REPOSITORY}/releases/tags/${TAG}" > "${RESPONSE}" 2>&1; then
    sed '1,/^\r\{0,1\}$/d' "${RESPONSE}" > "${RELEASE_FILE}"
    local release_id
    release_id=$(jq -er '.id | select(type == "number" and floor == . and . > 0 and . <= 9007199254740991)' "${RELEASE_FILE}") \
      || die "release ${TAG} ID is invalid"
    gh api "/repos/${GITHUB_REPOSITORY}/releases/${release_id}" > "${CANONICAL_FILE}"
    diff -u \
      <(jq -S '{id,tag_name,html_url,assets_url,draft,prerelease,target_commitish,author,body,assets}' "${RELEASE_FILE}") \
      <(jq -S '{id,tag_name,html_url,assets_url,draft,prerelease,target_commitish,author,body,assets}' "${CANONICAL_FILE}") >/dev/null \
      || die "canonical release ${TAG} differs from its tag lookup"
    cp "${CANONICAL_FILE}" "${RELEASE_FILE}"
    return 0
  fi
  mapfile -t statuses < <(sed -nE 's/^HTTP\/[0-9.]+ ([0-9]{3})( .*)?\r?$/\1/p' "${RESPONSE}")
  if [ "${#statuses[@]}" -eq 1 ] && [ "${statuses[0]}" = 404 ]; then
    return 4
  fi
  cat "${RESPONSE}" >&2
  die "could not determine release state for ${TAG}"
}

release_exists=false
if fetch_release; then
  release_exists=true
fi

ARTIFACT_ID=${INPUT_ARTIFACT_ID}
ARTIFACT_NAME=${INPUT_ARTIFACT_NAME}
ARTIFACT_DIGEST=${INPUT_ARTIFACT_DIGEST}
if [ "${release_exists}" = true ]; then
  BODY=$(jq -er '.body | select(type == "string")' "${RELEASE_FILE}") \
    || die "release ${TAG} lacks staged evidence"
  PREFIX='<!-- cliproxy-staged-release:v1 '
  SUFFIX=' -->'
  [[ "${BODY}" == "${PREFIX}"*"${SUFFIX}" ]] \
    || die "release ${TAG} staged evidence differs"
  EVIDENCE=${BODY#"${PREFIX}"}
  EVIDENCE=${EVIDENCE%"${SUFFIX}"}
  jq -e 'keys == ["artifact_digest","artifact_id","artifact_name","workflow_run_attempt","workflow_run_id"] and
      (.artifact_id | type) == "string" and (.artifact_id | test("^[1-9][0-9]*$")) and
      (.artifact_digest | type) == "string" and (.artifact_digest | test("^sha256:[0-9a-f]{64}$")) and
      (.artifact_name | type) == "string" and
      (.workflow_run_id | type) == "string" and (.workflow_run_id | test("^[1-9][0-9]*$")) and
      (.workflow_run_attempt | type) == "string" and (.workflow_run_attempt | test("^[1-9][0-9]*$"))' \
    <<< "${EVIDENCE}" >/dev/null || die "release ${TAG} staged evidence is malformed"
  ARTIFACT_ID=$(jq -r '.artifact_id' <<< "${EVIDENCE}")
  ARTIFACT_NAME=$(jq -r '.artifact_name' <<< "${EVIDENCE}")
  ARTIFACT_DIGEST=$(jq -r '.artifact_digest' <<< "${EVIDENCE}")
  [ "$(jq -r '.workflow_run_id' <<< "${EVIDENCE}")" = "${WORKFLOW_RUN_ID}" ] \
    || die "release ${TAG} staged evidence belongs to another workflow run"
fi

[[ "${ARTIFACT_ID}" =~ ^[1-9][0-9]*$ ]] || die "staged artifact ID is invalid"
[[ "${ARTIFACT_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "staged artifact digest is invalid"
[[ "${ARTIFACT_NAME}" =~ ^staged-release-assets-${WORKFLOW_RUN_ID}-([1-9][0-9]*)$ ]] \
  || die "staged artifact name is invalid"
ARTIFACT_ATTEMPT=${BASH_REMATCH[1]}
gh api "/repos/${GITHUB_REPOSITORY}/actions/artifacts/${ARTIFACT_ID}" > "${ARTIFACT_FILE}"
jq -e \
  --arg name "${ARTIFACT_NAME}" \
  --arg digest "${ARTIFACT_DIGEST}" \
  --argjson id "${ARTIFACT_ID}" \
  --argjson run_id "${WORKFLOW_RUN_ID}" \
  --argjson repo_id 1247056725 \
  --arg head "${WORKFLOW_HEAD_SHA}" '
    .id == $id and .name == $name and .digest == $digest and .expired == false and
    (.size_in_bytes | type) == "number" and (.size_in_bytes | floor) == .size_in_bytes and
    .size_in_bytes > 0 and .size_in_bytes <= 2000000000 and
    .archive_download_url == ("https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/actions/artifacts/" + ($id | tostring) + "/zip") and
    .workflow_run.id == $run_id and .workflow_run.repository_id == $repo_id and
    .workflow_run.head_repository_id == $repo_id and .workflow_run.head_sha == $head
  ' "${ARTIFACT_FILE}" >/dev/null || die "staged artifact identity differs"
gh api -H 'Accept: application/vnd.github+json' \
  "/repos/${GITHUB_REPOSITORY}/actions/artifacts/${ARTIFACT_ID}/zip" > "${ARTIFACT_ZIP}"
[ "$(stat -c %s "${ARTIFACT_ZIP}")" -eq "$(jq -r '.size_in_bytes' "${ARTIFACT_FILE}")" ] \
  || die "staged artifact archive size differs"
[ "sha256:$(sha256sum "${ARTIFACT_ZIP}" | awk '{ print $1 }')" = "${ARTIFACT_DIGEST}" ] \
  || die "staged artifact archive digest differs"

EXTRACT_ARGS=(release-manifest.json:1000000)
while IFS= read -r asset_name; do
  EXTRACT_ARGS+=("${asset_name}:2000000000")
done < <(jq -r '.[]' <<< "${EXPECTED_ASSETS}")
python3 "${EXTRACTOR}" "${ARTIFACT_ZIP}" "${EXTRACTED}" "${EXTRACT_ARGS[@]}"
MANIFEST="${EXTRACTED}/release-manifest.json"
jq -e \
  --arg repository "${GITHUB_REPOSITORY}" \
  --arg tag "${TAG}" \
  --arg commit "${EXPECTED_COMMIT}" \
  --arg receipt "${ALLOWED_RECEIPT_NAME}" \
  --arg run_id "${WORKFLOW_RUN_ID}" \
  --arg attempt "${ARTIFACT_ATTEMPT}" \
  --argjson expected "${EXPECTED_ASSETS}" '
    keys == ["assets","commit","receipt_name","repository","schema_version","tag","workflow_run_attempt","workflow_run_id"] and
    .schema_version == 1 and .repository == $repository and .tag == $tag and
    .commit == $commit and .receipt_name == $receipt and
    .workflow_run_id == $run_id and .workflow_run_attempt == $attempt and
    (.assets | keys) == $expected and all(.assets[];
      keys == ["digest","size"] and
      (.size | type) == "number" and (.size | floor) == .size and .size > 0 and .size <= 2000000000 and
      (.digest | type) == "string" and (.digest | test("^sha256:[0-9a-f]{64}$")))
  ' "${MANIFEST}" >/dev/null || die "staged release manifest differs"
while IFS= read -r asset_name; do
  asset_path="${EXTRACTED}/${asset_name}"
  [ "$(stat -c %s "${asset_path}")" -eq "$(jq -r --arg name "${asset_name}" '.assets[$name].size' "${MANIFEST}")" ] \
    || die "staged asset ${asset_name} size differs"
  [ "sha256:$(sha256sum "${asset_path}" | awk '{ print $1 }')" = "$(jq -r --arg name "${asset_name}" '.assets[$name].digest' "${MANIFEST}")" ] \
    || die "staged asset ${asset_name} digest differs"
done < <(jq -r '.[]' <<< "${EXPECTED_ASSETS}")

EVIDENCE=$(jq -Scn \
  --arg artifact_id "${ARTIFACT_ID}" \
  --arg artifact_name "${ARTIFACT_NAME}" \
  --arg artifact_digest "${ARTIFACT_DIGEST}" \
  --arg workflow_run_id "${WORKFLOW_RUN_ID}" \
  --arg workflow_run_attempt "${ARTIFACT_ATTEMPT}" '{
    artifact_id: $artifact_id,
    artifact_name: $artifact_name,
    artifact_digest: $artifact_digest,
    workflow_run_id: $workflow_run_id,
    workflow_run_attempt: $workflow_run_attempt
  }')
EXPECTED_BODY="<!-- cliproxy-staged-release:v1 ${EVIDENCE} -->"

revalidate_target() {
  local tag_commit main_commit
  tag_commit=$(gh api "/repos/${GITHUB_REPOSITORY}/commits/${TAG}" --jq .sha)
  main_commit=$(gh api "/repos/${GITHUB_REPOSITORY}/commits/main" --jq .sha)
  if [ "${tag_commit}" != "${EXPECTED_COMMIT}" ] || [ "${main_commit}" != "${EXPECTED_COMMIT}" ]; then
    die "tag or main moved before release mutation"
  fi
}

validate_release() {
  local required_state=$1
  jq -e \
    --arg tag "${TAG}" \
    --arg repo "${GITHUB_REPOSITORY}" \
    --arg receipt "${ALLOWED_RECEIPT_NAME}" \
    --arg state "${required_state}" \
    --arg body "${EXPECTED_BODY}" \
    --argjson expected "${EXPECTED_ASSETS}" '
      .tag_name == $tag and .prerelease == false and .body == $body and
      .target_commitish == "main" and
      .html_url == ("https://github.com/" + $repo + "/releases/tag/" + $tag) and
      .assets_url == ("https://api.github.com/repos/" + $repo + "/releases/" + (.id | tostring) + "/assets") and
      .author.login == "github-actions[bot]" and .author.id == 41898282 and .author.type == "Bot" and
      (($state == "draft" and .draft == true) or ($state == "stable" and .draft == false)) and
      (.assets | type) == "array" and
      ([.assets[].name] | length) == ([.assets[].name] | unique | length) and
      all(.assets[];
        ((.name as $name | ($expected | index($name)) != null) or .name == $receipt) and
        (.id | type) == "number" and (.id | floor) == .id and .id > 0 and .id <= 9007199254740991 and
        (.size | type) == "number" and (.size | floor) == .size and .size > 0 and .size <= 2000000000 and
        .state == "uploaded" and
        .url == ("https://api.github.com/repos/" + $repo + "/releases/assets/" + (.id | tostring)) and
        .uploader.login == "github-actions[bot]" and .uploader.id == 41898282 and .uploader.type == "Bot" and
        (.digest | type) == "string" and (.digest | test("^sha256:[0-9a-f]{64}$"))) and
      (if $state == "draft"
       then ([.assets[] | select(.name == $receipt)] | length) == 0
       else ([.assets[] | select(.name == $receipt)] | length) <= 1
       end)
    ' "${RELEASE_FILE}" >/dev/null || die "release ${TAG} has a conflicting ${required_state} identity"
}

validate_asset_bytes() {
  while IFS=$'\t' read -r name size digest; do
    [ -n "${name}" ] || continue
    expected=$(jq -ce --arg name "${name}" '.assets[$name] // empty' "${MANIFEST}")
    [ -n "${expected}" ] || continue
    if [ "$(jq -r '.size' <<< "${expected}")" != "${size}" ] || \
      [ "$(jq -r '.digest' <<< "${expected}")" != "${digest}" ]; then
      die "existing release asset ${name} differs from immutable staged bytes"
    fi
  done < <(jq -r '.assets[] | [.name, (.size | tostring), .digest] | @tsv' "${RELEASE_FILE}")
}

if [ "${release_exists}" = false ]; then
  revalidate_target
  CREATE_INPUT=$(mktemp)
  jq -n \
    --arg tag "${TAG}" \
    --arg body "${EXPECTED_BODY}" '{
      tag_name: $tag, target_commitish: "main", name: $tag,
      body: $body, draft: true, prerelease: false
    }' > "${CREATE_INPUT}"
  if ! gh api --method POST "/repos/${GITHUB_REPOSITORY}/releases" --input "${CREATE_INPUT}" >/dev/null; then
    fetch_release || die "draft creation outcome is unknown"
  else
    fetch_release || die "new draft release is not visible"
  fi
  rm -f "${CREATE_INPUT}"
fi

RELEASE_STATE=$(jq -r 'if .draft then "draft" else "stable" end' "${RELEASE_FILE}")
validate_release "${RELEASE_STATE}"
validate_asset_bytes
if [ "${RELEASE_STATE}" = draft ]; then
  while IFS= read -r asset_name; do
    if jq -e --arg name "${asset_name}" 'any(.assets[]; .name == $name)' "${RELEASE_FILE}" >/dev/null; then
      continue
    fi
    revalidate_target
    if ! gh release upload "${TAG}" "${EXTRACTED}/${asset_name}" --repo "${GITHUB_REPOSITORY}"; then
      fetch_release || die "asset upload outcome for ${asset_name} is unknown"
      validate_release draft
      validate_asset_bytes
      jq -e --arg name "${asset_name}" 'any(.assets[]; .name == $name)' "${RELEASE_FILE}" >/dev/null \
        || die "asset upload outcome for ${asset_name} is unknown"
    else
      fetch_release || die "draft release disappeared during publication"
      validate_release draft
      validate_asset_bytes
    fi
  done < <(jq -r '.[]' <<< "${EXPECTED_ASSETS}")
  [ "$(jq -c '[.assets[].name] | sort' "${RELEASE_FILE}")" = "${EXPECTED_ASSETS}" ] \
    || die "draft release is incomplete after staged upload"
  revalidate_target
  RELEASE_ID=$(jq -r '.id' "${RELEASE_FILE}")
  if ! gh api --method PATCH "/repos/${GITHUB_REPOSITORY}/releases/${RELEASE_ID}" \
    -f draft=false >/dev/null; then
    fetch_release || die "release publication outcome is unknown"
    [ "$(jq -r '.draft' "${RELEASE_FILE}")" = false ] \
      || die "release publication outcome is unknown"
  else
    fetch_release || die "published release is not visible"
  fi
  validate_release stable
  validate_asset_bytes
fi

[ "$(jq -c '[.assets[].name | select(. != "upstream-sync-receipt.json" and . != "hotfix-release-receipt.json")] | sort' "${RELEASE_FILE}")" = "${EXPECTED_ASSETS}" ] \
  || die "stable release asset set differs"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "release_url=$(jq -r '.html_url' "${RELEASE_FILE}")"
    echo "asset_names_json=$(jq -c '[.assets[].name]' "${RELEASE_FILE}")"
  } >> "${GITHUB_OUTPUT}"
fi
echo "[OK] published or adopted exact staged release ${TAG}"
