---
name: working-on-cliproxy-provider-executors
description: Use when adding, fixing, or validating CLIProxyAPI provider executors, live model catalogs, OpenAI-compatible streams, tool calls, Responses events, or provider auth/model registration.
---

# Working on CLIProxyAPI Provider Executors

## Overview

Provider work crosses executor, registry, service binding, auth/config synthesis,
and translator paths. Validate the full route, not just the executor helper.

## Required Workflow

1. **Map the route first**
   - Executor: `internal/runtime/executor/`.
   - Service binding/model registration: `sdk/cliproxy/service.go`.
   - Static and remote model metadata: `internal/registry/`.
   - Config/auth synthesis: `internal/config/`, `internal/watcher/synthesizer/`, `sdk/cliproxy/auth/`.
   - OpenAI-compatible auths may register under provider key `openai-compatibility`; verify the actual registry bucket.
   - Translators only when the provider output shape requires broader protocol changes.

2. **Model catalog rule**
   - Prefer live provider catalogs when available.
   - Keep static fallback for fetch failure, non-2xx, bad JSON, and empty results.
   - Preserve explicit config model overrides and apply excluded models after selection.
   - If an empty catalog section intentionally relies on builtin fallback, suppress noisy warnings with a focused test.

3. **Streaming rule**
   - Normalize provider events into stable OpenAI-compatible chunks before downstream translation.
   - Test text deltas, reasoning deltas, tool start/delta/end, final tool-call dedupe by count, finish reasons, usage, cache tokens, reasoning tokens, and terminal `[DONE]` / `response.completed`.
   - Do not double-count cache tokens when the provider supplies `totalTokens`.
   - Codex/XAI Responses streams may leave `response.completed.response.output` empty and carry real items only in `response.output_item.added/done`. Any consumer of completed output (transcript recording, compaction replay, downstream state, non-stream translation) must reconstruct output from streamed items first. Reuse the existing `collectCodexOutputItemDone`/`patchCodexCompletedOutput` (or `xaiCollectOutputItemDone`/`xaiPatchCompletedOutput`) helpers — do not write new variants.

4. **Fork-overlay rule**
   - Before changing a shared-hotspot file (large fork diff vs original upstream, e.g. `openai_responses_websocket.go`, `openai_responses_handlers.go`, `codex_websockets_executor.go`), check whether upstream already solved the problem elsewhere in the package and mirror that pattern exactly; identical dupes that exist upstream stay duplicated to keep merge conflicts small.
   - Prefer putting fork-only additions in a fork-owned sibling file with minimal one-line call sites in the hotspot file; separate files survive upstream-sync merges, inline hunks often do not.
   - When landing a fork-critical behavior in a shared hotspot, add a `contains` entry for its call site and its regression test name to `.github/upstream-sync-invariants.tsv` in the same change.

5. **Validation rule**
   - Add focused tests at the layer that owns the behavior, then integration coverage through service registration when models/auth are involved.
   - Run:
     ```bash
     gofmt -w <changed-go-files>
     go build -o test-output ./cmd/server && rm test-output
     go test ./internal/runtime/executor/... ./internal/registry/... ./sdk/cliproxy/... -count=1
     ```
   - Add `./test` when Responses, thinking, translator, request-conversion, or end-to-end protocol behavior changes.

## Common Mistakes

- Testing parser helpers but not `registerModelsForAuth`.
- Trusting a first `/v1/models` response during startup; auth-discovered providers can register a moment later.
- Emitting streamed tool calls twice when a provider sends both incremental input and final consolidated tool-call events.
- Treating Responses streams as complete without proving `response.completed` or equivalent terminal output.
- Writing validation artifacts outside `scratch/`.
