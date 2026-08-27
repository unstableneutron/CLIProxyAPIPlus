# Fork Maintenance Audit

## Scope and evidence

This audit records which differences in this fork are intentional, inherited, obsolete, or still insufficiently characterized. Its purpose is to reduce future upstream-sync conflict work without deleting behavior merely because it differs from one upstream.

Primary comparison snapshot:

- Fork: `3853349bd27e4bc3f38d1e863593664ada55b4fb`
- Original: `router-for-me/CLIProxyAPI` `v7.2.143` (`4b5f1eab25fca4b3815369a826e958e7c070a69e`)
- Plus: `kaitranntt/CLIProxyAPIPlus` `v7.2.127-7` (`f5570ed69c3b82e3ec789b986a7f61396af49180`)

The audit used pinned source trees, current callers and tests, `.github/upstream-sync-ownership.tsv`, `.github/upstream-sync-invariants.tsv`, and `.ccs-fork-upstream.env`. Follow-up release-policy and Responses fixes were merged as PRs #98 and #99; they do not change the classifications below.

Classification meanings:

- **Keep unique**: deliberate fork behavior with active callers, tests, policy, or security value.
- **Upstream equivalent**: inherited Original or Plus behavior. Keep it, but allow normal upstream replacement when equivalence is proven.
- **Obsolete residue**: compatibility-only code with no live caller and a canonical replacement.
- **Needs characterization**: provenance or behavior is not understood well enough for safe deletion.

## Decisions

1. Keep both upstreams as distinct sources. Original owns the newer core; Plus supplies provider families and integrations absent from Original.
2. Preserve fork policy and runtime features explicitly. Absence from Original is not evidence that Plus or fork behavior is obsolete.
3. Delete only evidence-backed residue. A filename containing `overlay`, `compat`, or `fork` is not a deletion criterion.
4. Treat shared files symbol-by-symbol. Many shared files contain both upstream-equivalent machinery and fork-only branches.
5. Keep synchronization, release, and deployment separate. Release success does not imply deployment, and deployment health does not prove release provenance.

## Cleanup completed

| Path / symbol | Evidence | Decision |
|---|---|---|
| `internal/runtime/executor/antigravity_reasoning_replay_overlay_compat.go` / `(*antigravityReasoningReplayAccumulator).Flush` | One-line delegation to canonical `Commit`; no production or test caller found; the accumulator type is unexported. | Deleted. |
| `sdk/api/handlers/openai/openai_responses_websocket.go` compile anchors for `finalizeResponsesWebsocketRequest` and `stripUnsupportedResponsesWebsocketInputItemMetadata` | Both functions have direct callers in `openai_responses_websocket_requests.go`; the declarations carried no behavior. | Deleted declarations only; functions and callers retained. Invariant paths now point at the implementation file. |
| Legacy `test-upstream-sync-tracker-fixtures.sh`, `upstream-sync.yml`, and `sync-validation-status.yml` | Already absent; superseded by the candidate/provenance v2 synchronization lifecycle. | Keep deleted; do not resurrect. |
| `internal/watcher/diff/model_hash_fork_overlay_test.go` and earlier executor/translator compatibility overlays | Already removed before this audit; current canonical implementations and tests cover the behavior. | Keep deleted. |

The ownership manifest was expanded to cover fork policy, plugins, tools, runtime helpers, and protocol surfaces that were previously under-specified.

## Retained fork behavior

### Runtime and provider execution

| Area | Why it remains fork-owned |
|---|---|
| `internal/runtime/executor/codex_continue_fold.go` | Codex continuation state machine for reasoning truncation, rewind/replay, terminal fallback, usage aggregation, and event rewriting. Active in HTTP and WebSocket execution and protected by focused tests. |
| `internal/runtime/executor/codex_websocket_compaction_overlap.go` | Deduplicates full-history overlap during WebSocket compaction replay. Required by synchronization invariants and compaction tests. |
| `internal/runtime/executor/bedrock_executor.go` | Native Bedrock executor and transport behavior absent from both pinned upstreams; registered and tested through the service. |
| Command Code registry, executor, model catalog, and service wiring | Fork provider absent from Original and not replaceable by Plus. Config, catalog, watcher hashes, registration, and executor must be changed as one feature. |
| `internal/runtime/executor/xai_executor_overlay_compat.go` / `xaiBaseURLForLog` | Security policy that redacts credentials and query data from XAI URL logs. Original logs the raw base URL. |
| `internal/runtime/executor/helps/logging_helpers.go` / `RecordAPIHTTPResponseMetadata` | Preserves HTTP protocol, status, and headers for request/response capture across multiple providers. Active callers remain. |

Plus-owned provider infrastructure such as Cursor, iFlow, Gemini CLI, AiStudio, websocket relay, and their auth/translator packages is retained. It is upstream-equivalent to Plus, not residue.

### Protocol and API behavior

| Area | Why it remains fork-owned |
|---|---|
| `internal/api/chatgpt_backend_passthrough.go` | Authenticated `/backend-api/*` passthrough with account-bound Codex credentials, header filtering, uTLS transport, and response relay. No pinned upstream equivalent. |
| `sdk/api/handlers/force_model_prefix.go` | `X-Force-Model-Prefix` routing policy used by OpenAI, Claude, Gemini, Gemini CLI, multipart, path, and WebSocket request flows. |
| Direct Responses state cache in `openai_responses_handlers.go` | Bounded, expiring response state with pending tool IDs and originating-auth pinning; prevents cross-auth response-ID reuse. |
| `sdk/api/handlers/openai/codex_fast_model.go` | Maps supported `*-fast` client models to the base model plus priority service tier while preserving thinking suffix behavior. |
| WebSocket input-item metadata stripping | Avoids upstream request rejection. Only the redundant compile anchors were removed. |
| Gemini CLI and iFlow thinking/translator packages | Plus-equivalent implementations protected as fork-owned policy because this fork depends on those provider families. |

Original-owned route, middleware, thinking, and core Responses WebSocket behavior remains inherited normally. It must not be relabeled as fork behavior solely because it is heavily modified by upstream syncs.

### Platform and configuration

Retained coordinated fork features:

- Command Code config types, normalization, aliases, exclusions, watcher hashes, model summaries, registry entries, and executor binding.
- Kiro model discovery, cache, runtime configuration, lifecycle wiring, and executor-backed model resolution.
- Bedrock/provider dimensions in config diff and model hashing.
- Responses-state capability configuration.

Generic conductor cooldown, scheduler, and custom-header helpers match Original behavior. They remain active upstream-equivalent code, not cleanup targets.

### Synchronization, release, and operations

The following are intentional fork policy and have no equivalent guarded lifecycle in either pinned upstream:

- Candidate-first `.github/workflows/upstream-sync-v2.yml`.
- Ownership-aware planning, materialization, provenance, invariant, dropped-symbol, freshness, and repair validation in `.github/scripts/upstream-sync.sh` and its helpers.
- Consecutive chained hotfix verification and publication in `.github/workflows/hotfix-release.yml`.
- Staged immutable release assets, digest-first multi-platform Docker publication, registry-index verification, and no-clobber revalidation.
- Recovery-only `.github/workflows/sync-release-tag.yml` for an already accepted upstream root.
- Amp release and upstream-sync webhook admission/provenance plugins.
- Pinned state, ownership, invariant, dropped-symbol, and release-asset contracts.
- `tools/upstream-sync-smoke`, `tools/request-fingerprint-probe`, and `tools/release-asset-contract`.
- Digest-pinned Docker build policy, `mise.toml`, shared agent skills, setup hooks, and release runbooks.

These files are high-value safeguards, but also the fork's largest maintenance concentration. Simplification must preserve immutable identity checks, no-clobber behavior, bounded artifact readers, and final pre-mutation revalidation.

## Recommended synchronization enforcement

The current gate is over-specified at the wrong layer:

- `.github/upstream-sync-invariants.tsv` contains 58 literal-presence rows: 31 implementation anchors and 27 test-name anchors.
- `.github/upstream-sync-ownership.tsv` contains 96 path rules.
- `.github/upstream-sync-dropped-symbols.tsv` contains 59 approvals, all bound to the current plan fingerprint.
- Several literal checks can pass on comments or compatibility declarations rather than live wiring. The Command Code registration rows match a header comment in `sdk/cliproxy/service.go`; the registrations live in `service_executors.go`. Management route rows have the same problem across `server.go` and `server_management.go`.

Neither Codemod nor an ast-grep rule catalog is a suitable replacement. Codemod is a transformation system, not a behavioral or Git-provenance verifier. AST rules eliminate comment matches but recreate a manually maintained inventory and still cannot prove call binding, reachability, cross-file state relationships, Git ancestry, ownership policy, or immutable publication identity.

Use this smaller enforcement model:

1. Keep immutable target identity, final freshness, ancestry, no-clobber publication, and ownership-aware conflict handling.
2. Keep one dynamic Go declaration-survival comparison between the accepted fork base and the composed candidate. Keep fingerprint-bound dropped-symbol approvals, but treat them as temporary records and protect the approval file from candidate-authored repair changes.
3. Use compilation and focused behavior tests for runtime contracts. They replace literal test-name and implementation-text anchors; they do not replace Git/provenance gates.
4. Remove `.github/upstream-sync-invariants.tsv`, its `contains` evaluator, and declarations/comments retained only to satisfy it after focused tests cover the retained behavior.
5. Run each gate once through the validation driver. Workflows should consume its result rather than repeat invariant, symbol, build, and test commands.

The ownership manifest should remain during this reduction. A later patch-stack pilot can test a cleaner long-term representation: materialize the pinned Original/Plus base, replay a small fork-only commit stack, and review `git range-diff` output. Do not rewrite `main` or remove ownership/symbol-survival policy until one complete sync candidate proves the patch stack preserves every retained customization.

## Technical-debt register

| Priority | Area | Unknown or cost | Exit criterion |
|---|---|---|---|
| P1 | Cursor executor additions | Client-version headers, H2 header handling, MCP schema validation/deduplication, raw protocol logging, model aliases, and resume behavior are current-only relative to pinned Plus. Their individual ownership is not recorded. | Produce a symbol-level behavior matrix against the next Plus release; mark each symbol fork-owned or adopt the Plus implementation with focused Cursor tests. |
| P1 | HTTP response metadata divergence | iFlow and Gemini CLI largely match Plus but call `RecordAPIHTTPResponseMetadata` instead of Plus's canonical metadata helper. | Decide whether protocol capture is a required fork contract. If yes, add explicit invariants/tests; if no, migrate all callers together and remove the adapter only when no callers remain. |
| P1 | Service provider overlays | `sdk/cliproxy/service_provider_overlays.go` mixes provider-specific resolution, including Command Code, with Plus behavior. | Compare each branch against pinned Original and Plus; split ownership in the manifest or tests without creating a second registration path. |
| P2 | Store, cache, usage, and TUI deltas | Exact current-vs-upstream provenance was not completed. These areas may contain both inherited and fork behavior. | Audit by exported behavior and live callers before any cleanup. No filename-based deletions. |
| P2 | Gemini CLI ownership | Implementations are Plus-equivalent but intentionally marked fork-owned. This reduces accidental deletion but increases future manual composition. | Decide whether to keep policy ownership or move to Plus ownership after verifying every translator/applier path and invariant. |
| P2 | Agent skill discovery | `.agents/` is consumed by external coding harnesses, so source-code caller search cannot prove usage. | Keep fork-owned unless the supported harness inventory and `skills-lock.json` references prove a narrower safe set. |
| P2 | Manual diagnostic utilities | `cmd/mcpdebug`, `cmd/protocheck`, `cmd/qoder_replay`, and the request fingerprint probe have few or no production callers. | Inventory operator usage before deletion. Preserve any utility that shortens provider incident diagnosis. |
| P3 | Release-policy complexity | The release webhook, upstream-sync script/test, and hotfix-chain verifier are large, high-churn policy hotspots. | Reduce only by consolidating duplicated pure validation logic behind existing command contracts; retain independent verifier boundaries and negative tests. |
| P3 | Literal upstream invariants | The 58 path/string checks duplicate tests and can pass on comments or compile anchors. | Delete the literal invariant table and evaluator after focused tests cover each retained behavior; keep dynamic declaration survival and Git policy gates. |
| P3 | Upstream replay test observability | Candidate replay currently reports only the temporary `tests.log` path, which disappears with the replay worktree on CI failure. | Upload or print a bounded failing-test summary while keeping full logs redacted and bounded. |

## Validation performed for this cleanup

- `go test -race -run '^TestAntigravityConcurrentRequestsReusePooledConnections$' -count=200 ./internal/runtime/executor`
- `go test -count=1 -timeout=10m ./...`
- Focused OpenAI handler tests after declaration cleanup.
- `go build -o test-output ./cmd/server && rm test-output`
- `.github/scripts/test-upstream-sync.sh`
- `.github/scripts/validate-upstream-sync.sh --mode tooling --report-dir /tmp/fork-audit-ownership-validation`
- `.github/scripts/upstream-sync.sh replay-plan` against the exact current main and pinned upstream refs.
- `.github/scripts/upstream-sync.sh check-invariants`
- `git diff --check`

The exact replay passed locally after one CI replay attempt reported an unavailable temporary `tests.log`. That observation is recorded as P3 debt rather than masked with a retry policy.

## Rules for future audits

1. Pin the fork, Original tag/commit, and Plus tag/commit before comparing.
2. Search live callers and tests before classifying a symbol.
3. Use `fork-owned`, `original-owned`, `plus-owned`, and shared-hotspot rules before resolving conflicts.
4. Treat removed-symbol approvals as temporary, evidence-backed records; never use them to hide unexplained loss.
5. Re-run targeted behavior tests, the required server build, tooling validation when policy changes, and a full candidate replay before promotion.
6. Update this document and the ownership manifest whenever a customization is added, adopted upstream, or removed.
