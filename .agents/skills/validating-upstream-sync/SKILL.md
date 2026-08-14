---
name: validating-upstream-sync
description: Validate and repair CLIProxyAPIPlus upstream-sync plans, candidates, sync PRs, v2 promotion, release tags, assets, receipts, and GHCR publication. Use when resolving, retriggering, promoting, or verifying upstream-sync maintenance, including clean no-op and blocked or manual-composition runs.
---

# Validating Upstream Sync

## Core Contract

Never call upstream sync complete from workflow status alone. Prove the immutable target, ownership and invariant state, promoted commit, peeled tag, release, receipt, and multi-platform image.

Keep release maintenance separate from deployment. Unless deployment is explicitly in scope, do not deploy or runtime-smoke VN3; report `runtime_smoke=not_run` and `vn3_deployed=false`.

## Maintenance Flow

### 1. Snapshot and classify the target

- Use the automation worktree and preserve the primary checkout, including unrelated dirty state.
- Fetch and prune current fork, original, Plus, models, candidate, and tag refs on every run.
- Confirm the `original`, `plus`, and `fork` meanings from `AGENTS.md`.
- Run `plan` before `replay-plan`. Treat its fingerprint as the identity of one immutable source snapshot.
- Record `base_fork_commit`, original tag/commit, Plus tag/tag commit/head commit and inclusion state, models commit, sync ID, plan fingerprint, candidate branch, expected fork tag, `has_changes`, `target_drift`, and blocked state.
- Reuse a candidate branch or PR only when its fingerprint exactly matches the current plan.
- If `has_changes=false`, skip mutation and candidate creation. Verify the represented release chain in step 6 and finish `clean-noop`.

### 2. Replay and repair overlays

- Run `replay-plan` for a changing target. Inspect conflicts, provenance, manual-composition state, invariants, symbol survival, build, and tests.
- If a v2 artifact exists for the same fingerprint, inspect its `plan.out`, candidate report, gate logs, provenance, and conflict files.
- Attempt a bounded local repair before reporting manual action. Create or reuse a resolution branch from the plan's fork base and reproduce the failing phase with `merge-ref` in a scratch clone.
- Never run `merge-ref` or harness experiments in the primary checkout. It fetches tags and restores fork-owned paths.
- Apply `.github/upstream-sync-ownership.tsv`:
  - Preserve newer original behavior on original-owned paths and reapply required fork or Plus compatibility.
  - Preserve compatible Plus behavior on Plus-owned paths while adapting to newer original APIs.
  - Preserve explicit fork behavior on fork-owned paths.
  - Manually compose shared hotspots; never blanket `ours` or `theirs`.
- Check `.github/upstream-sync-invariants.tsv`; conflict-free Git merges can still clobber owned behavior.
- After shared-hotspot composition, run `check-symbol-survival <pre-sync-head>`. Restore missing fork-only symbols and `Test*` functions, or document genuine replacements in `.github/upstream-sync-dropped-symbols.tsv` in the same commit.
- Re-check provider fallback, auth and proxy selection, CommandCode, Responses WebSocket continuity, compaction, Gemini CLI, model catalog, aliases, release branding, and CGO or plugin settings when touched.

During repair iteration, rerun `replay-plan`, the failing gate, and focused tests for the changed surface. Do not rerun the full matrix after every edit. Once the repair is stable, run the complete matrix once. If final review causes another code change, rerun its focused checks and the complete matrix.

Stop as `needs-manual-action` only when repository ownership and invariants cannot determine the intended behavior, the repair expands beyond the bounded overlay, required authority or secrets are missing, or the exact repair-import or integration gates cannot be proven.

### 3. Import an exact repaired candidate

`upstream-sync-v2.yml` can import a repaired candidate only through the complete pinned contract: `repair_ref`, `repair_sha`, `repair_fingerprint`, and `repair_pr`. Import means that the reviewed repair SHA is independently revalidated by v2 and then promoted unchanged; it does not mean trusting local evidence or copying a patch onto a different candidate.

- Repair the fingerprinted candidate branch selected by the plan, not an unrelated branch. Run `record-state <plan-file>` after the final composition so `.ccs-fork-upstream.env` records the exact plan before validation.
- Publish or update a PR from that candidate branch with the immutable refs, fingerprint, conflict inventory, ownership decisions, repair summary, and complete gate evidence.
- Immediately before dispatch, require the PR head to equal the locally validated repair SHA, all required checks to belong to that SHA and pass, the PR to be open, non-draft, mergeable, and review-clean, and a fresh plan to select the same fingerprint and upstream refs. Repair candidates may not change `.github/scripts/`, workflow files, or upstream-sync policy manifests; land any intentional pipeline change on `main` before replanning the repair.
- Dispatch repair import with all four pinned inputs. The workflow must independently verify PR identity, base ancestry, selected original and Plus ancestry, exact models snapshot, recorded state, protected sync-policy paths, freshness, provenance, and the complete matrix.
- Manual-composition provenance may become acceptable only when `validate-repair` succeeds for the imported SHA. Never bypass or globally disable the provenance gate.
- Candidate branch SHA, validated repair SHA, promoted `main`, and peeled tag must remain equal. If any identity changes, restart from planning.
- If repair import is unavailable or fails closed, use a merge commit only under explicit interactive authorization or standing automation authorization. Re-fetch immediately and merge with `gh pr merge --merge --match-head-commit <validated-sha>`; never use `--admin` or deferred `--auto`. Then replan and rerun the stable full matrix before promotion.

### 4. Run the stable full matrix

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

Capture the first failing diagnostic with enough context to reproduce it. Treat sandbox, network, cache, or runner failures as infrastructure failures until reproduced as code failures.

### 5. Accept only through v2

Immediately before dispatch, replan and recheck source freshness. Restart validation if selected refs or fingerprint changed.

Use `shadow` only for an explicitly requested non-mutating equivalence check. Confirm it does not move `main`, create a tag or release, or move `latest`.

For acceptance, dispatch only:

```bash
gh workflow run upstream-sync-v2.yml \
  --repo unstableneutron/CLIProxyAPIPlus \
  --ref main \
  -f mode=promote \
  -f force_candidate=false
```

For an imported repair, add the exact pinned inputs:

```bash
gh workflow run upstream-sync-v2.yml \
  --repo unstableneutron/CLIProxyAPIPlus \
  --ref main \
  -f mode=promote \
  -f force_candidate=false \
  -f repair_ref=<fingerprinted-candidate-branch> \
  -f repair_sha=<validated-40-character-sha> \
  -f repair_fingerprint=<plan-fingerprint> \
  -f repair_pr=<pr-number>
```

Never dispatch `.github/workflows-disabled/upstream-sync.yml`. Never create an accepted tag manually. Use `Recover Existing Release Tag` only when an already accepted tag needs publication recovery and its peeled commit is exact.

### 6. Publish an explicitly authorized hotfix

Use `Hotfix Release` only for a reviewed fork fix after a fully accepted upstream-sync release and only with explicit authorization to ship that fix. It is not an alternative way to accept upstream changes.

- Land the focused fix and any release-policy change on `main` in separate reviewed commits. Re-run the stable full matrix and affected isolated runtime canaries before dispatch.
- Require a fresh no-op plan, unchanged `.ccs-fork-upstream.env`, the latest accepted release tag and its exact peeled commit, the exact current `origin/main` SHA, and the exact next numeric fork-tag suffix.
- Dispatch `.github/workflows/hotfix-release.yml` on `main` with all four pinned inputs: `tag`, `expected_commit`, `base_tag`, and `expected_base_commit`. Never create or move the hotfix tag manually.
- The workflow must reject a stale or non-descendant SHA, changed upstream representation, wrong base or suffix, any existing tag/release/image identity, an invalid base receipt, or planner drift. It must rerun the full repository and upstream suite before creating an annotated tag.
- Publish through the checked reusable GoReleaser and multi-platform Docker workflows. Require `hotfix-release-receipt.json`, not an upstream-promotion receipt. Independently regenerate it and verify its previous release, upstream-state hash, workflow/run identity, release asset digests, `checksums.txt`, OCI index, architecture tags, platform digests, and `latest` parity.
- Require the final fetched planner to report `has_changes=false`, `target_drift=false`, and `blocked=false`, with the hotfix tag as `latest_fork_tag`. Subsequent v2 no-op runs must verify the hotfix receipt as the represented release.

`Recover Existing Release Tag` is restricted to the tag recorded by the accepted upstream-sync state. It must not publish or recover a hotfix suffix; stop for a dedicated reviewed recovery policy if a hotfix run partially publishes.

### 7. Verify the published state independently

For a new release, require all of the following:

- Candidate, promote, reusable GoReleaser, Docker amd64, Docker arm64, Docker publish, and final verify jobs have the expected successful conclusions.
- Fingerprinted candidate SHA, fetched remote `main`, local detached `HEAD`, and the peeled expected tag are equal.
- The GitHub release is published, not draft, and contains only `CLIProxyAPIPlus`-branded archives plus checksums and `upstream-sync-receipt.json`.
- Regenerating the receipt with `verify-upstream-release.sh` matches the attached receipt after removing only `workflow_run_id`.
- The versioned GHCR tag and `latest` resolve to the same OCI index digest with `linux/amd64` and `linux/arm64` manifests.
- Every declared Docker target has a versioned architecture tag, such as `<tag>-amd64`, whose digest equals that platform manifest in the canonical OCI index. New target-matrix entries inherit this requirement automatically.
- A final fetched plan reports `has_changes=false`, `target_drift=false`, and `blocked=false`.

Keep `run-state.json` with the immutable target, repair/PR identity, gate result, promoted SHA, release evidence, final planner state, and explicit runtime/deployment flags. Treat it as a ledger, not a substitute for independent verification.

For `has_changes=false`, verify the fetched `main`, peeled `latest_fork_tag`, represented release, receipt, and Docker digest without mutation. Do not use `next_fork_tag` and do not dispatch merely to exercise CI.

Close superseded blocked PRs only after successful acceptance; retain their branches unless deletion is explicitly requested.

## Optional Deployment Proof

Run deployment only when explicitly authorized. Pin the exact released digest, create a rollback anchor, deploy only the intended service, and then prove the running digest and version, local health, model catalogs, REST, SSE, downstream and native WebSocket where supported, compact, affected provider paths, and recent error logs. When advertised, include both `openai/gpt-5.5` and `openai/gpt-5.6-sol`. Keep request evidence bounded and redacted, and inspect structured event/status/error fields rather than grepping arbitrary numeric substrings such as `408` from raw JSON.

## Terminal States

Return exactly one primary state:

- `clean-noop`: the target is already represented and its release chain verifies without mutation.
- `released-clean`: a conflict-free changing target was validated, promoted, and independently verified.
- `released-auto-resolved`: a repaired target was integrated under explicit or standing authorized policy, validated, promoted, and independently verified.
- `needs-manual-action`: a bounded repair or approval remains; include the exact next action.
- `failed`: infrastructure or an unrecoverable execution failure prevented a trustworthy result.

Always report the target refs and fingerprint, candidate and repair state, conflict or failed-gate summary, branch and PR state, full-matrix result, promoted SHA and release evidence when applicable, final planner state, and explicit runtime-smoke and deployment status.
