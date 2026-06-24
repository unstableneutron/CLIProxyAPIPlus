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
   - Run or inspect the planner output under `scratch/upstream-sync/`.
   - Record original tag, Plus tag/head, sync ID, expected fork tag, and blocked state.
   - After PR resolution, rerun normal-mode acceptance; compare selected refs with PR-reviewed refs. If they changed, restart review.
   - Immediately before merging a resolved PR, rerun planning or `replay-plan`. If `target_drift=true` or replay reports new conflicts, update to the latest selected refs and rerun review.

2. **Resolve the sync PR as an overlay**
   - Treat sync PRs as preview branches until local validation passes.
   - Preserve owner intent from `.github/upstream-sync-ownership.tsv`; no side wins by default on shared hotspots.
   - Check `.github/upstream-sync-invariants.tsv` after conflicts. Non-conflicting Git auto-merges can still clobber owned behavior.
   - Re-check hotspots: fallback/`NoRoute`, executor sanitization, selected-auth/proxy-status, CommandCode, Gemini CLI, live model catalog, aliases, release branding, and CGO/plugin settings.
   - Also re-check hotspots seen in recent overlays: model registry `SupportedEndpoints`; server module/route registration for Amp, usage, GitLab PAT, management CORS, and ChatGPT backend fallback; auth-file callback host, GitLab PAT metadata, Kiro token persistence, `excluded_models`, and post-auth persistence; Gemini OpenAI Responses thought-only handling; watcher synthesis for CommandCode auth and Gemini virtual auths; Codex websocket executor tests when upstream adds raw-payload passthrough coverage.

3. **Run local gates before merging**
   ```bash
   .github/scripts/upstream-sync.sh replay-plan
   .github/scripts/test-upstream-sync.sh
   .github/scripts/upstream-sync.sh check-invariants
   shellcheck .github/scripts/upstream-sync.sh .github/scripts/test-upstream-sync.sh
   go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/upstream-sync.yml
   go build -o test-output ./cmd/server && rm test-output
   go test ./...
   ```
   If a gate fails, capture the diagnostic tail, fix root cause, and rerun the full gate set.

4. **Merge, then run normal-mode sync**
   Fast-forward the validated result into `main`, then trigger acceptance:
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
   - `main` contains the expected commit, and local `main` is clean.
   - Expected fork tag exists locally after fetch and remotely, e.g. `vX.Y.Z-unstableneutron.N`.
   - Upstream-sync, GoReleaser, and Docker workflows succeeded for that tag/main.
   - GitHub release exists; assets use `CLIProxyAPIPlus` branding.
   - Local fetched state proves tag/main/release chain, not just web UI status.
   - Docker multi-arch publish can finish much later than GoReleaser. Poll the Docker workflow to completion; do not rely on package-listing APIs because tokens may lack `read:packages`.

## Smoke Matrix Notes

- Local provider smoke should include `cursor-composer-2.5`, `cursor-default`, `default`, and `cc/deepseek/deepseek-v4-flash` when credentials are available.
- Prod/Pi smoke currently maps to `gust/cursor/composer-2.5` and `gust/cc/ds4-flash`; do not assume prod has cursor `auto` or `default` selectors unless `pi --list-models` shows them.
- If a local provider smoke hits a plan/auth failure that marks a provider unavailable in memory, restart the scratch server before testing later CommandCode/provider selectors.

## Blocked-Sync Triage

If upstream-sync is green but no tag appears:

1. Inspect the run logs for a blocked-sync report or test-gate failure.
2. Check whether the workflow opened, updated, or superseded `upstream-sync/pending-overlay`.
3. If blocked reporting succeeds but the workflow did not fail, treat that as a workflow bug; blocked non-force sync failures must fail after reporting.
4. Reproduce reported failures locally with the gate set above.
5. Patch `main`, push, and rerun normal-mode sync. Do not create the tag manually unless workflow logic is broken.

## Common Mistakes

- Treating workflow-level success as release success.
- Merging a resolved sync PR without running the normal-mode acceptance sync afterward.
- Trusting a conflict-free merge without checking ownership clobbers and invariants.
- Letting original or Plus overwrite fork-owned workflow, Gemini CLI, CommandCode, or release behavior.
- Declaring Docker/release complete before checking the follow-up runs.
