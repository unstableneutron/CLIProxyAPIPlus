#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
VERIFIER="${SCRIPT_DIR}/revalidate-release-target.sh"
TAG=v7.2.135-unstableneutron.2
COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

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
endpoint=""
for argument in "$@"; do
  case "${argument}" in /repos/*) endpoint=${argument} ;; esac
done
case "${endpoint}" in
  */commits/main) printf '%s\n' "${STUB_MAIN_COMMIT}" ;;
  */commits/*) printf '%s\n' "${STUB_TAG_COMMIT}" ;;
  */compare/*) printf '%s\n' "${STUB_COMPARE_STATUS}" ;;
  *) exit 2 ;;
esac
EOF
  chmod +x "${root}/bin/gh"
}

run_verifier() {
  local root=$1 repository=${2:-unstableneutron/CLIProxyAPIPlus} policy=${3:-exact}
  PATH="${root}/bin:${PATH}" \
    STUB_COMPARE_STATUS="${STUB_COMPARE_STATUS:-ahead}" \
    STUB_MAIN_COMMIT="${STUB_MAIN_COMMIT:-${COMMIT}}" \
    STUB_TAG_COMMIT="${STUB_TAG_COMMIT:-${COMMIT}}" \
    GITHUB_REPOSITORY="${repository}" \
    "${VERIFIER}" "${TAG}" "${COMMIT}" "${policy}"
}

expect_failure() {
  local root=$1 expected=$2 repository=${3:-unstableneutron/CLIProxyAPIPlus} policy=${4:-exact}
  if run_verifier "${root}" "${repository}" "${policy}" > "${root}/stdout" 2> "${root}/stderr"; then
    fail "target verifier unexpectedly accepted: ${expected}"
  fi
  grep -Fq "${expected}" "${root}/stderr" \
    || { cat "${root}/stderr" >&2; fail "missing rejection: ${expected}"; }
}

main() {
  local root
  root=$(mktemp -d)
  make_stub "${root}"
  run_verifier "${root}"
  STUB_MAIN_COMMIT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    expect_failure "${root}" "main moved"
  STUB_TAG_COMMIT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    expect_failure "${root}" "tag moved"
  STUB_MAIN_COMMIT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    run_verifier "${root}" unstableneutron/CLIProxyAPIPlus descendant
  STUB_MAIN_COMMIT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    STUB_COMPARE_STATUS=diverged \
    expect_failure "${root}" "main is not descended" unstableneutron/CLIProxyAPIPlus descendant
  expect_failure "${root}" "main policy must be exact or descendant" unstableneutron/CLIProxyAPIPlus invalid
  expect_failure "${root}" "repository identity differs" other/example
  rm -rf "${root}"
  echo "[OK] release target revalidation tests passed"
}

main "$@"
