#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
PUBLISHER="${SCRIPT_DIR}/publish-staged-release.sh"
STAGER="${SCRIPT_DIR}/stage-release-assets.sh"
# shellcheck source=/dev/null
source "${SCRIPT_DIR}/release-assets.sh"

TAG=v7.2.135-unstableneutron.2
COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
RUN_ID=123456789
RUN_HEAD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
ARTIFACT_ID=700
RECEIPT=hotfix-release-receipt.json

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

make_stub() {
  local root=$1
  mkdir -p "${root}/bin"
  cat > "${root}/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

asset_json() {
  local id=$1 name=$2 file=$3 digest size
  digest="sha256:$(sha256sum "${file}" | awk '{ print $1 }')"
  size=$(stat -c %s "${file}")
  jq -n --argjson id "${id}" --arg name "${name}" --arg digest "${digest}" --argjson size "${size}" '{
    id: $id, name: $name, size: $size, digest: $digest, state: "uploaded",
    url: ("https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/assets/" + ($id | tostring)),
    uploader: {login: "github-actions[bot]", id: 41898282, type: "Bot"}
  }'
}

write_release() {
  local input=$1
  mv "${input}" "${STUB_RELEASE_FILE}"
}

if [ "${1:-}" = release ] && [ "${2:-}" = upload ]; then
  file=$4
  name=${file##*/}
  printf 'upload:%s\n' "${name}" >> "${STUB_CALLS}"
  if [ "${STUB_FAIL_UPLOAD_BEFORE_ACCEPT:-}" = "${name}" ]; then exit 1; fi
  id=$(cat "${STUB_NEXT_ASSET_ID}")
  echo $((id + 1)) > "${STUB_NEXT_ASSET_ID}"
  asset=$(asset_json "${id}" "${name}" "${file}")
  jq --argjson asset "${asset}" '.assets += [$asset]' "${STUB_RELEASE_FILE}" > "${STUB_RELEASE_FILE}.new"
  write_release "${STUB_RELEASE_FILE}.new"
  if [ "${STUB_FAIL_UPLOAD_AFTER_ACCEPT:-}" = "${name}" ]; then exit 1; fi
  exit 0
fi

[ "${1:-}" = api ] || { echo "unexpected gh arguments: $*" >&2; exit 2; }
shift
include=false
method=GET
input=""
jq_filter=""
form_draft=""
path=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --include) include=true; shift ;;
    --method) method=$2; shift 2 ;;
    --input) input=$2; shift 2 ;;
    --jq) jq_filter=$2; shift 2 ;;
    -H) shift 2 ;;
    -f)
      [ "$2" = draft=false ] && form_draft=false
      shift 2
      ;;
    /*) path=$1; shift ;;
    *) shift ;;
  esac
done
[ -n "${path}" ] || { echo "missing gh api path" >&2; exit 2; }

case "${method}:${path}" in
  GET:/repos/*/releases/tags/*)
    if [ ! -f "${STUB_RELEASE_FILE}" ]; then
      [ "${include}" = true ] && printf 'HTTP/2 404 Not Found\n\n'
      exit 1
    fi
    [ "${include}" = true ] && printf 'HTTP/2 200 OK\n\n'
    cat "${STUB_RELEASE_FILE}"
    ;;
  GET:/repos/*/releases/900)
    cat "${STUB_RELEASE_FILE}"
    ;;
  GET:/repos/*/actions/artifacts/${STUB_ARTIFACT_ID})
    cat "${STUB_ARTIFACT_JSON}"
    ;;
  GET:/repos/*/actions/artifacts/${STUB_ARTIFACT_ID}/zip)
    cat "${STUB_ARTIFACT_ZIP}"
    ;;
  GET:/repos/*/commits/*)
    count=$(cat "${STUB_COMMIT_COUNT}")
    count=$((count + 1))
    echo "${count}" > "${STUB_COMMIT_COUNT}"
    value=${STUB_EXPECTED_COMMIT}
    if [ -n "${STUB_MOVE_AFTER_COMMIT_CHECKS:-}" ] && [ "${count}" -gt "${STUB_MOVE_AFTER_COMMIT_CHECKS}" ]; then
      value=ffffffffffffffffffffffffffffffffffffffff
    fi
    printf '{"sha":"%s"}\n' "${value}"
    ;;
  POST:/repos/*/releases)
    printf 'create\n' >> "${STUB_CALLS}"
    [ "${STUB_FAIL_CREATE_BEFORE_ACCEPT:-false}" = true ] && exit 1
    jq '{
      id: 900,
      tag_name: .tag_name,
      html_url: ("https://github.com/unstableneutron/CLIProxyAPIPlus/releases/tag/" + .tag_name),
      assets_url: "https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/900/assets",
      draft: true,
      prerelease: false,
      target_commitish: .target_commitish,
      body: .body,
      author: {login: "github-actions[bot]", id: 41898282, type: "Bot"},
      assets: []
    }' "${input}" > "${STUB_RELEASE_FILE}.new"
    write_release "${STUB_RELEASE_FILE}.new"
    [ "${STUB_FAIL_CREATE_AFTER_ACCEPT:-false}" = true ] && exit 1
    cat "${STUB_RELEASE_FILE}"
    ;;
  PATCH:/repos/*/releases/900)
    printf 'publish\n' >> "${STUB_CALLS}"
    [ "${form_draft}" = false ] || exit 2
    [ "${STUB_FAIL_PUBLISH_BEFORE_ACCEPT:-false}" = true ] && exit 1
    jq '.draft = false' "${STUB_RELEASE_FILE}" > "${STUB_RELEASE_FILE}.new"
    write_release "${STUB_RELEASE_FILE}.new"
    [ "${STUB_FAIL_PUBLISH_AFTER_ACCEPT:-false}" = true ] && exit 1
    cat "${STUB_RELEASE_FILE}"
    ;;
  *) echo "unexpected gh api request: ${method} ${path}" >&2; exit 2 ;;
esac | if [ -n "${jq_filter}" ]; then jq -r "${jq_filter}"; else cat; fi
EOF
  chmod +x "${root}/bin/gh"
}

write_artifact_zip() {
  local root=$1 unsafe_name=${2:-}
  python3 - "${root}" "${root}/artifact.zip" "${unsafe_name}" <<'PY'
import pathlib
import sys
import zipfile

root = pathlib.Path(sys.argv[1])
unsafe_name = sys.argv[3]
used_unsafe_name = False
with zipfile.ZipFile(sys.argv[2], "w", zipfile.ZIP_STORED) as archive:
    archive.write(root / "release-manifest.json", "release-manifest.json")
    for path in sorted((root / "dist").iterdir()):
        member_name = path.name
        if unsafe_name and not used_unsafe_name and path.name.endswith(".zip"):
            member_name = unsafe_name
            used_unsafe_name = True
        archive.write(path, member_name)
PY
}

refresh_artifact_identity() {
  local root=$1 artifact_digest artifact_size
  artifact_digest="sha256:$(sha256sum "${root}/artifact.zip" | awk '{ print $1 }')"
  artifact_size=$(stat -c %s "${root}/artifact.zip")
  jq --arg digest "${artifact_digest}" --argjson size "${artifact_size}" \
    '.digest = $digest | .size_in_bytes = $size' \
    "${root}/artifact.json" > "${root}/artifact.json.new"
  mv "${root}/artifact.json.new" "${root}/artifact.json"
}

setup_fixture() {
  local root=$1
  local expected name digest artifact_digest artifact_size
  mkdir -p "${root}/dist"
  expected=$(expected_release_assets_json "${TAG}") || fail "asset contract failed"
  : > "${root}/dist/checksums.txt"
  while IFS= read -r name; do
    [ "${name}" != checksums.txt ] || continue
    printf 'staged bytes for %s\n' "${name}" > "${root}/dist/${name}"
    digest=$(sha256sum "${root}/dist/${name}" | awk '{ print $1 }')
    printf '%s  %s\n' "${digest}" "${name}" >> "${root}/dist/checksums.txt"
  done < <(jq -r '.[]' <<< "${expected}")
  GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
  GITHUB_RUN_ID="${RUN_ID}" GITHUB_RUN_ATTEMPT=1 \
    "${STAGER}" "${TAG}" "${COMMIT}" "${RECEIPT}" \
      "${root}/dist" "${root}/release-manifest.json"
  write_artifact_zip "${root}"
  artifact_digest="sha256:$(sha256sum "${root}/artifact.zip" | awk '{ print $1 }')"
  artifact_size=$(stat -c %s "${root}/artifact.zip")
  jq -n \
    --argjson id "${ARTIFACT_ID}" \
    --arg name "staged-release-assets-${RUN_ID}-1" \
    --arg digest "${artifact_digest}" \
    --argjson size "${artifact_size}" \
    --argjson run_id "${RUN_ID}" \
    --arg head "${RUN_HEAD}" '{
      id: $id, name: $name, digest: $digest, size_in_bytes: $size, expired: false,
      archive_download_url: ("https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/actions/artifacts/" + ($id | tostring) + "/zip"),
      workflow_run: {id: $run_id, repository_id: 1247056725, head_repository_id: 1247056725, head_sha: $head}
    }' > "${root}/artifact.json"
  jq -Scn \
    --arg artifact_id "${ARTIFACT_ID}" \
    --arg artifact_name "staged-release-assets-${RUN_ID}-1" \
    --arg artifact_digest "${artifact_digest}" \
    --arg workflow_run_id "${RUN_ID}" \
    --arg workflow_run_attempt 1 '{
      artifact_id: $artifact_id,
      artifact_name: $artifact_name,
      artifact_digest: $artifact_digest,
      workflow_run_id: $workflow_run_id,
      workflow_run_attempt: $workflow_run_attempt
    }' > "${root}/evidence.json"
  : > "${root}/calls"
  echo 1000 > "${root}/next-asset-id"
  echo 0 > "${root}/commit-count"
  make_stub "${root}"
}

evidence_body() {
  local root=$1
  printf '<!-- cliproxy-staged-release:v1 %s -->' "$(cat "${root}/evidence.json")"
}

write_release() {
  local root=$1 draft=$2 count=$3
  local assets='[]' id=1000 name asset body
  body=$(evidence_body "${root}")
  while IFS= read -r name; do
    [ "${count}" -gt 0 ] || break
    asset=$(jq -n \
      --argjson id "${id}" \
      --arg name "${name}" \
      --argjson size "$(stat -c %s "${root}/dist/${name}")" \
      --arg digest "sha256:$(sha256sum "${root}/dist/${name}" | awk '{ print $1 }')" '{
        id: $id, name: $name, size: $size, digest: $digest, state: "uploaded",
        url: ("https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/assets/" + ($id | tostring)),
        uploader: {login: "github-actions[bot]", id: 41898282, type: "Bot"}
      }')
    assets=$(jq -c --argjson asset "${asset}" '. + [$asset]' <<< "${assets}")
    id=$((id + 1))
    count=$((count - 1))
  done < <(expected_release_assets_json "${TAG}" | jq -r '.[]')
  echo "${id}" > "${root}/next-asset-id"
  jq -n \
    --arg tag "${TAG}" \
    --arg body "${body}" \
    --argjson draft "${draft}" \
    --argjson assets "${assets}" '{
      id: 900, tag_name: $tag,
      html_url: ("https://github.com/unstableneutron/CLIProxyAPIPlus/releases/tag/" + $tag),
      assets_url: "https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/900/assets",
      draft: $draft, prerelease: false, target_commitish: "main", body: $body,
      author: {login: "github-actions[bot]", id: 41898282, type: "Bot"}, assets: $assets
    }' > "${root}/release.json"
}

run_publisher() {
  local root=$1
  shift
  local digest
  digest=$(jq -r '.digest' "${root}/artifact.json")
  PATH="${root}/bin:${PATH}" \
  GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
  STUB_RELEASE_FILE="${root}/release.json" \
  STUB_ARTIFACT_ID="${ARTIFACT_ID}" \
  STUB_ARTIFACT_JSON="${root}/artifact.json" \
  STUB_ARTIFACT_ZIP="${root}/artifact.zip" \
  STUB_EXPECTED_COMMIT="${COMMIT}" \
  STUB_CALLS="${root}/calls" \
  STUB_NEXT_ASSET_ID="${root}/next-asset-id" \
  STUB_COMMIT_COUNT="${root}/commit-count" \
  env "$@" \
    "${PUBLISHER}" "${TAG}" "${COMMIT}" "${RECEIPT}" \
      "${ARTIFACT_ID}" "staged-release-assets-${RUN_ID}-1" "${digest}" \
      "${RUN_ID}" "${RUN_HEAD}"
}

expect_failure() {
  local root=$1 expected=$2
  shift 2
  if run_publisher "${root}" "$@" > "${root}/failure.log" 2>&1; then
    fail "publisher unexpectedly succeeded"
  fi
  grep -Fq "${expected}" "${root}/failure.log" \
    || { cat "${root}/failure.log" >&2; fail "missing failure: ${expected}"; }
}

test_fresh_and_every_partial_draft_boundary() {
  local root count expected_count
  root=$(mktemp -d)
  setup_fixture "${root}"
  run_publisher "${root}" >/dev/null
  [ "$(jq -r '.draft' "${root}/release.json")" = false ] || fail "fresh release remained draft"
  expected_count=$(expected_release_assets_json "${TAG}" | jq length)
  [ "$(jq '.assets | length' "${root}/release.json")" -eq "${expected_count}" ] \
    || fail "fresh release matrix is incomplete"
  rm -rf "${root}"

  for count in $(seq 0 9); do
    root=$(mktemp -d)
    setup_fixture "${root}"
    write_release "${root}" true "${count}"
    run_publisher "${root}" >/dev/null
    [ "$(jq -r '.draft' "${root}/release.json")" = false ] \
      || fail "partial boundary ${count} remained draft"
    [ "$(jq '.assets | length' "${root}/release.json")" -eq "${expected_count}" ] \
      || fail "partial boundary ${count} is incomplete"
    rm -rf "${root}"
  done
}

test_reconciles_uncertain_mutation_results_without_replay() {
  local root first expected_count
  root=$(mktemp -d)
  setup_fixture "${root}"
  run_publisher "${root}" STUB_FAIL_CREATE_AFTER_ACCEPT=true \
    STUB_FAIL_PUBLISH_AFTER_ACCEPT=true >/dev/null
  [ "$(grep -c '^create$' "${root}/calls")" -eq 1 ] || fail "draft create replayed"
  [ "$(grep -c '^publish$' "${root}/calls")" -eq 1 ] || fail "publish replayed"
  rm -rf "${root}"

  root=$(mktemp -d)
  setup_fixture "${root}"
  expect_failure "${root}" "draft creation outcome is unknown" \
    STUB_FAIL_CREATE_BEFORE_ACCEPT=true
  [ "$(grep -c '^create$' "${root}/calls")" -eq 1 ] || fail "uncertain draft create replayed"
  rm -rf "${root}"

  root=$(mktemp -d)
  setup_fixture "${root}"
  write_release "${root}" true 0
  first=$(expected_release_assets_json "${TAG}" | jq -r '.[0]')
  expect_failure "${root}" "upload outcome" STUB_FAIL_UPLOAD_BEFORE_ACCEPT="${first}"
  [ "$(grep -c "^upload:${first}$" "${root}/calls")" -eq 1 ] || fail "failed upload replayed"
  rm -rf "${root}"

  root=$(mktemp -d)
  setup_fixture "${root}"
  write_release "${root}" true 0
  first=$(expected_release_assets_json "${TAG}" | jq -r '.[0]')
  run_publisher "${root}" STUB_FAIL_UPLOAD_AFTER_ACCEPT="${first}" >/dev/null
  [ "$(grep -c "^upload:${first}$" "${root}/calls")" -eq 1 ] \
    || fail "accepted upload was replayed"
  rm -rf "${root}"

  root=$(mktemp -d)
  setup_fixture "${root}"
  expected_count=$(expected_release_assets_json "${TAG}" | jq length)
  write_release "${root}" true "${expected_count}"
  expect_failure "${root}" "release publication outcome is unknown" \
    STUB_FAIL_PUBLISH_BEFORE_ACCEPT=true
  [ "$(grep -c '^publish$' "${root}/calls")" -eq 1 ] \
    || fail "uncertain publish was replayed"
  rm -rf "${root}"
}

test_rejects_conflicting_or_unbound_release_state() {
  local root
  root=$(mktemp -d); setup_fixture "${root}"; write_release "${root}" false 8
  expect_failure "${root}" "stable release asset set differs"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"; write_release "${root}" true 1
  jq '.assets[0].digest = ("sha256:" + ("f" * 64))' "${root}/release.json" > "${root}/release.json.new"
  mv "${root}/release.json.new" "${root}/release.json"
  expect_failure "${root}" "differs from immutable staged bytes"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"; write_release "${root}" true 0
  jq '.body = "unbound"' "${root}/release.json" > "${root}/release.json.new"
  mv "${root}/release.json.new" "${root}/release.json"
  expect_failure "${root}" "staged evidence differs"
  rm -rf "${root}"
}

test_reuses_pinned_evidence_and_rejects_drift() {
  local root digest
  root=$(mktemp -d)
  setup_fixture "${root}"
  write_release "${root}" true 3
  digest=$(jq -r '.digest' "${root}/artifact.json")
  PATH="${root}/bin:${PATH}" \
  GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
  STUB_RELEASE_FILE="${root}/release.json" STUB_ARTIFACT_ID="${ARTIFACT_ID}" \
  STUB_ARTIFACT_JSON="${root}/artifact.json" STUB_ARTIFACT_ZIP="${root}/artifact.zip" \
  STUB_EXPECTED_COMMIT="${COMMIT}" STUB_CALLS="${root}/calls" \
  STUB_NEXT_ASSET_ID="${root}/next-asset-id" STUB_COMMIT_COUNT="${root}/commit-count" \
    "${PUBLISHER}" "${TAG}" "${COMMIT}" "${RECEIPT}" \
      999 "staged-release-assets-${RUN_ID}-2" "sha256:$(printf 'f%.0s' {1..64})" \
      "${RUN_ID}" "${RUN_HEAD}" >/dev/null
  [ "$(jq -r '.draft' "${root}/release.json")" = false ] \
    || fail "pinned evidence did not resume"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  jq '.digest = ("sha256:" + ("f" * 64))' "${root}/artifact.json" > "${root}/artifact.json.new"
  mv "${root}/artifact.json.new" "${root}/artifact.json"
  expect_failure "${root}" "staged artifact archive digest differs"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  expect_failure "${root}" "tag or main moved" STUB_MOVE_AFTER_COMMIT_CHECKS=2
  rm -rf "${root}"
}

test_rejects_mismatched_manifest_assets_and_unsafe_archives() {
  local root archive_name
  root=$(mktemp -d)
  setup_fixture "${root}"
  jq '.tag = "v0-invalid"' "${root}/release-manifest.json" \
    > "${root}/release-manifest.json.new"
  mv "${root}/release-manifest.json.new" "${root}/release-manifest.json"
  write_artifact_zip "${root}"
  refresh_artifact_identity "${root}"
  expect_failure "${root}" "staged release manifest differs"
  rm -rf "${root}"

  root=$(mktemp -d)
  setup_fixture "${root}"
  archive_name=$(expected_release_assets_json "${TAG}" | jq -r 'map(select(endswith(".zip")))[0]')
  printf 'tampered staged bytes\n' >> "${root}/dist/${archive_name}"
  write_artifact_zip "${root}"
  refresh_artifact_identity "${root}"
  expect_failure "${root}" "staged asset ${archive_name} size differs"
  rm -rf "${root}"

  root=$(mktemp -d)
  setup_fixture "${root}"
  write_artifact_zip "${root}" "../unsafe.zip"
  refresh_artifact_identity "${root}"
  expect_failure "${root}" "archive contains an unsafe member path"
  rm -rf "${root}"
}

main() {
  [ -x "${PUBLISHER}" ] || fail "publisher is not executable"
  [ -x "${STAGER}" ] || fail "stager is not executable"
  test_fresh_and_every_partial_draft_boundary
  test_reconciles_uncertain_mutation_results_without_replay
  test_rejects_conflicting_or_unbound_release_state
  test_reuses_pinned_evidence_and_rejects_drift
  test_rejects_mismatched_manifest_assets_and_unsafe_archives
  echo "[OK] staged release publication tests passed"
}

main "$@"
