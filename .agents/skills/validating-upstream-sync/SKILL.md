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
   - Record original tag, Plus tag/head, sync ID, `has_changes`, `target_drift`, latest fork tag, next fork tag, and blocked state.
   - If `blocked=false`, `target_drift=false`, and `has_changes=false`, this is a clean no-op only after verifying the existing latest fork tag and artifacts. Do not treat `next_fork_tag` as expected, trigger acceptance, or force rebuild unless the existing artifact chain is missing or intentionally superseded.
   - After PR resolution, rerun normal-mode acceptance; compare selected refs with PR-reviewed refs. If they changed, restart review.
   - Immediately before merging a resolved PR, rerun planning or `replay-plan`. If `target_drift=true` or replay reports new conflicts, update to the latest selected refs and rerun review.

2. **Resolve the sync PR as an overlay**
   - Treat sync PRs as preview branches until local validation passes.
   - If `replay-plan` reports conflicts or failing gates during maintenance, attempt a local repair before declaring the sync blocked. Create or reuse a resolution branch from current fork `main`, reproduce the phase with `merge-ref`, make the smallest overlay fix, and rerun `replay-plan`.
   - Preserve owner intent from `.github/upstream-sync-ownership.tsv`; no side wins by default on shared hotspots.
   - Mechanical owner-class conflicts can use the owner policy as a starting point; shared hotspots must be manually composed. Never blanket `ours` or `theirs`.
   - Check `.github/upstream-sync-invariants.tsv` after conflicts. Non-conflicting Git auto-merges can still clobber owned behavior.
   - When a conflict also exposes test or replay failures, fix the behavioral root cause in the same cycle before acceptance. Recent example: `sdk/cliproxy/auth/conductor.go` needed bootstrap-marker, CommandCode, proxy-selection, `NextRefreshAfter`, and force-mapped stream behavior composed together; the replay then exposed the Codex image edit endpoint drift in `internal/runtime/executor/codex_openai_images.go`.
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
   - Expected fork tag exists locally after fetch and remotely, e.g. `vX.Y.Z-unstableneutron.N`. For no-op plans, verify `latest_fork_tag`, not `next_fork_tag`.
   - Compare annotated tags by their peeled commit (`git rev-parse <tag>^{}`); remote tag object SHA can differ from the commit SHA.
   - Upstream-sync, GoReleaser, and Docker workflows succeeded for that tag/main.
   - GitHub release exists; assets use `CLIProxyAPIPlus` branding.
   - Local fetched state proves tag/main/release chain, not just web UI status.
   - Docker multi-arch publish can finish much later than GoReleaser. Poll the Docker workflow to completion; do not rely on package-listing APIs because tokens may lack `read:packages`.
   - Clean no-op classification requires current `main`, peeled `latest_fork_tag`, release, GoReleaser, and Docker to already line up. Report `clean / already released` or `clean / no-op`, not `released` or `auto-resolved`.

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
6. If the maintenance automation attempted repair and still blocked, include exactly what was tried, the branch publication state, and the next manual action. If repair succeeded, classify the run as `auto-resolved` or `released`, not merely `clean`.

## Common Mistakes

- Treating workflow-level success as release success.
- Treating `next_fork_tag` as expected when `has_changes=false`; the represented artifact is `latest_fork_tag`.
- Merging a resolved sync PR without running the normal-mode acceptance sync afterward.
- Reporting blocked before attempting a reasonable local repair for conflict or gate failures.
- Trusting a conflict-free merge without checking ownership clobbers and invariants.
- Letting original or Plus overwrite fork-owned workflow, Gemini CLI, CommandCode, or release behavior.
- Declaring Docker/release complete before checking the follow-up runs.
