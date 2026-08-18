#!/usr/bin/env bash

RELEASE_ASSETS_SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
RELEASE_ASSET_CONTRACT=${RELEASE_ASSETS_SCRIPT_DIR}/../release-asset-contract.json
# shellcheck source=/dev/null
source "${RELEASE_ASSETS_SCRIPT_DIR}/hotfix-release-tag.sh"

expected_release_assets_json() {
  local tag=$1
  parse_fork_release_tag "${tag}" || return 1
  jq -ce --arg version "${tag#v}" '
      if keys != ["archive_suffixes", "schema_version"] or
         .schema_version != 1 or
         (.archive_suffixes | type) != "array" or
         (.archive_suffixes | length) == 0 or
         (.archive_suffixes | length) != (.archive_suffixes | unique | length) or
         any(.archive_suffixes[]; type != "string" or test("^[a-z0-9_-]+(?:\\.tar\\.gz|\\.zip)$") | not)
      then error("release asset contract is invalid")
      else ([.archive_suffixes[] | "CLIProxyAPIPlus_" + $version + "_" + .] + ["checksums.txt"] | sort)
      end
    ' "${RELEASE_ASSET_CONTRACT}"
}
