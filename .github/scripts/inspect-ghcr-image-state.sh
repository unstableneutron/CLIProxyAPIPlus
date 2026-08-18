#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 <image-reference> [present-output]" >&2
  exit 2
fi
REFERENCE=$1
PRESENT_OUTPUT=${2:-}
OUTPUT=$(mktemp)
trap 'rm -f "${OUTPUT}"' EXIT

if docker buildx imagetools inspect "${REFERENCE}" > "${OUTPUT}" 2>&1; then
  if [ -n "${PRESENT_OUTPUT}" ]; then
    cp "${OUTPUT}" "${PRESENT_OUTPUT}"
  fi
  echo present
  exit 0
fi

OUTPUT_LINES=$(awk 'END { print NR }' "${OUTPUT}")
if [ "${OUTPUT_LINES}" -eq 1 ] && grep -Eiq \
  '^ERROR: .*(: 404 Not Found|: not found|: manifest unknown(: manifest unknown)?|: no such manifest)$' \
  "${OUTPUT}"; then
  echo absent
  exit 0
fi

echo "Could not determine whether image identity ${REFERENCE} exists:" >&2
cat "${OUTPUT}" >&2
exit 1
