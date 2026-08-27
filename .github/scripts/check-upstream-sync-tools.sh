#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/portable-tools.sh
source "${SCRIPT_DIR}/portable-tools.sh"

os_name=$(uname -s)
case "${os_name}" in
  Darwin)
    hash_backend=shasum
    install_hint='brew install bash coreutils findutils gh go jq python shellcheck && brew install oven-sh/bun/bun'
    ;;
  Linux)
    hash_backend=sha256sum
    install_hint='install Bash, coreutils, findutils, Git, GitHub CLI, Go, jq, Python 3, and ShellCheck with the system package manager; install Bun from https://bun.sh/docs/installation'
    ;;
  *)
    echo "[upstream-sync-tools] unsupported host kernel: ${os_name}" >&2
    exit 1
    ;;
esac

missing=()
for command_name in awk bash basename bun find gh git go jq mktemp python3 shellcheck sort stat; do
  command -v "${command_name}" >/dev/null 2>&1 || missing+=("${command_name}")
done
type -P "${hash_backend}" >/dev/null 2>&1 || missing+=("${hash_backend}")

if [ "${#missing[@]}" -gt 0 ]; then
  printf '[upstream-sync-tools] missing commands for %s: %s\n' "${os_name}" "${missing[*]}" >&2
  printf '[upstream-sync-tools] install with: %s\n' "${install_hint}" >&2
  exit 1
fi

script_size=$(stat -c %s "${BASH_SOURCE[0]}") || {
  echo "[upstream-sync-tools] filesystem size primitive failed" >&2
  exit 1
}
[[ "${script_size}" =~ ^[1-9][0-9]*$ ]] || {
  echo "[upstream-sync-tools] filesystem size primitive returned an invalid size" >&2
  exit 1
}
if [ "${hash_backend}" = shasum ]; then
  hash_value=$(shasum -a 256 "${BASH_SOURCE[0]}" | awk '{ print $1 }')
else
  hash_value=$(sha256sum "${BASH_SOURCE[0]}" | awk '{ print $1 }')
fi
[[ "${hash_value}" =~ ^[0-9a-f]{64}$ ]] || {
  echo "[upstream-sync-tools] ${hash_backend} returned an invalid digest" >&2
  exit 1
}

probe_dir=$(mktemp -d)
trap 'rm -rf "${probe_dir}"' EXIT
printf 'probe\n' > "${probe_dir}/probe.zip"
mapfile -t probe_assets < <(portable_find_release_assets "${probe_dir}")
if [ "${#probe_assets[@]}" -ne 1 ] || [ "${probe_assets[0]}" != probe.zip ]; then
  echo "[upstream-sync-tools] filesystem enumeration primitive failed" >&2
  exit 1
fi

printf '[OK] upstream-sync tooling is available (os=%s bash=%s go=%s bun=%s hash=%s)\n' \
  "${os_name}" "${BASH_VERSION}" "$(go version | cut -d' ' -f3)" "$(bun --version)" "${hash_backend}"
