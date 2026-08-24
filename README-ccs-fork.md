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

The guarded upstream-sync and hotfix workflows are the only release-producing
entry points. Their reusable binary and image publishers have no direct dispatch
surface, and all root publication workflows share one non-cancelling concurrency
domain. Binary name `cli-proxy-api-plus`, archives
`CLIProxyAPIPlus_<ver>_<os>_<arch>.*`. CCS CLI's `BACKEND_CONFIG.plus.repo`
points here, so guarded releases publish binaries CCS users pick up automatically.

### Chained hotfix policy

`.github/workflows/hotfix-release.yml` is the only supported publication path
for reviewed fixes after an accepted upstream-sync release. A dispatch pins the
exact current `origin/main`, the immediately previous immutable release and
peeled commit, and the consecutive next suffix. A fresh planner must remain a
clean represented no-op, and the candidate tag, release, and GHCR identities
must all be absent.

An accepted upstream-sync release may occupy any numeric suffix in its release
line. Legacy schema-1 receipts remain valid only for the consecutive first
hotfix directly above that recorded root. Schema 2 is required when the
immediate parent is itself a hotfix: each receipt records the immediate parent's
receipt asset, workflow run, and receipt artifact, plus the same identities for
the accepted upstream root. Verification walks the
chain recursively, checks every annotated tag, commit ancestry, stable release,
asset/checksum set, receipt byte identity, workflow artifact, unchanged upstream
state, and architecture image, and rejects cycles or chains over three hotfix nodes
per accepted upstream root so publication and deployment admission remain within the
bounded webhook deadline.
Every release must contain the exact archive matrix declared by
`.github/release-asset-contract.json`; a Go regression test derives that matrix
from the explicit, recognized build/archive IDs, mappings, formats, and name
templates in `.goreleaser.yml`, while publication, recursive verification, and
webhook admission consume the same contract and reject partial, renamed, extra,
duplicate, or default-named assets. Reusable publication callers also bind the
only permitted receipt kind, so upstream and hotfix receipts cannot be mixed.
Gaps, stale main, reused identities, missing historical evidence, drafts,
prereleases, and mismatches stop publication fail closed.

New accepted upstream roots use receipt schema 3. In addition to the image and
planner identities, it binds every non-receipt release asset's immutable GitHub
asset ID, byte size, and SHA-256 digest, plus the exact upstream workflow run and
evidence attempt. `checksums.txt` must cover the complete archive set and match
those API identities. A later successful rerun may authenticate an immutable
receipt/run-state artifact from an earlier attempt of the same run only when the
earlier attempt ended in `failure`, `cancelled`, or `timed_out`. Legacy schema-2
upstream roots and schema-1 first-hotfix receipts remain explicitly supported for
existing releases; new upstream roots are not emitted with those schemas. Shell
publication/chain verification and release-webhook admission enforce the same
schema, byte bounds, attempt recovery, and run-state contract.

Represented no-op planning accepts only the exact schema-2 state key set and
deterministically regenerates its repository, source, fingerprint, candidate,
and inherited accepted-root tag identities. A later hotfix may inherit that
root state, but malformed, missing, duplicate, extra, or drifted fields are not
treated as represented. Scheduled and manual runs use the same verifier.

Before the first hotfix side effect, all candidate tag, release, and GHCR
identities must be absent. Normal dispatch never adopts a preexisting candidate
tag, including an exact tag left by an interrupted run; that identity remains
blocked pending a separately reviewed recovery path. GoReleaser builds the
exact asset matrix without publishing it; the assets, checksums, and a
deterministic manifest are first stored as one immutable Actions artifact.
Release mutation then creates
a draft bound to that artifact ID and digest, validates every already-uploaded
asset against the manifest, uploads only absent names, and publishes only after a
complete recheck. A rerun can resume a matching partial draft from the original
artifact, but conflicting drafts and all incomplete public releases are rejected;
no asset is clobbered or deleted. Final-plan regeneration and the complete
run-attempt artifact are published before the immutable release receipt. If
receipt upload succeeds but
that attempt later fails, only a later attempt of the same workflow run may adopt
the receipt: it must reproduce its bytes and verify the exact earlier-attempt
artifact and final plan, and that earlier attempt must have ended in `failure`,
`cancelled`, or `timed_out`. Successful attempts are never recovery evidence.
Artifact ZIP members are allowlisted and size-bounded before bounded extraction.
Docker platform evidence is likewise immutable and run-attempt-qualified. The
manifest publisher selects one complete current attempt or one earlier
`failure`, `cancelled`, or `timed_out` attempt, authenticates its artifact IDs,
archive URLs, sizes, digests, workflow head, target commit, and architecture
tags, and never overwrites it. Canonical and `latest` indexes must contain the
exact amd64/arm64 platform set and only well-formed, uniquely bound attestation
descriptors. Repository, current main, and tag identities are freshly checked
immediately before every registry or receipt mutation.
The final successful attempt may therefore be later than the immutable evidence
attempt recorded by the receipt. Tags, releases, receipts, and canonical GHCR
tags are never deleted, moved, clobbered, or overwritten during recovery.
The former accepted-tag recovery workflow is intentionally non-mutating and
always rejects: a receipt generated by a recovery run cannot authenticate as an
upstream-sync provenance root. Normal upstream publication uploads the complete
receipt/run-state artifact first and attaches a previously absent receipt once,
without clobber. An interrupted or incomplete accepted release must be superseded
under a separately reviewed guarded policy rather than repaired in place.

## Related issues

- CCS CLI #1062 — runtime degrades `backend: plus → original` until this fork's releases are wired up in `BACKEND_CONFIG.plus.repo`.
