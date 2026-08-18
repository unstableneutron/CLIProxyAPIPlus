#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
SELECTOR="${SCRIPT_DIR}/select-docker-digest-evidence.sh"
TAG=v7.2.135-unstableneutron.2
COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
RUN_HEAD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
REPOSITORY_ID=12345
RUN_ID=67890
MATRIX='{"include":[{"tag_suffix":"amd64","runner":"ubuntu-24.04","platform":"linux/amd64"},{"tag_suffix":"arm64","runner":"ubuntu-24.04-arm","platform":"linux/arm64"}]}'

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

write_attempt() {
  local root=$1 attempt=$2 conclusion=$3
  jq -n \
    --arg commit "${RUN_HEAD}" \
    --arg conclusion "${conclusion}" \
    --argjson attempt "${attempt}" \
    --argjson repository_id "${REPOSITORY_ID}" \
    --argjson run_id "${RUN_ID}" '{
      id: $run_id,
      run_attempt: $attempt,
      head_sha: $commit,
      conclusion: $conclusion,
      repository: {id: $repository_id},
      head_repository: {id: $repository_id}
    }' > "${root}/attempt-${attempt}.json"
}

add_artifact() {
  local root=$1 attempt=$2 suffix=$3 platform=$4 id=$5
  local evidence_dir=${root}/evidence-${id}
  mkdir -p "${evidence_dir}"
  jq -S -n \
    --arg repository unstableneutron/CLIProxyAPIPlus \
    --arg tag "${TAG}" \
    --arg commit "${COMMIT}" \
    --arg run_id "${RUN_ID}" \
    --argjson run_attempt "${attempt}" \
    --arg tag_suffix "${suffix}" \
    --arg platform "${platform}" \
    --arg source_digest "sha256:$(printf '%064d' "${id}")" \
    --arg architecture_digest "sha256:$(printf '%064x' "$((id + 1000))")" '{
      schema_version: 1,
      repository: $repository,
      tag: $tag,
      commit: $commit,
      run_id: $run_id,
      run_attempt: $run_attempt,
      tag_suffix: $tag_suffix,
      platform: $platform,
      source_digest: $source_digest,
      architecture_digest: $architecture_digest
    }' > "${evidence_dir}/docker-digest-evidence.json"
  python3 - "${evidence_dir}/docker-digest-evidence.json" "${root}/artifact-${id}.zip" <<'PY'
import sys
import zipfile

with zipfile.ZipFile(sys.argv[2], "w", zipfile.ZIP_DEFLATED) as archive:
    archive.write(sys.argv[1], "docker-digest-evidence.json")
PY
  local size digest
  size=$(stat -c %s "${root}/artifact-${id}.zip")
  digest="sha256:$(sha256sum "${root}/artifact-${id}.zip" | awk '{ print $1 }')"
  jq \
    --arg name "docker-digests-${TAG}-${attempt}-${suffix}" \
    --arg digest "${digest}" \
    --arg commit "${RUN_HEAD}" \
    --argjson id "${id}" \
    --argjson size "${size}" \
    --argjson repository_id "${REPOSITORY_ID}" \
    --argjson run_id "${RUN_ID}" \
    '.artifacts += [{
      id: $id,
      name: $name,
      expired: false,
      size_in_bytes: $size,
      digest: $digest,
      archive_download_url: ("https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/actions/artifacts/" + ($id | tostring) + "/zip"),
      workflow_run: {
        id: $run_id,
        repository_id: $repository_id,
        head_repository_id: $repository_id,
        head_sha: $commit
      }
    }]' "${root}/artifacts.json" > "${root}/artifacts.tmp"
  mv "${root}/artifacts.tmp" "${root}/artifacts.json"
}

make_fixture() {
  local root=$1 attempt=${2:-2}
  mkdir -p "${root}/bin"
  printf '%s\n' '{"artifacts":[]}' > "${root}/artifacts.json"
  add_artifact "${root}" "${attempt}" amd64 linux/amd64 "$((attempt * 100 + 1))"
  add_artifact "${root}" "${attempt}" arm64 linux/arm64 "$((attempt * 100 + 2))"
  write_attempt "${root}" 1 failure
  write_attempt "${root}" 2 failure
  jq -s '.' "${root}/artifacts.json" > "${root}/artifact-pages.json"
  cat > "${root}/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = api ] || exit 2
shift
endpoint=""
for argument in "$@"; do
  case "${argument}" in /repos/*) endpoint=${argument} ;; esac
done
case "${endpoint}" in
  */actions/runs/*/artifacts\?*) jq -c '.[] .artifacts' "${STUB_ROOT}/artifact-pages.json" ;;
  */actions/runs/*/attempts/*)
    attempt=${endpoint##*/}
    cat "${STUB_ROOT}/attempt-${attempt}.json"
    ;;
  */actions/artifacts/*/zip)
    artifact=${endpoint%/zip}
    artifact=${artifact##*/}
    cat "${STUB_ROOT}/artifact-${artifact}.zip"
    ;;
  *) echo "unexpected endpoint: ${endpoint}" >&2; exit 2 ;;
esac
EOF
  chmod +x "${root}/bin/gh"
}

refresh_pages() {
  local root=$1
  jq -s '.' "${root}/artifacts.json" > "${root}/artifact-pages.json"
}

run_selector() {
  local root=$1 current_attempt=$2 matrix=${3:-${MATRIX}}
  PATH="${root}/bin:${PATH}" \
    STUB_ROOT="${root}" \
    GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
    GITHUB_REPOSITORY_ID="${REPOSITORY_ID}" \
    GITHUB_RUN_ID="${RUN_ID}" \
    GITHUB_RUN_ATTEMPT="${current_attempt}" \
    GITHUB_OUTPUT="${root}/output.env" \
    "${SELECTOR}" "${TAG}" "${COMMIT}" "${RUN_HEAD}" "${matrix}" "${root}/output"
}

expect_failure() {
  local root=$1 current_attempt=$2 expected=$3 matrix=${4:-${MATRIX}}
  if run_selector "${root}" "${current_attempt}" "${matrix}" > "${root}/stdout" 2> "${root}/stderr"; then
    fail "selector unexpectedly accepted: ${expected}"
  fi
  grep -Fq "${expected}" "${root}/stderr" \
    || { cat "${root}/stderr" >&2; fail "missing rejection: ${expected}"; }
}

test_accepts_current_and_recoverable_prior_attempts() {
  local root conclusion
  root=$(mktemp -d)
  make_fixture "${root}" 2
  run_selector "${root}" 2 >/dev/null
  grep -Fxq evidence_attempt=2 "${root}/output.env" \
    || fail "current attempt was not selected"
  [ "$(find "${root}/output" -type f | wc -l)" -eq 2 ] \
    || fail "current evidence output is incomplete"
  rm -rf "${root}"

  root=$(mktemp -d)
  make_fixture "${root}" 2
  add_artifact "${root}" 1 amd64 linux/amd64 101
  add_artifact "${root}" 1 arm64 linux/arm64 102
  write_attempt "${root}" 1 success
  refresh_pages "${root}"
  run_selector "${root}" 2 >/dev/null
  grep -Fxq evidence_attempt=2 "${root}/output.env" \
    || fail "current attempt was not preferred over a successful prior attempt"
  rm -rf "${root}"

  for conclusion in failure cancelled timed_out; do
    root=$(mktemp -d)
    make_fixture "${root}" 1
    write_attempt "${root}" 1 "${conclusion}"
    run_selector "${root}" 2 >/dev/null
    grep -Fxq evidence_attempt=1 "${root}/output.env" \
      || fail "recoverable ${conclusion} attempt was not selected"
    rm -rf "${root}"
  done
}

test_rejects_successful_or_mismatched_prior_attempt() {
  local root
  root=$(mktemp -d)
  make_fixture "${root}" 1
  write_attempt "${root}" 1 success
  expect_failure "${root}" 2 "prior workflow attempt 1 is not recoverable"
  rm -rf "${root}"

  root=$(mktemp -d)
  make_fixture "${root}" 1
  jq '.head_sha = "cccccccccccccccccccccccccccccccccccccccc"' \
    "${root}/attempt-1.json" > "${root}/attempt.tmp"
  mv "${root}/attempt.tmp" "${root}/attempt-1.json"
  expect_failure "${root}" 2 "prior workflow attempt 1 is not recoverable"
  rm -rf "${root}"
}

test_rejects_artifact_identity_download_and_payload_drift() {
  local root id
  root=$(mktemp -d)
  make_fixture "${root}" 2
  jq '.artifacts += [.artifacts[0]]' "${root}/artifacts.json" > "${root}/artifacts.tmp"
  mv "${root}/artifacts.tmp" "${root}/artifacts.json"
  refresh_pages "${root}"
  expect_failure "${root}" 2 "duplicate artifact identity"
  rm -rf "${root}"

  root=$(mktemp -d)
  make_fixture "${root}" 2
  jq '.artifacts[0].workflow_run.head_sha = "cccccccccccccccccccccccccccccccccccccccc"' \
    "${root}/artifacts.json" > "${root}/artifacts.tmp"
  mv "${root}/artifacts.tmp" "${root}/artifacts.json"
  refresh_pages "${root}"
  expect_failure "${root}" 2 "artifact identity"
  rm -rf "${root}"

  root=$(mktemp -d)
  make_fixture "${root}" 2
  jq '.artifacts[0].digest = ("sha256:" + ("f" * 64))' \
    "${root}/artifacts.json" > "${root}/artifacts.tmp"
  mv "${root}/artifacts.tmp" "${root}/artifacts.json"
  refresh_pages "${root}"
  expect_failure "${root}" 2 "download digest differs"
  rm -rf "${root}"

  root=$(mktemp -d)
  make_fixture "${root}" 2
  jq '.artifacts[0].size_in_bytes += 1' \
    "${root}/artifacts.json" > "${root}/artifacts.tmp"
  mv "${root}/artifacts.tmp" "${root}/artifacts.json"
  refresh_pages "${root}"
  expect_failure "${root}" 2 "download size differs"
  rm -rf "${root}"

  root=$(mktemp -d)
  make_fixture "${root}" 2
  id=201
  jq '.commit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
    "${root}/evidence-${id}/docker-digest-evidence.json" > "${root}/evidence.tmp"
  mv "${root}/evidence.tmp" "${root}/evidence-${id}/docker-digest-evidence.json"
  rm "${root}/artifact-${id}.zip"
  python3 - "${root}/evidence-${id}/docker-digest-evidence.json" "${root}/artifact-${id}.zip" <<'PY'
import sys
import zipfile
with zipfile.ZipFile(sys.argv[2], "w", zipfile.ZIP_DEFLATED) as archive:
    archive.write(sys.argv[1], "docker-digest-evidence.json")
PY
  size=$(stat -c %s "${root}/artifact-${id}.zip")
  digest="sha256:$(sha256sum "${root}/artifact-${id}.zip" | awk '{ print $1 }')"
  jq --argjson size "${size}" --arg digest "${digest}" \
    '.artifacts[0].size_in_bytes = $size | .artifacts[0].digest = $digest' \
    "${root}/artifacts.json" > "${root}/artifacts.tmp"
  mv "${root}/artifacts.tmp" "${root}/artifacts.json"
  refresh_pages "${root}"
  expect_failure "${root}" 2 "evidence payload differs"
  rm -rf "${root}"
}

test_rejects_incomplete_attempt_and_invalid_matrix() {
  local root
  root=$(mktemp -d)
  make_fixture "${root}" 2
  jq 'del(.artifacts[1])' "${root}/artifacts.json" > "${root}/artifacts.tmp"
  mv "${root}/artifacts.tmp" "${root}/artifacts.json"
  refresh_pages "${root}"
  expect_failure "${root}" 2 "no complete current or recoverable prior"
  rm -rf "${root}"

  root=$(mktemp -d)
  make_fixture "${root}" 2
  expect_failure "${root}" 101 "workflow attempt exceeds the recovery bound"
  rm -rf "${root}"

  root=$(mktemp -d)
  make_fixture "${root}" 2
  expect_failure "${root}" 2 "target matrix differs" \
    '{"include":[{"tag_suffix":"amd64","runner":"ubuntu","platform":"linux/amd64"},{"tag_suffix":"amd64","runner":"ubuntu","platform":"linux/amd64"}]}'
  rm -rf "${root}"
}

main() {
  test_accepts_current_and_recoverable_prior_attempts
  test_rejects_successful_or_mismatched_prior_attempt
  test_rejects_artifact_identity_download_and_payload_drift
  test_rejects_incomplete_attempt_and_invalid_matrix
  echo "[OK] Docker digest evidence selector tests passed"
}

main "$@"
