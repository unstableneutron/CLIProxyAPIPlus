#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

die() {
  echo "[docker-evidence] $*" >&2
  exit 1
}

[ "$#" -eq 5 ] \
  || die "usage: $0 <tag> <commit> <run-head-sha> <target-matrix-json> <output-directory>"
TAG=$1
EXPECTED_COMMIT=$2
RUN_HEAD_SHA=$3
TARGET_MATRIX=$4
OUTPUT_DIRECTORY=$5
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_REPOSITORY_ID:?GITHUB_REPOSITORY_ID is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"
: "${GITHUB_RUN_ATTEMPT:?GITHUB_RUN_ATTEMPT is required}"

[ "${GITHUB_REPOSITORY}" = unstableneutron/CLIProxyAPIPlus ] \
  || die "repository identity differs"
[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "commit must be an exact lowercase SHA"
[[ "${RUN_HEAD_SHA}" =~ ^[0-9a-f]{40}$ ]] \
  || die "run head must be an exact lowercase SHA"
[[ "${TAG}" =~ ^v[0-9][0-9A-Za-z._+-]*[-.]unstableneutron\.(0|[1-9][0-9]*)$ ]] \
  || die "release tag is invalid"
for numeric in "${GITHUB_REPOSITORY_ID}" "${GITHUB_RUN_ID}" "${GITHUB_RUN_ATTEMPT}"; do
  [[ "${numeric}" =~ ^[1-9][0-9]*$ ]] \
    || die "repository, run, and attempt identities must be positive integers"
  if [ "${#numeric}" -gt 16 ] || \
     { [ "${#numeric}" -eq 16 ] && [ "${numeric}" -gt 9007199254740991 ]; }; then
    die "repository, run, or attempt identity exceeds the safe integer bound"
  fi
done
[ "${GITHUB_RUN_ATTEMPT}" -le 100 ] \
  || die "workflow attempt exceeds the recovery bound"
if ! jq -e '
  (type == "object") and (keys == ["include"]) and
  (.include | type == "array" and length > 0 and length <= 16) and
  all(.include[];
    (type == "object") and
    (keys | sort) == ["platform", "runner", "tag_suffix"] and
    (.tag_suffix | type == "string" and test("^[a-z0-9]+(?:-[a-z0-9]+)*$")) and
    (.runner | type == "string" and length > 0) and
    (.platform | type == "string" and test("^linux/[a-z0-9]+(?:/[a-z0-9.]+)?$")) and
    (.tag_suffix == (.platform | sub("^linux/"; "") | gsub("/"; "-")))) and
  ([.include[].tag_suffix] | length == (unique | length)) and
  ([.include[].platform] | length == (unique | length))
' <<< "${TARGET_MATRIX}" >/dev/null; then
  die "target matrix differs from the validated contract"
fi

ARTIFACTS=$(gh api --paginate \
  "/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/artifacts?per_page=100" \
  --jq .artifacts | jq -sce 'add') \
  || die "could not enumerate workflow artifacts"
[ "$(jq -r 'type' <<< "${ARTIFACTS}")" = array ] \
  || die "artifact API response differs"
[ "$(jq 'length' <<< "${ARTIFACTS}")" -le 200 ] \
  || die "workflow artifact count exceeds the verification bound"

SELECTED_ATTEMPT=""
SELECTED_ARTIFACTS='[]'
for ((attempt = GITHUB_RUN_ATTEMPT; attempt >= 1; attempt--)); do
  candidate='[]'
  complete=true
  while IFS=$'\t' read -r suffix platform; do
    name="docker-digests-${TAG}-${attempt}-${suffix}"
    matching=$(jq -ce --arg name "${name}" '[.[] | select(.name == $name)]' \
      <<< "${ARTIFACTS}")
    count=$(jq 'length' <<< "${matching}")
    if [ "${count}" -gt 1 ]; then
      die "duplicate artifact identity ${name}"
    fi
    if [ "${count}" -eq 0 ]; then
      complete=false
      continue
    fi
    artifact=$(jq -ce --arg name "${name}" \
      --arg head "${RUN_HEAD_SHA}" \
      --arg platform "${platform}" \
      --arg repository "${GITHUB_REPOSITORY}" \
      --arg suffix "${suffix}" \
      --argjson repository_id "${GITHUB_REPOSITORY_ID}" \
      --argjson run_id "${GITHUB_RUN_ID}" '
        .[0] |
        if (type != "object") or
           (.id | type) != "number" or (.id | floor) != .id or .id <= 0 or .id > 9007199254740991 or
           .name != $name or .expired != false or
           .archive_download_url != ("https://api.github.com/repos/" + $repository +
             "/actions/artifacts/" + (.id | tostring) + "/zip") or
           (.size_in_bytes | type) != "number" or (.size_in_bytes | floor) != .size_in_bytes or
           .size_in_bytes <= 0 or .size_in_bytes > 1048576 or
           (.digest | type) != "string" or (.digest | test("^sha256:[0-9a-f]{64}$") | not) or
           (.workflow_run | type) != "object" or
           .workflow_run.id != $run_id or
           .workflow_run.repository_id != $repository_id or
           .workflow_run.head_repository_id != $repository_id or
           .workflow_run.head_sha != $head
        then error("artifact identity differs")
        else . + {expected_platform: $platform, expected_suffix: $suffix}
        end
      ' <<< "${matching}") \
      || die "artifact identity ${name} differs"
    candidate=$(jq -ce --argjson artifact "${artifact}" '. + [$artifact]' \
      <<< "${candidate}")
  done < <(jq -r '.include[] | [.tag_suffix, .platform] | @tsv' <<< "${TARGET_MATRIX}")
  if [ "${complete}" != true ] || \
     [ "$(jq 'length' <<< "${candidate}")" -ne "$(jq '.include | length' <<< "${TARGET_MATRIX}")" ]; then
    continue
  fi
  if [ "${attempt}" -lt "${GITHUB_RUN_ATTEMPT}" ]; then
    ATTEMPT_API=$(gh api \
      "/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/attempts/${attempt}") \
      || die "could not validate prior workflow attempt ${attempt}"
    if ! jq -e --arg head "${RUN_HEAD_SHA}" \
      --argjson attempt "${attempt}" \
      --argjson repository_id "${GITHUB_REPOSITORY_ID}" \
      --argjson run_id "${GITHUB_RUN_ID}" '
        .id == $run_id and
        .run_attempt == $attempt and
        .head_sha == $head and
        .repository.id == $repository_id and
        .head_repository.id == $repository_id and
        (.conclusion == "failure" or
         .conclusion == "cancelled" or
         .conclusion == "timed_out")
      ' <<< "${ATTEMPT_API}" >/dev/null; then
      die "prior workflow attempt ${attempt} is not recoverable"
    fi
  fi
  SELECTED_ATTEMPT=${attempt}
  SELECTED_ARTIFACTS=${candidate}
  break
done
[ -n "${SELECTED_ATTEMPT}" ] \
  || die "no complete current or recoverable prior digest evidence attempt exists"

mkdir -p "${OUTPUT_DIRECTORY}"
[ -d "${OUTPUT_DIRECTORY}" ] || die "could not create output directory"
[ -z "$(find "${OUTPUT_DIRECTORY}" -mindepth 1 -maxdepth 1 -print -quit)" ] \
  || die "output directory must be empty"
WORK_DIRECTORY=$(mktemp -d)
trap 'rm -rf "${WORK_DIRECTORY}"' EXIT
while IFS= read -r artifact; do
  artifact_id=$(jq -r '.id' <<< "${artifact}")
  artifact_size=$(jq -r '.size_in_bytes' <<< "${artifact}")
  artifact_digest=$(jq -r '.digest' <<< "${artifact}")
  suffix=$(jq -r '.expected_suffix' <<< "${artifact}")
  platform=$(jq -r '.expected_platform' <<< "${artifact}")
  archive="${WORK_DIRECTORY}/${artifact_id}.zip"
  gh api "/repos/${GITHUB_REPOSITORY}/actions/artifacts/${artifact_id}/zip" > "${archive}" \
    || die "could not download artifact ${artifact_id}"
  [ "$(stat -c %s "${archive}")" -eq "${artifact_size}" ] \
    || die "artifact ${artifact_id} download size differs"
  [ "sha256:$(sha256sum "${archive}" | awk '{ print $1 }')" = "${artifact_digest}" ] \
    || die "artifact ${artifact_id} download digest differs"
  extracted="${WORK_DIRECTORY}/extracted-${artifact_id}"
  "${SCRIPT_DIR}/extract-staged-release-artifact.py" \
    "${archive}" "${extracted}" docker-digest-evidence.json:4096
  evidence="${extracted}/docker-digest-evidence.json"
  if ! jq -e --arg repository "${GITHUB_REPOSITORY}" \
    --arg tag "${TAG}" \
    --arg commit "${EXPECTED_COMMIT}" \
    --arg run_id "${GITHUB_RUN_ID}" \
    --arg suffix "${suffix}" \
    --arg platform "${platform}" \
    --argjson attempt "${SELECTED_ATTEMPT}" '
      (type == "object") and
      (keys | sort) == [
        "architecture_digest", "commit", "platform", "repository",
        "run_attempt", "run_id", "schema_version", "source_digest",
        "tag", "tag_suffix"
      ] and
      .schema_version == 1 and
      .repository == $repository and .tag == $tag and .commit == $commit and
      .run_id == $run_id and .run_attempt == $attempt and
      .tag_suffix == $suffix and .platform == $platform and
      (.source_digest | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
      (.architecture_digest | type == "string" and test("^sha256:[0-9a-f]{64}$"))
    ' "${evidence}" >/dev/null; then
    die "artifact ${artifact_id} evidence payload differs"
  fi
  cp "${evidence}" "${OUTPUT_DIRECTORY}/${suffix}.json"
done < <(jq -c '.[]' <<< "${SELECTED_ARTIFACTS}")

[ "$(find "${OUTPUT_DIRECTORY}" -mindepth 1 -maxdepth 1 -type f -name '*.json' | wc -l)" -eq \
  "$(jq '.include | length' <<< "${TARGET_MATRIX}")" ] \
  || die "selected digest evidence set is incomplete"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "evidence_attempt=${SELECTED_ATTEMPT}" >> "${GITHUB_OUTPUT}"
fi
echo "[OK] authenticated Docker digest evidence from run ${GITHUB_RUN_ID} attempt ${SELECTED_ATTEMPT}."
