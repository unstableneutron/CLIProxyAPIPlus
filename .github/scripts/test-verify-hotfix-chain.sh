#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
VERIFIER="${SCRIPT_DIR}/verify-hotfix-chain.sh"
ROOT_TAG=v7.2.131-unstableneutron.0
FIRST_TAG=v7.2.131-unstableneutron.1
SECOND_TAG=v7.2.131-unstableneutron.2
ORIGINAL_SOURCE_TAG=v7.2.131
PLUS_SOURCE_TAG=v7.2.127-3
SYNC_ID=original-${ORIGINAL_SOURCE_TAG}_plus-${PLUS_SOURCE_TAG}
FINGERPRINT=eeef3819ca9dfb38b4528fc5dabc3324d538b19b
IMAGE=ghcr.io/unstableneutron/cli-proxy-api-plus

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

run_git() {
  git -c init.defaultBranch=main "$@"
}

sha256_digest() {
  printf 'sha256:%s\n' "$(sha256sum "$1" | awk '{ print $1 }')"
}

asset_json() {
  local id=$1 name=$2 file=$3
  jq -n \
    --argjson id "${id}" \
    --arg name "${name}" \
    --arg url "https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/assets/${id}" \
    --argjson size "$(stat -c %s "${file}")" \
    --arg digest "$(sha256_digest "${file}")" \
    '{
      id: $id,
      name: $name,
      url: $url,
      size: $size,
      state: "uploaded",
      digest: $digest,
      uploader: {login: "github-actions[bot]", id: 41898282, type: "Bot"}
    }'
}

write_release() {
  local root=$1 tag=$2 release_id=$3 archive_id=$4 checksum_id=$5 receipt_id=$6 receipt_name=$7
  local node="${root}/fixtures/${tag}" archive_name
  archive_name=$(readlink "${node}/archive")
  jq -n \
    --argjson id "${release_id}" \
    --arg tag "${tag}" \
    --arg url "https://github.com/unstableneutron/CLIProxyAPIPlus/releases/tag/${tag}" \
    --arg assets_url "https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/releases/${release_id}/assets" \
    --argjson archive "$(asset_json "${archive_id}" "${archive_name}" "${node}/${archive_name}")" \
    --argjson checksum "$(asset_json "${checksum_id}" checksums.txt "${node}/checksums.txt")" \
    --argjson receipt "$(asset_json "${receipt_id}" "${receipt_name}" "${node}/${receipt_name}")" \
    '{
      id: $id,
      tag_name: $tag,
      html_url: $url,
      assets_url: $assets_url,
      published_at: "2026-08-15T05:32:46Z",
      draft: false,
      prerelease: false,
      target_commitish: "main",
      author: {login: "github-actions[bot]", id: 41898282, type: "Bot"},
      assets: [$checksum, $archive, $receipt]
    }' > "${node}/release.json"
}

write_artifact() {
  local root=$1 tag=$2 kind=$3 run_id=$4 run_head=$5 receipt_name=$6 artifact_id=$7
  local node="${root}/fixtures/${tag}" artifact_prefix=hotfix-release
  local work
  work=$(mktemp -d)
  if [ "${kind}" = upstream ]; then
    artifact_prefix=upstream-sync
    cp "${node}/${receipt_name}" "${work}/${receipt_name}"
    cp "${node}/run-state.json" "${work}/run-state.json"
  else
    cp "${node}/${receipt_name}" "${work}/${receipt_name}"
    cp "${node}/${receipt_name}" "${work}/independently-verified-receipt.json"
    cp "${node}/final-plan.out" "${work}/final-plan.out"
  fi
  python3 - "${work}" "${node}/artifact.zip" <<'PY'
import pathlib
import sys
import zipfile

source = pathlib.Path(sys.argv[1])
with zipfile.ZipFile(sys.argv[2], "w", zipfile.ZIP_STORED) as archive:
    for path in sorted(source.iterdir()):
        archive.write(path, path.name)
PY
  rm -rf "${work}"
  jq -n \
    --argjson id "${artifact_id}" \
    --arg name "${artifact_prefix}-receipt-${run_id}-1" \
    --arg digest "$(sha256_digest "${node}/artifact.zip")" \
    --argjson size "$(stat -c %s "${node}/artifact.zip")" \
    --argjson run_id "${run_id}" \
    --arg head "${run_head}" \
    '{
      total_count: 1,
      artifacts: [{
        id: $id,
        name: $name,
        digest: $digest,
        size_in_bytes: $size,
        expired: false,
        archive_download_url: ("https://api.github.com/repos/unstableneutron/CLIProxyAPIPlus/actions/artifacts/" + ($id | tostring) + "/zip"),
        workflow_run: {
          id: $run_id,
          repository_id: 1247056725,
          head_repository_id: 1247056725,
          head_sha: $head
        }
      }]
    }' > "${node}/artifacts.json"
}

write_root_fixture() {
  local root=$1 commit=$2
  local node="${root}/fixtures/${ROOT_TAG}"
  local archive_name=CLIProxyAPIPlus_7.2.131-unstableneutron.0_linux_amd64_no-plugin.tar.gz final_fingerprint
  final_fingerprint=$(
    printf '%s\n' \
      "base_fork_commit=${commit}" \
      "original_tag=${ORIGINAL_SOURCE_TAG}" \
      'original_commit=2222222222222222222222222222222222222222' \
      "plus_tag=${PLUS_SOURCE_TAG}" \
      'plus_tag_commit=3333333333333333333333333333333333333333' \
      'plus_head_commit=3333333333333333333333333333333333333333' \
      'plus_head_included=false' \
      'models_commit=4444444444444444444444444444444444444444' \
      "expected_fork_tag=${ROOT_TAG}" \
      | git hash-object --stdin
  )
  mkdir -p "${node}"
  printf 'root archive\n' > "${node}/archive"
  printf '%s  %s\n' "$(sha256sum "${node}/archive" | awk '{ print $1 }')" "${archive_name}" \
    > "${node}/checksums.txt"
  mv "${node}/archive" "${node}/${archive_name}"
  ln -s "${archive_name}" "${node}/archive"
  jq -n \
    --arg sync_id "${SYNC_ID}" \
    --arg fingerprint "${FINGERPRINT}" \
    --arg commit "${commit}" \
    --arg tag "${ROOT_TAG}" \
    --arg archive "${archive_name}" \
    --arg image "${IMAGE}:${ROOT_TAG}" \
    --arg digest "sha256:$(printf 'a%.0s' {1..64})" '
      {
        schema_version: 2,
        sync_id: $sync_id,
        plan_fingerprint: $fingerprint,
        main_commit: $commit,
        tag: $tag,
        tag_commit: $commit,
        release_url: ("https://github.com/unstableneutron/CLIProxyAPIPlus/releases/tag/" + $tag),
        release_assets: [$archive, "checksums.txt"] | sort,
        image: $image,
        image_digest: $digest,
        platforms: ["linux/amd64", "linux/arm64"],
        workflow_run_id: "800",
        architecture_images: {
          "linux/amd64": {image: ($image + "-amd64"), digest: ("sha256:" + ("1" * 64))},
          "linux/arm64": {image: ($image + "-arm64"), digest: ("sha256:" + ("2" * 64))}
        }
      }
    ' > "${node}/upstream-sync-receipt.json"
  write_release "${root}" "${ROOT_TAG}" 100 1 2 3 upstream-sync-receipt.json
  jq -n \
    --arg commit "${commit}" \
    --arg sync_id "${SYNC_ID}" \
    --arg fingerprint "${FINGERPRINT}" \
    --arg final_fingerprint "${final_fingerprint}" \
    --arg candidate "upstream-sync/${SYNC_ID}-${FINGERPRINT:0:12}" \
    --arg original_tag "${ORIGINAL_SOURCE_TAG}" \
    --arg plus_tag "${PLUS_SOURCE_TAG}" \
    --slurpfile receipt "${node}/upstream-sync-receipt.json" '
      {
        schema_version: 1,
        state: "released",
        target: {
          base_fork_commit: ("1" * 40),
          original: {tag: $original_tag, commit: ("2" * 40)},
          plus: {tag: $plus_tag, tag_commit: ("3" * 40), head: ("3" * 40), head_included: false},
          models_commit: ("4" * 40),
          sync_id: $sync_id,
          plan_fingerprint: $fingerprint,
          expected_fork_tag: $receipt[0].tag,
          target_drift: true,
          blocked: false
        },
        candidate: {branch: $candidate, sha: $commit, acceptable: true, validation_status: "passed"},
        repair: {imported: false, pr: null, sha: null},
        final_plan: {status: "clean-noop", plan_fingerprint: $final_fingerprint, has_changes: false, target_drift: false, blocked: false},
        runtime_smoke: "not_run",
        vn3_deployed: false,
        promotion: {commit: $commit, tag: $receipt[0].tag},
        release: {
          url: $receipt[0].release_url,
          assets: $receipt[0].release_assets,
          image: $receipt[0].image,
          image_digest: $receipt[0].image_digest,
          platforms: $receipt[0].platforms,
          architecture_images: $receipt[0].architecture_images
        }
      }
    ' > "${node}/run-state.json"
  jq -n \
    --arg head "${commit}" '
      {
        path: ".github/workflows/upstream-sync-v2.yml",
        head_branch: "main",
        head_sha: $head,
        status: "completed",
        conclusion: "success",
        event: "workflow_dispatch",
        run_attempt: 1,
        actor: {login: "unstableneutron", id: 156744497},
        repository: {full_name: "unstableneutron/CLIProxyAPIPlus", id: 1247056725}
      }
    ' > "${node}/run.json"
  write_artifact "${root}" "${ROOT_TAG}" upstream 800 "${commit}" upstream-sync-receipt.json 10800
}

write_final_plan() {
  local root=$1 commit=$2
  local fingerprint namespace
  fingerprint=$(
    printf '%s\n' \
      "base_fork_commit=${commit}" \
      "original_tag=${ORIGINAL_SOURCE_TAG}" \
      'original_commit=2222222222222222222222222222222222222222' \
      "plus_tag=${PLUS_SOURCE_TAG}" \
      'plus_tag_commit=3333333333333333333333333333333333333333' \
      'plus_head_commit=3333333333333333333333333333333333333333' \
      'plus_head_included=false' \
      'models_commit=4444444444444444444444444444444444444444' \
      "expected_fork_tag=${FIRST_TAG}" \
      | git hash-object --stdin
  )
  namespace="refs/upstream-sync/${fingerprint}"
  cat > "${root}/fixtures/${FIRST_TAG}/final-plan.out" <<EOF
original_tag=${ORIGINAL_SOURCE_TAG}
plus_tag=${PLUS_SOURCE_TAG}
pre_sync_head=${commit}
base_fork_commit=${commit}
original_repository=router-for-me/CLIProxyAPI
plus_repository=kaitranntt/CLIProxyAPIPlus
models_repository=router-for-me/models
original_head=2222222222222222222222222222222222222222
plus_tag_head=3333333333333333333333333333333333333333
plus_head=3333333333333333333333333333333333333333
models_commit=4444444444444444444444444444444444444444
plus_head_included=false
plus_head_already_represented=true
plus_head_delta_paths=
unsafe_plus_head_delta_paths=
blocked=false
block_reason=
fork_tag_prefix=v7.2.131-unstableneutron
latest_fork_tag=${FIRST_TAG}
latest_fork_models_commit=4444444444444444444444444444444444444444
latest_fork_suffix=1
next_fork_tag=${SECOND_TAG}
expected_fork_tag=${FIRST_TAG}
safe_sync_id=${SYNC_ID}
plan_fingerprint=${fingerprint}
candidate_branch=upstream-sync/${SYNC_ID}-${fingerprint:0:12}
snapshot_namespace=${namespace}
original_snapshot_ref=${namespace}/original
plus_tag_snapshot_ref=${namespace}/plus-tag
plus_head_snapshot_ref=${namespace}/plus-head
models_snapshot_ref=${namespace}/models
target_drift=false
target_drift_summary=
has_changes=false
EOF
}

write_first_fixture() {
  local root=$1 commit=$2 root_commit=$3
  local node="${root}/fixtures/${FIRST_TAG}"
  local archive_name=CLIProxyAPIPlus_7.2.131-unstableneutron.1_linux_amd64_no-plugin.tar.gz
  mkdir -p "${node}"
  printf 'first archive\n' > "${node}/archive"
  printf '%s  %s\n' "$(sha256sum "${node}/archive" | awk '{ print $1 }')" "${archive_name}" \
    > "${node}/checksums.txt"
  mv "${node}/archive" "${node}/${archive_name}"
  ln -s "${archive_name}" "${node}/archive"
  write_final_plan "${root}" "${commit}"
  local state_digest archive_digest checksums_digest
  state_digest=$(sha256sum "${root}/repo/.ccs-fork-upstream.env" | awk '{ print $1 }')
  archive_digest=$(sha256_digest "${node}/archive")
  checksums_digest=$(sha256_digest "${node}/checksums.txt")
  jq -n \
    --arg sync_id "${SYNC_ID}" \
    --arg fingerprint "${FINGERPRINT}" \
    --arg commit "${commit}" \
    --arg root_commit "${root_commit}" \
    --arg tag "${FIRST_TAG}" \
    --arg root_tag "${ROOT_TAG}" \
    --arg archive "${archive_name}" \
    --arg archive_digest "${archive_digest}" \
    --arg checksums_digest "${checksums_digest}" \
    --arg state_digest "${state_digest}" \
    --arg image "${IMAGE}:${FIRST_TAG}" \
    --arg digest "sha256:$(printf 'a%.0s' {1..64})" '
      {
        schema_version: 2,
        sync_id: $sync_id,
        plan_fingerprint: $fingerprint,
        main_commit: $commit,
        tag: $tag,
        tag_commit: $commit,
        release_url: ("https://github.com/unstableneutron/CLIProxyAPIPlus/releases/tag/" + $tag),
        release_assets: [$archive, "checksums.txt"] | sort,
        image: $image,
        image_digest: $digest,
        platforms: ["linux/amd64", "linux/arm64"],
        workflow_run_id: "900",
        architecture_images: {
          "linux/amd64": {image: ($image + "-amd64"), digest: ("sha256:" + ("1" * 64))},
          "linux/arm64": {image: ($image + "-arm64"), digest: ("sha256:" + ("2" * 64))}
        },
        receipt_type: "hotfix-release",
        hotfix_schema_version: 1,
        previous_release: {tag: $root_tag, commit: $root_commit},
        upstream_state: {sync_id: $sync_id, plan_fingerprint: $fingerprint, sha256: $state_digest},
        release_asset_digests: {($archive): $archive_digest, "checksums.txt": $checksums_digest},
        release_workflow: {
          path: ".github/workflows/hotfix-release.yml",
          ref: "unstableneutron/CLIProxyAPIPlus/.github/workflows/hotfix-release.yml@refs/heads/main",
          commit: $commit,
          run_id: "900",
          run_attempt: "1"
        }
      }
    ' > "${node}/hotfix-release-receipt.json"
  rebuild_first_fixture "${root}"
  jq -n \
    --arg head "${commit}" '
      {
        path: ".github/workflows/hotfix-release.yml",
        head_branch: "main",
        head_sha: $head,
        status: "completed",
        conclusion: "success",
        event: "workflow_dispatch",
        run_attempt: 1,
        actor: {login: "unstableneutron", id: 156744497},
        repository: {full_name: "unstableneutron/CLIProxyAPIPlus", id: 1247056725}
      }
    ' > "${node}/run.json"
}

rebuild_first_fixture() {
  local root=$1
  local node="${root}/fixtures/${FIRST_TAG}"
  write_release "${root}" "${FIRST_TAG}" 101 11 12 13 hotfix-release-receipt.json
  write_artifact "${root}" "${FIRST_TAG}" hotfix 900 "$(cat "${root}/first.commit")" hotfix-release-receipt.json 10900
}

rebuild_root_artifact() {
  local root=$1
  write_artifact "${root}" "${ROOT_TAG}" upstream 800 "$(cat "${root}/root.commit")" upstream-sync-receipt.json 10800
}

make_stubs() {
  local root=$1
  mkdir -p "${root}/bin"
  cat > "${root}/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = release ] && [ "${2:-}" = view ]; then
  tag=$3
  jq '{url: .html_url, isDraft: .draft, isPrerelease: .prerelease, assets: [.assets[] | {name}]}' \
    "${STUB_ROOT}/fixtures/${tag}/release.json"
  exit
fi
[ "${1:-}" = api ] || { echo "unexpected gh arguments: $*" >&2; exit 2; }
shift
path=""
jq_filter=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -H) shift 2 ;;
    --jq) jq_filter=$2; shift 2 ;;
    repos/*) path=$1; shift ;;
    *) shift ;;
  esac
done
[ -n "${path}" ] || { echo "missing gh api path" >&2; exit 2; }
case "${path}" in
  repos/*/commits/main) printf '{"sha":"%s"}\n' "$(cat "${STUB_ROOT}/second.commit")" ;;
  repos/*/commits/*unstableneutron.0) printf '{"sha":"%s"}\n' "$(cat "${STUB_ROOT}/root.commit")" ;;
  repos/*/commits/*unstableneutron.1) printf '{"sha":"%s"}\n' "$(cat "${STUB_ROOT}/first.commit")" ;;
  repos/*/compare/*) printf '{"status":"ahead"}\n' ;;
  repos/*/releases/tags/*)
    tag=${path##*/}; cat "${STUB_ROOT}/fixtures/${tag}/release.json" ;;
  repos/*/releases/100) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.0/release.json" ;;
  repos/*/releases/101) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.1/release.json" ;;
  repos/*/releases/assets/1) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.0/archive" ;;
  repos/*/releases/assets/2) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.0/checksums.txt" ;;
  repos/*/releases/assets/3) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.0/upstream-sync-receipt.json" ;;
  repos/*/releases/assets/11) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.1/archive" ;;
  repos/*/releases/assets/12) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.1/checksums.txt" ;;
  repos/*/releases/assets/13) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.1/hotfix-release-receipt.json" ;;
  repos/*/actions/runs/800/artifacts*) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.0/artifacts.json" ;;
  repos/*/actions/runs/900/artifacts*) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.1/artifacts.json" ;;
  repos/*/actions/runs/900/attempts/1) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.1/attempt-1.json" ;;
  repos/*/actions/runs/800) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.0/run.json" ;;
  repos/*/actions/runs/900) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.1/run.json" ;;
  repos/*/actions/artifacts/10800/zip) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.0/artifact.zip" ;;
  repos/*/actions/artifacts/10900/zip) cat "${STUB_ROOT}/fixtures/v7.2.131-unstableneutron.1/artifact.zip" ;;
  *) echo "unexpected gh api path: ${path}" >&2; exit 2 ;;
esac | if [ -n "${jq_filter}" ]; then jq -r "${jq_filter}"; else cat; fi
EOF
  cat > "${root}/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${4:-}" in
  *-amd64) printf '{"digest":"sha256:%s"}\n' "$(printf '1%.0s' {1..64})" ;;
  *-arm64) printf '{"digest":"sha256:%s"}\n' "$(printf '2%.0s' {1..64})" ;;
  *)
    printf '{"digest":"sha256:%s","manifests":[{"digest":"sha256:%s","platform":{"os":"linux","architecture":"amd64"}},{"digest":"sha256:%s","platform":{"os":"linux","architecture":"arm64"}}]}\n' \
      "$(printf 'a%.0s' {1..64})" "$(printf '1%.0s' {1..64})" "$(printf '2%.0s' {1..64})"
    ;;
esac
EOF
  chmod +x "${root}/bin/gh" "${root}/bin/docker"
}

setup_fixture() {
  local root=$1
  local repo="${root}/repo"
  mkdir -p "${repo}"
  run_git -C "${repo}" init -q
  run_git -C "${repo}" config user.name chain-test
  run_git -C "${repo}" config user.email chain-test@example.invalid
  cat > "${repo}/.ccs-fork-upstream.env" <<EOF
SCHEMA_VERSION=2
SYNC_ID=${SYNC_ID}
PLAN_FINGERPRINT=${FINGERPRINT}
BASE_FORK_COMMIT=1111111111111111111111111111111111111111
ORIGINAL_REPOSITORY=router-for-me/CLIProxyAPI
ORIGINAL_TAG=${ORIGINAL_SOURCE_TAG}
ORIGINAL_COMMIT=2222222222222222222222222222222222222222
PLUS_REPOSITORY=kaitranntt/CLIProxyAPIPlus
PLUS_TAG=${PLUS_SOURCE_TAG}
PLUS_TAG_COMMIT=3333333333333333333333333333333333333333
PLUS_HEAD_COMMIT=3333333333333333333333333333333333333333
PLUS_HEAD_INCLUDED=false
MODELS_REPOSITORY=router-for-me/models
MODELS_COMMIT=4444444444444444444444444444444444444444
EXPECTED_FORK_TAG=${ROOT_TAG}
CANDIDATE_BRANCH=upstream-sync/${SYNC_ID}-${FINGERPRINT:0:12}
${EXTRA_STATE_LINE:-}
EOF
  printf 'root\n' > "${repo}/app.txt"
  run_git -C "${repo}" add .
  run_git -C "${repo}" commit -m root >/dev/null
  run_git -C "${repo}" rev-parse HEAD > "${root}/root.commit"
  GIT_COMMITTER_NAME='cliproxy-upstream-sync[bot]' \
    GIT_COMMITTER_EMAIL=cliproxy-upstream-sync@users.noreply.github.com \
    run_git -C "${repo}" tag -a "${ROOT_TAG}" -m "Release ${ROOT_TAG}"
  printf 'first\n' > "${repo}/app.txt"
  run_git -C "${repo}" commit -am first >/dev/null
  run_git -C "${repo}" rev-parse HEAD > "${root}/first.commit"
  GIT_COMMITTER_NAME='cliproxy-hotfix-release[bot]' \
    GIT_COMMITTER_EMAIL=cliproxy-hotfix-release@users.noreply.github.com \
    run_git -C "${repo}" tag -a "${FIRST_TAG}" -m "Hotfix release ${FIRST_TAG} after ${ROOT_TAG}"
  printf 'second\n' > "${repo}/app.txt"
  run_git -C "${repo}" commit -am second >/dev/null
  run_git -C "${repo}" rev-parse HEAD > "${root}/second.commit"
  write_root_fixture "${root}" "$(cat "${root}/root.commit")"
  write_first_fixture "${root}" "$(cat "${root}/first.commit")" "$(cat "${root}/root.commit")"
  make_stubs "${root}"
}

run_chain() {
  local root=$1 candidate=$2 parent=$3 expected_commit=$4 parent_commit=$5 output=$6
  (
    cd "${root}/repo"
    PATH="${root}/bin:${PATH}" \
      STUB_ROOT="${root}" \
      GITHUB_REPOSITORY=unstableneutron/CLIProxyAPIPlus \
      "${VERIFIER}" \
        --tag "${candidate}" \
        --expected-commit "${expected_commit}" \
        --parent-tag "${parent}" \
        --expected-parent-commit "${parent_commit}" \
        --expected-sync-id "${SYNC_ID}" \
        --expected-plan-fingerprint "${FINGERPRINT}" \
        --image "${IMAGE}" \
        --output "${output}"
  )
}

expect_second_failure() {
  local root=$1 expected=$2
  local output="${root}/failure.log"
  if run_chain \
    "${root}" "${SECOND_TAG}" "${FIRST_TAG}" \
    "$(cat "${root}/second.commit")" "$(cat "${root}/first.commit")" \
    "${root}/failed-chain.json" > "${output}" 2>&1; then
    fail "chained verifier unexpectedly accepted malformed evidence"
  fi
  if [ -n "${expected}" ]; then
    grep -Fq "${expected}" "${output}" \
      || fail "expected verifier failure to contain: ${expected}"
  fi
}

test_workflow_legacy_preflight_and_chained_parent() {
  local root
  root=$(mktemp -d)
  setup_fixture "${root}"
  run_chain \
    "${root}" "${FIRST_TAG}" "${ROOT_TAG}" \
    "$(cat "${root}/first.commit")" "$(cat "${root}/root.commit")" \
    "${root}/first-chain.json" >/dev/null
  jq -e '.immediate_parent == .accepted_upstream_root' "${root}/first-chain.json" >/dev/null \
    || fail "legacy .1 workflow preflight did not terminate directly at .0"
  run_chain \
    "${root}" "${SECOND_TAG}" "${FIRST_TAG}" \
    "$(cat "${root}/second.commit")" "$(cat "${root}/first.commit")" \
    "${root}/second-chain.json" >/dev/null
  jq -e \
    --arg parent "${FIRST_TAG}" \
    --arg root_tag "${ROOT_TAG}" \
    '.immediate_parent.tag == $parent and .accepted_upstream_root.tag == $root_tag' \
    "${root}/second-chain.json" >/dev/null \
    || fail "schema-v2 chain output did not bind parent and root"
  rm -rf "${root}"
}

test_accepts_parent_artifact_only_from_earlier_failed_attempt() {
  local root run attempt conclusion
  for conclusion in failure cancelled timed_out; do
    root=$(mktemp -d)
    setup_fixture "${root}"
    run="${root}/fixtures/${FIRST_TAG}/run.json"
    attempt="${root}/fixtures/${FIRST_TAG}/attempt-1.json"
    jq --arg conclusion "${conclusion}" \
      '.id = 900 | .run_attempt = 1 | .conclusion = $conclusion' \
      "${run}" > "${attempt}"
    jq '.run_attempt = 2' "${run}" > "${run}.new"
    mv "${run}.new" "${run}"
    run_chain \
      "${root}" "${SECOND_TAG}" "${FIRST_TAG}" \
      "$(cat "${root}/second.commit")" "$(cat "${root}/first.commit")" \
      "${root}/recovered-parent.json" >/dev/null
    jq -e '.immediate_parent.workflow.run_attempt == "1"' \
      "${root}/recovered-parent.json" >/dev/null \
      || fail "chain did not retain the immutable parent evidence attempt"
    rm -rf "${root}"
  done

  root=$(mktemp -d)
  setup_fixture "${root}"
  run="${root}/fixtures/${FIRST_TAG}/run.json"
  jq '.id = 900 | .run_attempt = 1' "${run}" \
    > "${root}/fixtures/${FIRST_TAG}/attempt-1.json"
  jq '.run_attempt = 2' "${run}" > "${run}.new"
  mv "${run}.new" "${run}"
  expect_second_failure "${root}" "was not a recoverable failure"
  rm -rf "${root}"
}

test_rejects_oversized_compressed_artifact_member() {
  local root node artifact digest size
  root=$(mktemp -d)
  setup_fixture "${root}"
  node="${root}/fixtures/${FIRST_TAG}"
  artifact="${node}/artifact.zip"
  python3 - "${node}" "${artifact}" <<'PY'
import pathlib
import sys
import zipfile

node = pathlib.Path(sys.argv[1])
with zipfile.ZipFile(sys.argv[2], "w", zipfile.ZIP_DEFLATED) as archive:
    archive.write(node / "hotfix-release-receipt.json", "hotfix-release-receipt.json")
    archive.write(node / "hotfix-release-receipt.json", "independently-verified-receipt.json")
    archive.writestr("final-plan.out", b"x" * 1_000_001)
PY
  digest=$(sha256_digest "${artifact}")
  size=$(stat -c %s "${artifact}")
  jq --arg digest "${digest}" --argjson size "${size}" \
    '.artifacts[0].digest = $digest | .artifacts[0].size_in_bytes = $size' \
    "${node}/artifacts.json" > "${node}/artifacts.json.new"
  mv "${node}/artifacts.json.new" "${node}/artifacts.json"
  expect_second_failure "${root}" "archive member exceeds its output limit"
  rm -rf "${root}"
}

test_accepts_planner_sanitized_source_tag_linkage() (
  local root
  PLUS_SOURCE_TAG=v7.2.127+meta
  SYNC_ID=original-${ORIGINAL_SOURCE_TAG}_plus-v7.2.127-meta
  root=$(mktemp -d)
  setup_fixture "${root}"
  run_chain \
    "${root}" "${SECOND_TAG}" "${FIRST_TAG}" \
    "$(cat "${root}/second.commit")" "$(cat "${root}/first.commit")" \
    "${root}/source-tag-chain.json" >/dev/null
  rm -rf "${root}"
)

test_rejects_noncanonical_historical_checksum_separators() {
  local root checksum release separator digest
  for separator in tabs mixed; do
    root=$(mktemp -d)
    setup_fixture "${root}"
    checksum="${root}/fixtures/${FIRST_TAG}/checksums.txt"
    release="${root}/fixtures/${FIRST_TAG}/release.json"
    if [ "${separator}" = tabs ]; then
      sed -i $'s/  /\t\t/' "${checksum}"
    else
      sed -i $'s/  / \t/' "${checksum}"
    fi
    digest=$(sha256_digest "${checksum}")
    jq --arg digest "${digest}" \
      '(.assets[] | select(.name == "checksums.txt") | .digest) = $digest' \
      "${release}" > "${release}.new"
    mv "${release}.new" "${release}"
    expect_second_failure "${root}" "checksums.txt for ${FIRST_TAG} is malformed"
    rm -rf "${root}"
  done
}

test_rejects_historical_receipt_and_planner_drift() {
  local root receipt temporary

  root=$(mktemp -d); EXTRA_STATE_LINE=EXTRA_STATE_KEY=unexpected setup_fixture "${root}"
  expect_second_failure "${root}" "state schema differs"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${FIRST_TAG}/release.json"
  jq '(.assets[] | select(.name == "checksums.txt") | .uploader.id) += 1' \
    "${temporary}" > "${temporary}.new"; mv "${temporary}.new" "${temporary}"
  expect_second_failure "${root}" "invalid asset identity"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${FIRST_TAG}/artifacts.json"
  jq '.artifacts[0].archive_download_url += "/wrong"' \
    "${temporary}" > "${temporary}.new"; mv "${temporary}.new" "${temporary}"
  expect_second_failure "${root}" "artifact for ${FIRST_TAG} has an unexpected identity"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  receipt="${root}/fixtures/${FIRST_TAG}/hotfix-release-receipt.json"
  jq '.receipt_type = "wrong"' "${receipt}" > "${receipt}.new"; mv "${receipt}.new" "${receipt}"
  rebuild_first_fixture "${root}"
  expect_second_failure "${root}" "invalid upstream-state provenance"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  receipt="${root}/fixtures/${FIRST_TAG}/hotfix-release-receipt.json"
  jq '.upstream_state.sha256 = ("f" * 64)' "${receipt}" > "${receipt}.new"; mv "${receipt}.new" "${receipt}"
  rebuild_first_fixture "${root}"
  expect_second_failure "${root}" "invalid upstream-state provenance"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  receipt="${root}/fixtures/${FIRST_TAG}/hotfix-release-receipt.json"
  jq '.release_asset_digests["checksums.txt"] = ("sha256:" + ("f" * 64))' \
    "${receipt}" > "${receipt}.new"; mv "${receipt}.new" "${receipt}"
  rebuild_first_fixture "${root}"
  expect_second_failure "${root}" "release-asset digests differ"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${ROOT_TAG}/run-state.json"
  printf '{}\n' > "${temporary}"
  rebuild_root_artifact "${root}"
  expect_second_failure "${root}" "historical run state"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${ROOT_TAG}/run-state.json"
  jq '.final_plan.plan_fingerprint = ("f" * 40)' \
    "${temporary}" > "${temporary}.new"; mv "${temporary}.new" "${temporary}"
  rebuild_root_artifact "${root}"
  expect_second_failure "${root}" "historical run state"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${FIRST_TAG}/release.json"
  jq '(.assets[] | select(.name == "hotfix-release-receipt.json") | .size) += 1' \
    "${temporary}" > "${temporary}.new"; mv "${temporary}.new" "${temporary}"
  expect_second_failure "${root}" "receipt bytes"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${FIRST_TAG}/release.json"
  jq '(.assets[] | select(.name == "checksums.txt") | .size) += 1' \
    "${temporary}" > "${temporary}.new"; mv "${temporary}.new" "${temporary}"
  expect_second_failure "${root}" "checksums.txt bytes"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  receipt="${root}/fixtures/${FIRST_TAG}/hotfix-release-receipt.json"
  jq '.hotfix_schema_version = "1"' "${receipt}" > "${receipt}.new"; mv "${receipt}.new" "${receipt}"
  rebuild_first_fixture "${root}"
  expect_second_failure "${root}" "schema version must be an integer"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${FIRST_TAG}/run.json"
  jq '.run_attempt = 1.5' "${temporary}" > "${temporary}.new"; mv "${temporary}.new" "${temporary}"
  receipt="${root}/fixtures/${FIRST_TAG}/hotfix-release-receipt.json"
  jq '.release_workflow.run_attempt = "1.5"' "${receipt}" > "${receipt}.new"; mv "${receipt}.new" "${receipt}"
  rebuild_first_fixture "${root}"
  temporary="${root}/fixtures/${FIRST_TAG}/artifacts.json"
  jq '.artifacts[0].name = "hotfix-release-receipt-900-1.5"' \
    "${temporary}" > "${temporary}.new"; mv "${temporary}.new" "${temporary}"
  expect_second_failure "${root}" "workflow run"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${FIRST_TAG}/final-plan.out"
  sed -i 's/^has_changes=false$/has_changes=true/' "${temporary}"
  rebuild_first_fixture "${root}"
  expect_second_failure "${root}" "final plan identity"
  rm -rf "${root}"

  root=$(mktemp -d); setup_fixture "${root}"
  temporary="${root}/fixtures/${FIRST_TAG}/final-plan.out"
  printf 'unexpected_field=value\n' >> "${temporary}"
  rebuild_first_fixture "${root}"
  expect_second_failure "${root}" "final plan fields"
  rm -rf "${root}"

  local baseline copy field
  baseline=$(mktemp -d); setup_fixture "${baseline}"
  for field in \
    fork_tag_prefix latest_fork_suffix next_fork_tag plan_fingerprint candidate_branch \
    snapshot_namespace original_snapshot_ref plus_tag_snapshot_ref plus_head_snapshot_ref \
    models_snapshot_ref plus_head_already_represented plus_head_delta_paths \
    unsafe_plus_head_delta_paths block_reason target_drift_summary; do
    copy=$(mktemp -d)
    cp -a "${baseline}/." "${copy}/"
    temporary="${copy}/fixtures/${FIRST_TAG}/final-plan.out"
    sed -i "s#^${field}=.*#${field}=tampered#" "${temporary}"
    rebuild_first_fixture "${copy}"
    expect_second_failure "${copy}" ""
    rm -rf "${copy}"
  done
  rm -rf "${baseline}"
}

test_rejects_schema_v2_at_suffix_one() {
  local root receipt descriptor
  root=$(mktemp -d)
  setup_fixture "${root}"
  receipt="${root}/fixtures/${FIRST_TAG}/hotfix-release-receipt.json"
  descriptor=$(jq -n \
    --arg tag "${ROOT_TAG}" \
    --arg commit "$(cat "${root}/root.commit")" \
    --arg digest "$(jq -r '.assets[] | select(.name == "upstream-sync-receipt.json") | .digest' "${root}/fixtures/${ROOT_TAG}/release.json")" \
    --arg artifact_digest "$(jq -r '.artifacts[0].digest' "${root}/fixtures/${ROOT_TAG}/artifacts.json")" '
      {
        tag: $tag,
        commit: $commit,
        receipt: {name: "upstream-sync-receipt.json", asset_id: "3", digest: $digest},
        workflow: {path: ".github/workflows/upstream-sync-v2.yml", run_id: "800", run_attempt: "1", head_sha: $commit},
        artifact: {id: "10800", name: "upstream-sync-receipt-800-1", digest: $artifact_digest}
      }
    ')
  jq --argjson descriptor "${descriptor}" '
      .hotfix_schema_version = 2 |
      .previous_release = $descriptor |
      .accepted_upstream_root = $descriptor
    ' "${receipt}" > "${receipt}.new"
  mv "${receipt}.new" "${receipt}"
  rebuild_first_fixture "${root}"
  expect_second_failure "${root}" "requires suffix .2 or later"
  rm -rf "${root}"
}

main() {
  [ -x "${VERIFIER}" ] || fail "chain verifier is missing or not executable"
  for command in gh jq python3; do
    command -v "${command}" >/dev/null || fail "${command} is required"
  done
  test_workflow_legacy_preflight_and_chained_parent
  test_accepts_parent_artifact_only_from_earlier_failed_attempt
  test_rejects_oversized_compressed_artifact_member
  test_accepts_planner_sanitized_source_tag_linkage
  test_rejects_noncanonical_historical_checksum_separators
  test_rejects_historical_receipt_and_planner_drift
  test_rejects_schema_v2_at_suffix_one
  echo "[OK] hotfix chain verifier tests passed"
}

main "$@"
