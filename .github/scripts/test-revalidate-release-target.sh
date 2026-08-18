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
  *) exit 2 ;;
esac
EOF
  chmod +x "${root}/bin/gh"
}

run_verifier() {
  local root=$1 repository=${2:-unstableneutron/CLIProxyAPIPlus}
  PATH="${root}/bin:${PATH}" \
    STUB_MAIN_COMMIT="${STUB_MAIN_COMMIT:-${COMMIT}}" \
    STUB_TAG_COMMIT="${STUB_TAG_COMMIT:-${COMMIT}}" \
    GITHUB_REPOSITORY="${repository}" \
    "${VERIFIER}" "${TAG}" "${COMMIT}"
}

expect_failure() {
  local root=$1 expected=$2 repository=${3:-unstableneutron/CLIProxyAPIPlus}
  if run_verifier "${root}" "${repository}" > "${root}/stdout" 2> "${root}/stderr"; then
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
    expect_failure "${root}" "main or tag moved"
  STUB_TAG_COMMIT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    expect_failure "${root}" "main or tag moved"
  expect_failure "${root}" "repository identity differs" other/example
  rm -rf "${root}"
  echo "[OK] release target revalidation tests passed"
}

main "$@"
