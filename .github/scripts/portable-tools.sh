#!/usr/bin/env bash

# Cross-platform filesystem primitives used by release policy scripts.
# Bash 4+ remains required for mapfile and associative arrays.
if [ "${BASH_VERSINFO[0]:-0}" -lt 4 ]; then
  echo "[portable-tools] Bash 4 or newer is required" >&2
  if [ "${BASH_SOURCE[0]}" != "$0" ]; then
    return 1
  fi
  exit 1
fi

if ! command -v sha256sum >/dev/null 2>&1; then
  sha256sum() {
    shasum -a 256 "$@"
  }
fi

if ! stat -c %s "${BASH_SOURCE[0]}" >/dev/null 2>&1; then
  stat() {
    if [ "${1:-}" = -c ] && [ "${2:-}" = %s ] && [ "$#" -eq 3 ]; then
      command stat -f %z "$3"
      return
    fi
    command stat "$@"
  }
fi

portable_sed_in_place() {
  local expression=$1
  local path=$2
  sed -i.bak "${expression}" "${path}"
  rm -f "${path}.bak"
}

portable_find_release_assets() {
  local directory=$1
  find "${directory}" -maxdepth 1 -type f \
    \( -name '*.tar.gz' -o -name '*.zip' -o -name checksums.txt \) \
    -exec basename {} \; | LC_ALL=C sort
}
