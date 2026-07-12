# CCS Fork Notes

This is the CCS-maintained fork of `router-for-me/CLIProxyAPIPlus`, snapshotted at commit `0c48ef58` (2026-04-17) under MIT before the upstream repo was deleted and rebranded as the SSPL-licensed `CLIProxyAPIBusiness`.

## Why this fork exists

- Upstream `router-for-me/CLIProxyAPIPlus` was deleted 2026-04-17.
- Successor repo `CLIProxyAPIBusiness` is SSPL-licensed — incompatible with CCS redistribution.
- Plus providers (`codebuddy`, `copilot`, `cursor`, `gitlab`, `iflow`, `kilo`, `kiro`) do **not** exist in the still-alive `router-for-me/CLIProxyAPI` (original MIT). They live only here.
- Our MIT snapshot predates the rebrand; MIT rights do not retroactively revoke.

## Upstream Sync

The daily candidate-first workflow (`.github/workflows/upstream-sync-v2.yml`) snapshots exact commits from `router-for-me/CLIProxyAPI`, the retained Plus source, and the model catalog. It materializes and validates one candidate SHA, then promotes that same SHA through tag, release, and multi-architecture Docker publication.

- **Clean candidate + all gates green** → fast-forward `main`, publish the expected fork tag, and attach an independently verifiable release receipt.
- **Conflict, ownership hotspot, stale source, or failing gate** → retain the fingerprinted candidate and open a review PR without mutating `main` or release artifacts.
- **Already represented target** → verify the existing tag, release, Docker digest, and receipt without rebuilding.
- Manual trigger: Actions → "Upstream Sync v2" → choose `shadow` or `promote`.

The retired workflow is retained at `.github/workflows-disabled/upstream-sync.yml` only as a rollback reference; GitHub does not execute workflows outside `.github/workflows/`.

## What NOT to pull in

- Any code from `router-for-me/CLIProxyAPIBusiness` — SSPL would infect this fork and any downstream user of CCS. Do not copy, cherry-pick, or look at it as reference.
- Fixes to the 7 plus-only providers must be self-authored or sourced from MIT-compatible contributions only.

## Releases

`release.yaml` builds via goreleaser on tag push. Binary name `cli-proxy-api-plus`, archives `CLIProxyAPIPlus_<ver>_<os>_<arch>.*`. CCS CLI's `BACKEND_CONFIG.plus.repo` points here, so tagging a release (e.g., `v6.9.19-ccs.1`) publishes binaries CCS users pick up automatically.

## Related issues

- CCS CLI #1062 — runtime degrades `backend: plus → original` until this fork's releases are wired up in `BACKEND_CONFIG.plus.repo`.
