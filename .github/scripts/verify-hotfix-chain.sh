#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
UPSTREAM_VERIFIER="${SCRIPT_DIR}/verify-upstream-release.sh"
MAX_HOTFIX_DEPTH=32
REPOSITORY_ID=1247056725
OWNER_LOGIN=unstableneutron
OWNER_ID=156744497
BOT_LOGIN='github-actions[bot]'
BOT_ID=41898282

die() {
  echo "[hotfix-chain-verifier] $*" >&2
  exit 1
}

TAG=""
EXPECTED_COMMIT=""
PARENT_TAG=""
EXPECTED_PARENT_COMMIT=""
EXPECTED_SYNC_ID=""
EXPECTED_PLAN_FINGERPRINT=""
IMAGE=""
OUTPUT=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --tag) TAG=${2:-}; shift 2 ;;
    --expected-commit) EXPECTED_COMMIT=${2:-}; shift 2 ;;
    --parent-tag) PARENT_TAG=${2:-}; shift 2 ;;
    --expected-parent-commit) EXPECTED_PARENT_COMMIT=${2:-}; shift 2 ;;
    --expected-sync-id) EXPECTED_SYNC_ID=${2:-}; shift 2 ;;
    --expected-plan-fingerprint) EXPECTED_PLAN_FINGERPRINT=${2:-}; shift 2 ;;
    --image) IMAGE=${2:-}; shift 2 ;;
    --output) OUTPUT=${2:-}; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

release_parts() {
  local tag=$1
  if [[ "${tag}" =~ ^(v[0-9]+\.[0-9]+\.[0-9]+-unstableneutron)\.([0-9]+)$ ]]; then
    RELEASE_PREFIX=${BASH_REMATCH[1]}
    RELEASE_SUFFIX=$((10#${BASH_REMATCH[2]}))
    return 0
  fi
  return 1
}

release_parts "${TAG}" || die "--tag must be a fork release tag"
CANDIDATE_PREFIX=${RELEASE_PREFIX}
CANDIDATE_SUFFIX=${RELEASE_SUFFIX}
release_parts "${PARENT_TAG}" || die "--parent-tag must be a fork release tag"
[ "${RELEASE_PREFIX}" = "${CANDIDATE_PREFIX}" ] \
  || die "candidate and parent release lines differ"
[ "${CANDIDATE_SUFFIX}" -eq $((RELEASE_SUFFIX + 1)) ] \
  || die "parent ${PARENT_TAG} is not the immediate predecessor of ${TAG}"
[ "${CANDIDATE_SUFFIX}" -ge 1 ] \
  || die "hotfix verification requires suffix .1 or later"
[[ "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-commit must be a 40-character lowercase commit"
[[ "${EXPECTED_PARENT_COMMIT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-parent-commit must be a 40-character lowercase commit"
[ -n "${EXPECTED_SYNC_ID}" ] || die "--expected-sync-id is required"
[[ "${EXPECTED_PLAN_FINGERPRINT}" =~ ^[0-9a-f]{40}$ ]] \
  || die "--expected-plan-fingerprint must be a 40-character lowercase hash"
[ -n "${IMAGE}" ] || die "--image is required"
[ -n "${OUTPUT}" ] || die "--output is required"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
[ "${GITHUB_REPOSITORY}" = unstableneutron/CLIProxyAPIPlus ] \
  || die "unexpected repository ${GITHUB_REPOSITORY}"

ROOT=$(mktemp -d)
VISITED_TAGS="${ROOT}/visited-tags"
VISITED_COMMITS="${ROOT}/visited-commits"
STATE_FILE="${ROOT}/expected-state.env"
ROOT_LINK="${ROOT}/root-link.json"
trap 'rm -rf "${ROOT}"' EXIT
: > "${VISITED_TAGS}"
: > "${VISITED_COMMITS}"

git show "${EXPECTED_COMMIT}:.ccs-fork-upstream.env" > "${STATE_FILE}" \
  || die "candidate commit does not contain upstream-sync state"
state_value() {
  local key=$1
  awk -F= -v key="${key}" \
    '$1 == key { sub(/^[^=]*=/, ""); print; exit }' \
    "${STATE_FILE}"
}
STATE_SYNC_ID=$(state_value SYNC_ID)
STATE_FINGERPRINT=$(state_value PLAN_FINGERPRINT)
ROOT_TAG=$(state_value EXPECTED_FORK_TAG)
[ "${STATE_SYNC_ID}" = "${EXPECTED_SYNC_ID}" ] \
  || die "candidate upstream-sync ID differs"
[ "${STATE_FINGERPRINT}" = "${EXPECTED_PLAN_FINGERPRINT}" ] \
  || die "candidate upstream-sync fingerprint differs"
release_parts "${ROOT_TAG}" || die "upstream-sync state records an invalid release tag"
if [ "${RELEASE_PREFIX}" != "${CANDIDATE_PREFIX}" ] || [ "${RELEASE_SUFFIX}" -ne 0 ]; then
  die "receipt chain root must be ${CANDIDATE_PREFIX}.0"
fi

json_exact_keys() {
  local file=$1
  local expression=$2
  local label=$3
  jq -e "${expression}" "${file}" >/dev/null \
    || die "${label} schema differs"
}

verify_release_and_link() {
  local tag=$1
  local commit=$2
  local kind=$3
  local output=$4
  local node_dir="${ROOT}/node-${tag}"
  local receipt_name workflow_path expected_tagger expected_email expected_message
  mkdir -p "${node_dir}"
  if [ "${kind}" = upstream ]; then
    receipt_name=upstream-sync-receipt.json
    workflow_path=.github/workflows/upstream-sync-v2.yml
    expected_tagger='cliproxy-upstream-sync[bot]'
    expected_email='cliproxy-upstream-sync@users.noreply.github.com'
    expected_message="Release ${tag}"
  else
    receipt_name=hotfix-release-receipt.json
    workflow_path=.github/workflows/hotfix-release.yml
    expected_tagger='cliproxy-hotfix-release[bot]'
    expected_email='cliproxy-hotfix-release@users.noreply.github.com'
    expected_message="Hotfix release ${tag} after "
  fi

  git rev-parse --verify "refs/tags/${tag}^{commit}" >/dev/null 2>&1 \
    || die "chain tag ${tag} is not fetched"
  [ "$(git cat-file -t "refs/tags/${tag}")" = tag ] \
    || die "chain tag ${tag} must be annotated"
  [ "$(git rev-parse "refs/tags/${tag}^{}")" = "${commit}" ] \
    || die "chain tag ${tag} does not peel to recorded commit ${commit}"
  local tagger_name tagger_email tag_message
  tagger_name=$(git for-each-ref --format='%(taggername)' "refs/tags/${tag}")
  tagger_email=$(git for-each-ref --format='%(taggeremail)' "refs/tags/${tag}" | sed -E 's/^<|>$//g')
  tag_message=$(git for-each-ref --format='%(contents)' "refs/tags/${tag}")
  if [ "${tagger_name}" != "${expected_tagger}" ] || [ "${tagger_email}" != "${expected_email}" ]; then
    die "chain tag ${tag} has an unexpected tagger"
  fi
  if [ "${kind}" = upstream ]; then
    [ "${tag_message}" = "${expected_message}" ] \
      || die "chain tag ${tag} has an unexpected message"
  else
    [[ "${tag_message}" == "${expected_message}"* ]] \
      || die "chain tag ${tag} has an unexpected message"
  fi

  local remote_commit
  remote_commit=$(gh api "repos/${GITHUB_REPOSITORY}/commits/${tag}" --jq .sha)
  [ "${remote_commit}" = "${commit}" ] \
    || die "remote tag ${tag} resolves to ${remote_commit}, expected ${commit}"

  local release_file="${node_dir}/release.json"
  gh api "repos/${GITHUB_REPOSITORY}/releases/tags/${tag}" > "${release_file}"
  jq -e \
    --arg tag "${tag}" \
    --arg repo "${GITHUB_REPOSITORY}" \
    --arg login "${BOT_LOGIN}" \
    --argjson bot_id "${BOT_ID}" '
      .tag_name == $tag and .draft == false and .prerelease == false and
      .target_commitish == "main" and
      .html_url == ("https://github.com/" + $repo + "/releases/tag/" + $tag) and
      .author.login == $login and .author.id == $bot_id and
      (.assets | type) == "array" and
      ([.assets[].name] | length) == ([.assets[].name] | unique | length)
    ' "${release_file}" >/dev/null \
    || die "release ${tag} is missing, mutable, duplicated, or has an unexpected identity"
  local release_id canonical_file="${node_dir}/canonical.json"
  release_id=$(jq -r '.id' "${release_file}")
  gh api "repos/${GITHUB_REPOSITORY}/releases/${release_id}" > "${canonical_file}"
  diff -u \
    <(jq -S '{id,tag_name,html_url,assets_url,published_at,draft,prerelease,target_commitish,author,assets}' "${release_file}") \
    <(jq -S '{id,tag_name,html_url,assets_url,published_at,draft,prerelease,target_commitish,author,assets}' "${canonical_file}") >/dev/null \
    || die "canonical release ${tag} differs from its tag lookup"

  local receipt_count unknown_count receipt_id receipt_digest
  receipt_count=$(jq --arg name "${receipt_name}" '[.assets[] | select(.name == $name)] | length' "${release_file}")
  unknown_count=$(jq '[.assets[] | select(
      .name != "checksums.txt" and
      .name != "upstream-sync-receipt.json" and
      .name != "hotfix-release-receipt.json" and
      (.name | test("^CLIProxyAPIPlus_[A-Za-z0-9._+-]+\\.(tar\\.gz|zip)$") | not)
    )] | length' "${release_file}")
  if [ "${receipt_count}" -ne 1 ] || [ "${unknown_count}" -ne 0 ]; then
    die "release ${tag} has an incomplete or unexpected asset set"
  fi
  [ "$(jq '[.assets[] | select(.name == "checksums.txt")] | length' "${release_file}")" -eq 1 ] \
    || die "release ${tag} must contain exactly one checksums.txt"
  [ "$(jq '[.assets[] | select(.name | test("^CLIProxyAPIPlus_[A-Za-z0-9._+-]+\\.(tar\\.gz|zip)$"))] | length' "${release_file}")" -gt 0 ] \
    || die "release ${tag} has no archives"
  [ "$(jq '[.assets[] | select(.name == "upstream-sync-receipt.json" or .name == "hotfix-release-receipt.json")] | length' "${release_file}")" -eq 1 ] \
    || die "release ${tag} has a wrong or duplicate receipt"
  jq -e 'all(.assets[]; (.id | type) == "number" and (.id > 0) and
      (.size | type) == "number" and (.size > 0) and
      (.digest | type) == "string" and (.digest | test("^sha256:[0-9a-f]{64}$")))' \
    "${release_file}" >/dev/null \
    || die "release ${tag} has an invalid asset identity"

  receipt_id=$(jq -r --arg name "${receipt_name}" '.assets[] | select(.name == $name) | .id' "${release_file}")
  receipt_digest=$(jq -r --arg name "${receipt_name}" '.assets[] | select(.name == $name) | .digest' "${release_file}")
  gh api -H 'Accept: application/octet-stream' \
    "repos/${GITHUB_REPOSITORY}/releases/assets/${receipt_id}" > "${node_dir}/${receipt_name}"
  [ "sha256:$(sha256sum "${node_dir}/${receipt_name}" | awk '{ print $1 }')" = "${receipt_digest}" ] \
    || die "receipt bytes for ${tag} do not match the release asset digest"
  jq -e . "${node_dir}/${receipt_name}" >/dev/null \
    || die "release ${tag} receipt is malformed"

  local checksum_id checksum_digest checksum_file="${node_dir}/checksums.txt"
  checksum_id=$(jq -r '.assets[] | select(.name == "checksums.txt") | .id' "${release_file}")
  checksum_digest=$(jq -r '.assets[] | select(.name == "checksums.txt") | .digest' "${release_file}")
  gh api -H 'Accept: application/octet-stream' \
    "repos/${GITHUB_REPOSITORY}/releases/assets/${checksum_id}" > "${checksum_file}"
  [ "sha256:$(sha256sum "${checksum_file}" | awk '{ print $1 }')" = "${checksum_digest}" ] \
    || die "checksums.txt bytes for ${tag} do not match the release asset digest"
  local seen_checksums="${node_dir}/checksum-names"
  : > "${seen_checksums}"
  while IFS= read -r line || [ -n "${line}" ]; do
    [[ "${line}" =~ ^([0-9a-f]{64})[[:space:]][[:space:]]([A-Za-z0-9][A-Za-z0-9._+-]*\.(tar\.gz|zip))$ ]] \
      || die "checksums.txt for ${tag} is malformed"
    local expected_digest="sha256:${BASH_REMATCH[1]}" asset_name=${BASH_REMATCH[2]} api_digest
    grep -Fxq "${asset_name}" "${seen_checksums}" && die "checksums.txt for ${tag} has duplicate entries"
    echo "${asset_name}" >> "${seen_checksums}"
    api_digest=$(jq -r --arg name "${asset_name}" '.assets[] | select(.name == $name) | .digest' "${release_file}")
    [ "${api_digest}" = "${expected_digest}" ] \
      || die "checksum for ${asset_name} does not match its release asset digest"
  done < "${checksum_file}"
  [ "$(wc -l < "${seen_checksums}" | tr -d ' ')" -eq "$(jq '[.assets[] | select(.name | test("^CLIProxyAPIPlus_[A-Za-z0-9._+-]+\\.(tar\\.gz|zip)$"))] | length' "${release_file}")" ] \
    || die "checksums.txt for ${tag} does not cover every archive"

  local receipt_file="${node_dir}/${receipt_name}" run_id run_attempt run_head
  run_id=$(jq -r '.workflow_run_id // empty' "${receipt_file}")
  [[ "${run_id}" =~ ^[1-9][0-9]*$ ]] || die "receipt for ${tag} has an invalid workflow run ID"
  local regenerated="${node_dir}/regenerated.json"
  GITHUB_RUN_ID="${run_id}" "${UPSTREAM_VERIFIER}" \
    --tag "${tag}" \
    --expected-commit "${commit}" \
    --expected-sync-id "${EXPECTED_SYNC_ID}" \
    --expected-plan-fingerprint "${EXPECTED_PLAN_FINGERPRINT}" \
    --image "${IMAGE}" \
    --main-policy descendant \
    --require-architecture-tags true \
    --require-latest-parity false \
    --receipt "${regenerated}" >/dev/null
  diff -u \
    <(jq -S '{schema_version,sync_id,plan_fingerprint,main_commit,tag,tag_commit,release_url,release_assets,image,image_digest,platforms,workflow_run_id,architecture_images}' "${receipt_file}") \
    <(jq -S . "${regenerated}") >/dev/null \
    || die "release ${tag} core receipt does not regenerate independently"

  local run_file="${node_dir}/run.json"
  gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}" > "${run_file}"
  jq -e \
    --arg path "${workflow_path}" \
    --arg login "${OWNER_LOGIN}" \
    --argjson owner_id "${OWNER_ID}" \
    --arg repo "${GITHUB_REPOSITORY}" \
    --argjson repo_id "${REPOSITORY_ID}" '
      .path == $path and .head_branch == "main" and .status == "completed" and
      .conclusion == "success" and .actor.login == $login and .actor.id == $owner_id and
      .repository.full_name == $repo and .repository.id == $repo_id and
      (.run_attempt | type) == "number" and .run_attempt >= 1 and
      (.head_sha | type) == "string" and (.head_sha | test("^[0-9a-f]{40}$"))
    ' "${run_file}" >/dev/null \
    || die "workflow run for ${tag} has an unexpected identity"
  run_attempt=$(jq -r '.run_attempt' "${run_file}")
  run_head=$(jq -r '.head_sha' "${run_file}")
  if [ "${kind}" = hotfix ]; then
    if [ "$(jq -r '.event' "${run_file}")" != workflow_dispatch ] || [ "${run_head}" != "${commit}" ]; then
      die "hotfix workflow for ${tag} is not pinned to its commit"
    fi
    jq -e \
      --arg path "${workflow_path}" \
      --arg ref "${GITHUB_REPOSITORY}/${workflow_path}@refs/heads/main" \
      --arg commit "${commit}" \
      --arg run_id "${run_id}" \
      --arg attempt "${run_attempt}" '
        .release_workflow == {
          path: $path, ref: $ref, commit: $commit,
          run_id: $run_id, run_attempt: $attempt
        }
      ' "${receipt_file}" >/dev/null \
      || die "hotfix workflow receipt for ${tag} differs"
  else
    case "$(jq -r '.event' "${run_file}")" in
      schedule|workflow_dispatch) ;;
      *) die "upstream workflow for ${tag} has an unexpected event" ;;
    esac
    local run_compare
    run_compare=$(gh api "repos/${GITHUB_REPOSITORY}/compare/${run_head}...${commit}" --jq .status)
    case "${run_compare}" in
      identical|ahead) ;;
      *) die "upstream workflow head for ${tag} is not an ancestor of its release" ;;
    esac
  fi

  local artifact_prefix=${kind}
  [ "${kind}" = upstream ] && artifact_prefix=upstream-sync
  [ "${kind}" = hotfix ] && artifact_prefix=hotfix-release
  local artifact_name="${artifact_prefix}-receipt-${run_id}-${run_attempt}"
  local artifacts_file="${node_dir}/artifacts.json"
  gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}/artifacts?per_page=100" > "${artifacts_file}"
  jq -e '.total_count == (.artifacts | length)' "${artifacts_file}" >/dev/null \
    || die "workflow artifact listing for ${tag} is incomplete"
  [ "$(jq --arg name "${artifact_name}" '[.artifacts[] | select(.name == $name)] | length' "${artifacts_file}")" -eq 1 ] \
    || die "workflow receipt artifact for ${tag} is missing or duplicated"
  local artifact_id artifact_digest artifact_size artifact_zip="${node_dir}/artifact.zip"
  artifact_id=$(jq -r --arg name "${artifact_name}" '.artifacts[] | select(.name == $name) | .id' "${artifacts_file}")
  artifact_digest=$(jq -r --arg name "${artifact_name}" '.artifacts[] | select(.name == $name) | .digest' "${artifacts_file}")
  artifact_size=$(jq -r --arg name "${artifact_name}" '.artifacts[] | select(.name == $name) | .size_in_bytes' "${artifacts_file}")
  jq -e \
    --arg name "${artifact_name}" \
    --argjson run_id "${run_id}" \
    --argjson repo_id "${REPOSITORY_ID}" \
    --arg head "${run_head}" '
      [.artifacts[] | select(.name == $name)] | .[0] |
      .expired == false and (.id | type) == "number" and .id > 0 and
      (.size_in_bytes | type) == "number" and .size_in_bytes > 0 and
      (.digest | type) == "string" and (.digest | test("^sha256:[0-9a-f]{64}$")) and
      .workflow_run.id == $run_id and .workflow_run.repository_id == $repo_id and
      .workflow_run.head_repository_id == $repo_id and .workflow_run.head_sha == $head
    ' "${artifacts_file}" >/dev/null \
    || die "workflow receipt artifact for ${tag} has an unexpected identity"
  gh api -H 'Accept: application/vnd.github+json' \
    "repos/${GITHUB_REPOSITORY}/actions/artifacts/${artifact_id}/zip" > "${artifact_zip}"
  if [ "$(stat -c %s "${artifact_zip}")" -ne "${artifact_size}" ] || \
     [ "sha256:$(sha256sum "${artifact_zip}" | awk '{ print $1 }')" != "${artifact_digest}" ]; then
    die "workflow receipt artifact bytes for ${tag} differ"
  fi
  local artifact_files="${node_dir}/artifact-files"
  unzip -Z1 "${artifact_zip}" | sed 's#^.*/##' | sort > "${artifact_files}"
  if [ "${kind}" = upstream ]; then
    diff -u <(printf '%s\n' run-state.json upstream-sync-receipt.json | sort) "${artifact_files}" >/dev/null \
      || die "upstream receipt artifact files for ${tag} differ"
  else
    diff -u <(printf '%s\n' final-plan.out hotfix-release-receipt.json independently-verified-receipt.json | sort) "${artifact_files}" >/dev/null \
      || die "hotfix receipt artifact files for ${tag} differ"
    cmp -s <(unzip -p "${artifact_zip}" hotfix-release-receipt.json) "${receipt_file}" \
      || die "hotfix artifact receipt bytes for ${tag} differ"
    cmp -s <(unzip -p "${artifact_zip}" independently-verified-receipt.json) "${receipt_file}" \
      || die "independent artifact receipt bytes for ${tag} differ"

    local final_member final_plan="${node_dir}/final-plan.out"
    final_member=$(unzip -Z1 "${artifact_zip}" | awk -F/ '$NF == "final-plan.out" { print }')
    [ "$(wc -l <<< "${final_member}" | tr -d ' ')" -eq 1 ] \
      || die "historical final plan for ${tag} is missing or duplicated"
    unzip -p "${artifact_zip}" "${final_member}" > "${final_plan}"
    if [ ! -s "${final_plan}" ] || \
       [ "$(tail -c 1 "${final_plan}" | od -An -t x1 | tr -d ' \n')" != 0a ]; then
      die "historical final plan for ${tag} is malformed"
    fi
    local plan_keys="${node_dir}/plan-keys" expected_plan_keys="${node_dir}/expected-plan-keys"
    if ! awk -F= '
      !/^[a-z][a-z0-9_]*=/ { exit 1 }
      seen[$1]++ { exit 1 }
      { print $1 }
    ' "${final_plan}" | sort > "${plan_keys}"; then
      die "historical final plan for ${tag} is malformed"
    fi
    printf '%s\n' \
      original_tag plus_tag pre_sync_head base_fork_commit original_repository \
      plus_repository models_repository original_head plus_tag_head plus_head \
      models_commit plus_head_included plus_head_already_represented \
      plus_head_delta_paths unsafe_plus_head_delta_paths blocked block_reason \
      fork_tag_prefix latest_fork_tag latest_fork_models_commit latest_fork_suffix \
      next_fork_tag expected_fork_tag safe_sync_id plan_fingerprint candidate_branch \
      snapshot_namespace original_snapshot_ref plus_tag_snapshot_ref \
      plus_head_snapshot_ref models_snapshot_ref target_drift target_drift_summary \
      has_changes | sort > "${expected_plan_keys}"
    cmp -s "${plan_keys}" "${expected_plan_keys}" \
      || die "historical final plan fields for ${tag} differ"
    plan_value() {
      local key=$1
      awk -F= -v key="${key}" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "${final_plan}"
    }
    local plan_sha plan_boolean
    for plan_sha in pre_sync_head base_fork_commit original_head plus_tag_head plus_head models_commit plan_fingerprint; do
      [[ "$(plan_value "${plan_sha}")" =~ ^[0-9a-f]{40}$ ]] \
        || die "historical final plan hash ${plan_sha} for ${tag} is malformed"
    done
    for plan_boolean in plus_head_included plus_head_already_represented blocked target_drift has_changes; do
      case "$(plan_value "${plan_boolean}")" in
        true|false) ;;
        *) die "historical final plan boolean ${plan_boolean} for ${tag} is malformed" ;;
      esac
    done
    if [ "$(plan_value original_tag)" != "$(state_value ORIGINAL_TAG)" ] || \
       [ "$(plan_value plus_tag)" != "$(state_value PLUS_TAG)" ] || \
       [ "$(plan_value pre_sync_head)" != "${commit}" ] || \
       [ "$(plan_value base_fork_commit)" != "${commit}" ] || \
       [ "$(plan_value original_repository)" != "$(state_value ORIGINAL_REPOSITORY)" ] || \
       [ "$(plan_value plus_repository)" != "$(state_value PLUS_REPOSITORY)" ] || \
       [ "$(plan_value models_repository)" != "$(state_value MODELS_REPOSITORY)" ] || \
       [ "$(plan_value original_head)" != "$(state_value ORIGINAL_COMMIT)" ] || \
       [ "$(plan_value plus_tag_head)" != "$(state_value PLUS_TAG_COMMIT)" ] || \
       [ "$(plan_value plus_head)" != "$(state_value PLUS_HEAD_COMMIT)" ] || \
       [ "$(plan_value models_commit)" != "$(state_value MODELS_COMMIT)" ] || \
       [ "$(plan_value plus_head_included)" != "$(state_value PLUS_HEAD_INCLUDED)" ] || \
       [ "$(plan_value latest_fork_tag)" != "${tag}" ] || \
       [ "$(plan_value expected_fork_tag)" != "${tag}" ] || \
       [ "$(plan_value latest_fork_models_commit)" != "$(state_value MODELS_COMMIT)" ] || \
       [ "$(plan_value safe_sync_id)" != "${EXPECTED_SYNC_ID}" ] || \
       [ "$(plan_value has_changes)" != false ] || \
       [ "$(plan_value target_drift)" != false ] || \
       [ "$(plan_value blocked)" != false ]; then
      die "historical final plan identity for ${tag} differs"
    fi
  fi
  local receipt_member
  receipt_member=$(unzip -Z1 "${artifact_zip}" | awk -F/ -v name="${receipt_name}" '$NF == name { print }')
  [ "$(wc -l <<< "${receipt_member}" | tr -d ' ')" -eq 1 ] \
    || die "artifact receipt path for ${tag} is missing or duplicated"
  cmp -s <(unzip -p "${artifact_zip}" "${receipt_member}") "${receipt_file}" \
    || die "artifact receipt bytes for ${tag} differ"

  jq -n \
    --arg tag "${tag}" \
    --arg commit "${commit}" \
    --arg receipt_name "${receipt_name}" \
    --arg receipt_id "${receipt_id}" \
    --arg receipt_digest "${receipt_digest}" \
    --arg workflow_path "${workflow_path}" \
    --arg run_id "${run_id}" \
    --arg run_attempt "${run_attempt}" \
    --arg run_head "${run_head}" \
    --arg artifact_id "${artifact_id}" \
    --arg artifact_name "${artifact_name}" \
    --arg artifact_digest "${artifact_digest}" '{
      tag: $tag,
      commit: $commit,
      receipt: {
        name: $receipt_name,
        asset_id: $receipt_id,
        digest: $receipt_digest
      },
      workflow: {
        path: $workflow_path,
        run_id: $run_id,
        run_attempt: $run_attempt,
        head_sha: $run_head
      },
      artifact: {
        id: $artifact_id,
        name: $artifact_name,
        digest: $artifact_digest
      }
    }' > "${output}"
}

validate_chain() {
  local tag=$1
  local commit=$2
  local child_tag=$3
  local child_commit=$4
  local depth=$5
  local output=$6
  [ "${depth}" -le "${MAX_HOTFIX_DEPTH}" ] \
    || die "hotfix receipt chain exceeds ${MAX_HOTFIX_DEPTH} nodes"
  if grep -Fxq "${tag}" "${VISITED_TAGS}" || grep -Fxq "${commit}" "${VISITED_COMMITS}"; then
    die "hotfix receipt chain contains a cycle"
  fi
  echo "${tag}" >> "${VISITED_TAGS}"
  echo "${commit}" >> "${VISITED_COMMITS}"
  release_parts "${tag}" || die "chain contains invalid tag ${tag}"
  [ "${RELEASE_PREFIX}" = "${CANDIDATE_PREFIX}" ] \
    || die "chain tag ${tag} is on the wrong release line"
  local suffix=${RELEASE_SUFFIX}
  release_parts "${child_tag}" || die "chain contains invalid child tag ${child_tag}"
  [ "${RELEASE_SUFFIX}" -eq $((suffix + 1)) ] \
    || die "hotfix receipt chain has a suffix gap before ${child_tag}"
  local compare_status
  compare_status=$(gh api "repos/${GITHUB_REPOSITORY}/compare/${commit}...${child_commit}" --jq .status)
  [ "${compare_status}" = ahead ] \
    || die "${child_tag} is not strictly descended from ${tag}"
  local kind=hotfix
  [ "${suffix}" -eq 0 ] && kind=upstream
  verify_release_and_link "${tag}" "${commit}" "${kind}" "${output}"

  local receipt_name=hotfix-release-receipt.json
  [ "${kind}" = upstream ] && receipt_name=upstream-sync-receipt.json
  local receipt_file="${ROOT}/node-${tag}/${receipt_name}"
  local state_at_node="${ROOT}/node-${tag}/state.env"
  git show "${commit}:.ccs-fork-upstream.env" > "${state_at_node}" \
    || die "chain commit ${commit} has no upstream-sync state"
  cmp -s "${STATE_FILE}" "${state_at_node}" \
    || die "hotfix chain changed .ccs-fork-upstream.env at ${tag}"

  if [ "${kind}" = upstream ]; then
    [ "${tag}" = "${ROOT_TAG}" ] \
      || die "hotfix receipt chain terminated at unexpected root ${tag}"
    json_exact_keys "${receipt_file}" \
      'keys == (["architecture_images","image","image_digest","main_commit","plan_fingerprint","platforms","release_assets","release_url","schema_version","sync_id","tag","tag_commit","workflow_run_id"] | sort)' \
      "accepted upstream root receipt"
    cp "${output}" "${ROOT_LINK}"
    return
  fi

  jq -e \
    --arg sync_id "${EXPECTED_SYNC_ID}" \
    --arg fingerprint "${EXPECTED_PLAN_FINGERPRINT}" \
    --arg state_digest "$(sha256sum "${STATE_FILE}" | awk '{ print $1 }')" '
      .receipt_type == "hotfix-release" and
      (.upstream_state | keys) == ["plan_fingerprint", "sha256", "sync_id"] and
      .upstream_state.sync_id == $sync_id and
      .upstream_state.plan_fingerprint == $fingerprint and
      .upstream_state.sha256 == $state_digest
    ' "${receipt_file}" >/dev/null \
    || die "hotfix receipt ${tag} has invalid upstream-state provenance"
  local expected_asset_digests
  expected_asset_digests=$(jq -c '
    [.assets[] |
      select(.name != "upstream-sync-receipt.json" and .name != "hotfix-release-receipt.json") |
      {key: .name, value: .digest}] | from_entries
  ' "${ROOT}/node-${tag}/release.json")
  jq -e --argjson expected "${expected_asset_digests}" \
    '.release_asset_digests == $expected' "${receipt_file}" >/dev/null \
    || die "hotfix receipt ${tag} release-asset digests differ"

  local schema
  schema=$(jq -r '.hotfix_schema_version // empty' "${receipt_file}")
  case "${schema}" in
    1)
      [ "${suffix}" -eq 1 ] \
        || die "legacy hotfix receipt schema is valid only for suffix .1"
      json_exact_keys "${receipt_file}" \
        'keys == (["architecture_images","hotfix_schema_version","image","image_digest","main_commit","plan_fingerprint","platforms","previous_release","receipt_type","release_asset_digests","release_assets","release_url","release_workflow","schema_version","sync_id","tag","tag_commit","upstream_state","workflow_run_id"] | sort)' \
        "legacy hotfix receipt"
      local parent_tag parent_commit
      parent_tag=$(jq -r '.previous_release.tag // empty' "${receipt_file}")
      parent_commit=$(jq -r '.previous_release.commit // empty' "${receipt_file}")
      if [ "${parent_tag}" != "${ROOT_TAG}" ] || [[ ! "${parent_commit}" =~ ^[0-9a-f]{40}$ ]]; then
        die "legacy .1 receipt does not terminate at the accepted upstream root"
      fi
      local root_actual="${ROOT}/root-from-${tag}.json"
      validate_chain "${parent_tag}" "${parent_commit}" "${tag}" "${commit}" $((depth + 1)) "${root_actual}"
      ;;
    2)
      [ "${suffix}" -ge 2 ] \
        || die "chained hotfix receipt schema requires suffix .2 or later"
      json_exact_keys "${receipt_file}" \
        'keys == (["accepted_upstream_root","architecture_images","hotfix_schema_version","image","image_digest","main_commit","plan_fingerprint","platforms","previous_release","receipt_type","release_asset_digests","release_assets","release_url","release_workflow","schema_version","sync_id","tag","tag_commit","upstream_state","workflow_run_id"] | sort)' \
        "chained hotfix receipt"
      local parent_descriptor="${ROOT}/parent-recorded-${tag}.json"
      local root_descriptor="${ROOT}/root-recorded-${tag}.json"
      jq '.previous_release' "${receipt_file}" > "${parent_descriptor}"
      jq '.accepted_upstream_root' "${receipt_file}" > "${root_descriptor}"
      local previous_tag previous_commit
      previous_tag=$(jq -r '.tag // empty' "${parent_descriptor}")
      previous_commit=$(jq -r '.commit // empty' "${parent_descriptor}")
      [[ "${previous_commit}" =~ ^[0-9a-f]{40}$ ]] \
        || die "receipt ${tag} records an invalid parent commit"
      local parent_actual="${ROOT}/parent-actual-${tag}.json"
      validate_chain "${previous_tag}" "${previous_commit}" "${tag}" "${commit}" $((depth + 1)) "${parent_actual}"
      diff -u <(jq -S . "${parent_descriptor}") <(jq -S . "${parent_actual}") >/dev/null \
        || die "receipt ${tag} immediate parent identity differs"
      diff -u <(jq -S . "${root_descriptor}") <(jq -S . "${ROOT_LINK}") >/dev/null \
        || die "receipt ${tag} accepted upstream root identity differs"
      ;;
    *) die "hotfix receipt ${tag} has an unsupported schema" ;;
  esac
}

PARENT_LINK="${ROOT}/immediate-parent.json"
validate_chain \
  "${PARENT_TAG}" "${EXPECTED_PARENT_COMMIT}" \
  "${TAG}" "${EXPECTED_COMMIT}" 1 "${PARENT_LINK}"
[ -s "${ROOT_LINK}" ] || die "hotfix receipt chain did not terminate at an accepted upstream release"

mkdir -p "$(dirname -- "${OUTPUT}")"
OUTPUT_TEMP=$(mktemp "${OUTPUT}.tmp.XXXXXX")
jq -n \
  --slurpfile parent "${PARENT_LINK}" \
  --slurpfile root "${ROOT_LINK}" '{
    immediate_parent: $parent[0],
    accepted_upstream_root: $root[0]
  }' > "${OUTPUT_TEMP}"
mv "${OUTPUT_TEMP}" "${OUTPUT}"

echo "[OK] verified hotfix chain ${ROOT_TAG} -> ${PARENT_TAG}; output=${OUTPUT}"
