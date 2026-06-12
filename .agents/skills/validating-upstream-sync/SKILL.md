---
name: validating-upstream-sync
description: Use when resolving, merging, retriggering, or verifying this fork's upstream-sync workflow, sync PRs, release tags, GoReleaser assets, Docker publishes, or blocked-sync reports.
---

# Validating Upstream Sync

## Core Rule

Do not call upstream sync done from a green GitHub workflow alone. In this repo, the upstream-sync workflow can complete successfully while writing a blocked-sync report instead of creating the fork release tag.

## Required Flow

1. **Identify the sync target**
   - Confirm `original`, `plus`, and `fork` terms from `AGENTS.md`.
   - Run or inspect the planner output under `scratch/upstream-sync/`.
   - Record the selected original tag, plus tag/head, safe sync ID, expected fork tag, and whether the plan is blocked.

2. **Resolve the sync PR as an overlay**
   - Treat sync PRs as preview branches until local validation passes.
   - Preserve fork-owned behavior over original/plus where intentional.
   - Re-check overlay hotspots after conflict resolution:
     - fallback / `NoRoute` handler composition
     - executor header sanitization
     - selected-auth / proxy-status context propagation
     - CommandCode registration, live model catalog, and alias behavior
     - release branding and CGO/plugin settings

3. **Validate locally before merging**
   ```bash
   go test ./... -count=1 -timeout 10m
   go build -o test-output ./cmd/server && rm test-output
   ```
   If the workflow test gate failed, first reproduce the failing packages locally, fix the root cause, then rerun the full suite.

4. **Merge, then run normal-mode sync**
   After the resolved PR is merged and `main` is pushed, trigger the acceptance run:
   ```bash
   gh workflow run upstream-sync.yml \
     --repo unstableneutron/CLIProxyAPIPlus \
     --ref main \
     -f force_pr=false \
     -f force_rebuild=false
   ```
   This proves current `main` can fast-forward/tag/release without another conflict PR.

5. **Verify artifacts, not just checks**
   Required success evidence:
   - upstream-sync run conclusion is `success`
   - expected fork tag exists, e.g. `vX.Y.Z-unstableneutron.N`
   - GoReleaser follow-up run succeeds
   - GitHub release exists and assets use `CLIProxyAPIPlus` branding
   - Docker image workflow succeeds
   - local `main` is clean and has fetched the new tag

## Watcher Pattern

Use `background-task-watcher` for long CI/release/Docker runs, but keep the parent thread responsible for final verification. Give the watcher exact run URLs and expected artifacts. If the watcher times out or is quiet, the parent should check with `gh run view` directly before closing the goal.

## Blocked-Sync Triage

If upstream-sync is green but no tag appears:

1. Inspect the run logs for a blocked-sync report or test-gate failure.
2. Check whether the workflow opened or superseded a sync PR.
3. Reproduce the reported test failures locally.
4. Patch `main`, push, and rerun normal-mode sync.
5. Do not create the tag manually unless the workflow logic is confirmed broken.

## Common Mistakes

- Treating workflow-level success as release success.
- Merging a resolved sync PR without running the normal-mode acceptance sync afterward.
- Re-merging plus tags already represented in history.
- Letting original or plus overwrite fork-owned GitHub workflow behavior.
- Declaring Docker/release complete before checking the follow-up runs.
