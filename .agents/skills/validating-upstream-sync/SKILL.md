---
name: validating-upstream-sync
description: Use when resolving, merging, retriggering, or verifying this fork's upstream-sync workflow, sync PRs, release tags, assets, Docker publishes, owned overlays, or blocked-sync reports.
---

# Validating Upstream Sync

## Core Rule

Never call upstream sync done from a green workflow alone. Prove actual `main`, tag, release, Docker, ownership, and invariant state. A run can look green while only reporting a block, and a resolved PR can be superseded by newer original or Plus refs during normal-mode acceptance.

## Required Flow

1. **Identify the sync target**
   - Confirm `original`, `plus`, and `fork` terms from `AGENTS.md`.
   - Run `plan` or inspect the v2 candidate artifact. Record `base_fork_commit`, original tag/commit, Plus tag/tag commit/head commit, whether Plus head is included, models commit, sync ID, plan fingerprint, candidate branch, expected fork tag, `has_changes`, `target_drift`, and blocked state.
   - The fingerprint identifies one immutable source snapshot. Never validate one fingerprint and promote another.
   - If `has_changes=false`, verify the represented tag and attached receipt. Do not force a rebuild merely to exercise the workflow.
   - Immediately before accepting a repaired PR, rerun planning or `replay-plan`. If selected refs or the fingerprint changed, restart validation against the new snapshot.

2. **Resolve blocked candidates as overlays**
   - Download the `upstream-sync-v2-<run>-<attempt>` artifact and inspect `plan.out`, `report/candidate.md`, gate logs, provenance, and conflict files.
   - If `replay-plan` reports conflicts or failing gates during maintenance, attempt a local repair before declaring the sync blocked. Create or reuse a resolution branch from current fork `main`, reproduce the phase with `merge-ref`, make the smallest overlay fix, and rerun `replay-plan`.
   - Preserve owner intent from `.github/upstream-sync-ownership.tsv`; no side wins by default on shared hotspots.
   - Mechanical owner-class conflicts can use the owner policy as a starting point; shared hotspots must be manually composed. Never blanket `ours` or `theirs`.
   - Check `.github/upstream-sync-invariants.tsv` after conflicts. Non-conflicting Git auto-merges can still clobber owned behavior.
   - Run `check-symbol-survival <pre-sync-head>` after composing shared hotspots. Restore every missing fork-only symbol or `Test*` function unless a documented upstream replacement justifies adding it to `.github/upstream-sync-dropped-symbols.tsv`.
   - When resolution intentionally drops overlay code, record it in `.github/upstream-sync-dropped-symbols.tsv` in the same commit. Do not pre-populate the allowlist with historical noise.
   - Re-check high-value fork surfaces: provider fallback, auth/proxy selection, CommandCode, Responses WebSocket continuity, compaction, Gemini CLI, model catalog, aliases, release branding, and CGO/plugin settings.

3. **Run local gates before merging**
   ```bash
   .github/scripts/upstream-sync.sh replay-plan
   .github/scripts/test-upstream-sync.sh
   .github/scripts/test-verify-upstream-release.sh
   .github/scripts/upstream-sync.sh check-invariants
   .github/scripts/upstream-sync.sh check-symbol-survival <pre-sync-head>
   shellcheck .github/scripts/*.sh
   go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
   go build -o test-output ./cmd/server && rm test-output
   go test ./...
   ```
   If a gate fails, capture the diagnostic tail, fix root cause, and rerun the full gate set.
   Never run `merge-ref` or harness experiments with the real repository as the working directory; use a scratch clone. `merge-ref` fetches tags and restores fork-owned paths, which reverts uncommitted work in the live worktree.

4. **Run v2 acceptance**
   Use `shadow` for non-mutating equivalence. Confirm it publishes only the fingerprinted candidate and does not move `main`, create a tag, publish a release, or move `latest`.
   ```bash
   gh workflow run upstream-sync-v2.yml \
     --repo unstableneutron/CLIProxyAPIPlus \
     --ref main \
     -f mode=shadow \
     -f force_candidate=false
   ```
   After shadow equivalence and local review, run the same workflow with `mode=promote`. Scheduled runs use promote semantics automatically.
   ```bash
   gh workflow run upstream-sync-v2.yml \
     --repo unstableneutron/CLIProxyAPIPlus \
     --ref main \
     -f mode=promote \
     -f force_candidate=false
   ```
   The legacy implementation is retained only at `.github/workflows-disabled/upstream-sync.yml`; do not dispatch it during normal maintenance.

5. **Verify artifacts, not just checks**
   Required success evidence:
   - Candidate, promote, reusable GoReleaser, reusable Docker, and verify jobs reached the expected terminal conclusions.
   - Current `main` equals the promoted candidate SHA for a new release, and local fetched state is clean.
   - Expected fork tag exists locally after fetch and remotely, e.g. `vX.Y.Z-unstableneutron.N`. For no-op plans, verify `latest_fork_tag`, not `next_fork_tag`.
   - Compare annotated tags by their peeled commit (`git rev-parse <tag>^{}`); remote tag object SHA can differ from the commit SHA.
   - GitHub release is published and all binary archives use `CLIProxyAPIPlus` branding.
   - The release contains `upstream-sync-receipt.json`. Regenerate it with `verify-upstream-release.sh`, ignore only `workflow_run_id`, and require an otherwise exact match.
   - The fork tag and `latest` image references share one OCI index digest containing `linux/amd64` and `linux/arm64`.
   - Clean no-op classification requires current `main`, peeled represented tag, release, receipt, and Docker digest to line up. Report `clean / already released` or `clean / no-op`, not `released`.
   - When deployment is in scope, verify the running image digest, health, real REST/SSE/WebSocket/compaction/provider paths, and post-deploy logs before reporting success.

## Smoke Matrix Notes

- Use model IDs advertised by the surface being tested. The direct API advertises CommandCode IDs such as `cc/ds4-flash`; Pi may expose the provider-scoped selector as `gust/cc/ds4-flash`.
- Exercise REST, SSE, downstream Responses WebSocket, native Responses WebSocket where supported, and `/v1/responses/compact`.
- `tools/upstream-sync-smoke` emits redacted JSON outcomes and treats terminal events and exact markers as acceptance criteria.
- If a local provider smoke hits a plan/auth failure that marks a provider unavailable in memory, restart the scratch server before testing later CommandCode/provider selectors.

## Blocked-Sync Triage

If v2 does not produce the expected tag:

1. Inspect each job conclusion; a successful candidate with skipped promotion can be a legitimate no-op.
2. Download the candidate artifact and inspect the first failed gate, freshness reason, conflicts, and manual-composition flag.
3. Verify the fingerprinted candidate branch and any PR publication state.
4. Reproduce and repair locally with the exact refs, then rerun the full gate set and v2 promotion.
5. Do not create a tag manually. If an accepted tag already exists but publication failed, use `Recover Existing Release Tag` with the exact peeled commit.
6. Report the exact refs, candidate SHA, conflict files or failing gate, attempted repair, branch publication state, and next manual action.

## Common Mistakes

- Treating workflow-level success as release success.
- Treating `next_fork_tag` as expected when `has_changes=false`; the represented artifact is `latest_fork_tag`.
- Validating one plan fingerprint and promoting a candidate built from another.
- Dispatching the retired legacy workflow instead of `upstream-sync-v2.yml`.
- Reporting blocked before attempting a reasonable local repair for conflict or gate failures.
- Trusting a conflict-free merge without checking ownership clobbers and invariants.
- Accepting a resolution where `check-symbol-survival` failures are neither restored nor allowlisted with reasons.
- Letting original or Plus overwrite fork-owned workflow, Gemini CLI, CommandCode, or release behavior.
- Declaring Docker/release complete before verifying the attached receipt and `latest` digest parity.
- Using a Pi provider-scoped model selector directly against the API without checking `/v1/models`.
