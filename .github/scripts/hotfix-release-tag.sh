#!/usr/bin/env bash

parse_fork_release_tag() {
  local tag=$1
  if [[ "${tag}" =~ ^(v[0-9][0-9A-Za-z._+-]*[-.]unstableneutron)\.(0|[1-9][0-9]*)$ ]]; then
    local suffix=${BASH_REMATCH[2]}
    [ "${#suffix}" -le 16 ] || return 1
    # shellcheck disable=SC2034 # Outputs are consumed by the sourcing script.
    FORK_TAG_PREFIX=${BASH_REMATCH[1]}
    # shellcheck disable=SC2034 # Outputs are consumed by the sourcing script.
    FORK_TAG_SUFFIX=$((10#${suffix}))
    [ "${FORK_TAG_SUFFIX}" -lt 9007199254740991 ] || return 1
    return 0
  fi
  return 1
}

fork_tag_prefix_for_source_tag() {
  local source_tag=$1
  [[ "${source_tag}" =~ ^v[0-9A-Za-z][0-9A-Za-z._+-]*$ ]] || return 1
  if [[ "${source_tag}" == *-* ]]; then
    # shellcheck disable=SC2034 # Output is consumed by the sourcing script.
    FORK_TAG_PREFIX="${source_tag}.unstableneutron"
  else
    # shellcheck disable=SC2034 # Output is consumed by the sourcing script.
    FORK_TAG_PREFIX="${source_tag}-unstableneutron"
  fi
}
