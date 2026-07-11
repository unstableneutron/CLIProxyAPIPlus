# WebSocket Responses Turn Recovery Design

Status: architecture approved; implementation not started

Date: 2026-07-12

Scope: downstream `/v1/responses` WebSocket turns handled by
`sdk/api/handlers/openai/openai_responses_websocket.go`

## Summary

Replace the fork's ordered WebSocket retry chain with a commit-gated,
monotone recovery automaton. The automaton has only three delivery phases.
Request recovery is represented separately as an order-independent set of
constraints.

The handler will prepare one logical turn, invoke a private turn runner, and
pass the runner's accepted stream to the upstream-shaped
`forwardResponsesWebsocket` function. The runner owns pre-downstream-commit
credential failover, startup buffering, failure classification, and portable
request degradation. The existing forwarder continues to own protocol output
restoration, tool-call observation, downstream writes, and completion
collection.

This design preserves the fork's availability requirement while minimizing
the code that must be composed with upstream handler changes.

## Context

Internal provider availability is not reliable enough to treat a failed
provider turn as terminal in every case. A conversation must be able to move
to another credential or route when the original provider is unavailable or
when the replacement provider cannot understand provider-bound continuation
state.

The current fork implements this behavior across request normalization,
checkpoint rollback, route-state probing, forwarding interception, startup
buffering, auth pinning, and session rollback. That implementation works, but
it places most of the availability policy inside an upstream-owned protocol
handler and forwarder. The result is order-sensitive behavior and a large
upstream merge surface.

Upstream commit `aa05fb27f34d9a669db0faf00e9c42ae1bd7a83f`
also makes the forwarding boundary more important: the forwarder collects
`response.output_item.done` events and restores an empty
`response.completed.response.output`. Failed speculative attempts must never
enter that collector.

Two distinct commitment boundaries already exist and must not be conflated:

1. The auth manager's **bootstrap boundary** begins when it observes
   `StreamChunk{Bootstrap: true}` or the first payload. After that boundary,
   the auth manager does not silently rotate credentials for that stream.
2. The turn runner's **downstream commitment boundary** begins when semantic
   or unknown provider output is released to the downstream WebSocket. A
   bootstrapped stream may still be speculative at this layer.

## Goals

1. Preserve a conversation across credential or provider failure whenever a
   portable logical transcript remains available.
2. Keep upstream's handler and forwarding control flow recognizable.
3. Guarantee that a failed speculative attempt cannot leak lifecycle events,
   response IDs, output items, tool calls, or text into the accepted stream.
4. Never transparently restart after meaningful downstream output.
5. Ensure recovery always makes monotone progress and cannot cycle.
6. Let the existing scheduler choose credentials while supplying only narrow,
   turn-local exclusions.
7. Make session updates transactional: only a completed accepted attempt may
   become native continuation state.
8. Keep compaction-specific encrypted-content recovery in the executor.
9. Add no third-party dependencies and no generic state-machine framework.
10. Make the recovery policy exhaustively testable without sockets or auth
    infrastructure.

## Non-goals

- Mid-stream continuation after text, reasoning, or tool output was committed.
- Provider-wide or backend-group exclusion. The first implementation excludes
  exact auth IDs only.
- Replacing scheduler health, cooldown, or credential-ranking policy.
- Generalizing the runner to REST, SSE, or every provider protocol.
- Moving the feature into translators or plugin stream interceptors.
- Probing an unavailable provider to recover missing conversation state.
- Preserving previous-checkpoint rollback as an optimization.
- Adding a post-connect timeout. Existing request cancellation and the
  repository's allowed liveness mechanisms remain authoritative.
- Changing the shared direct-Responses `ResponsesStateMode*` executor API.
  Only the WebSocket handler's speculative route-state cache and probe branch
  are removed.

## Terminology

- **Logical turn:** one downstream `response.create` or `response.append`
  request, including every hidden upstream attempt made before commitment.
- **Attempt:** one rendered payload submitted through the auth manager.
- **Native continuation:** a request that may contain
  `previous_response_id`, provider item IDs, or compatible opaque state.
- **Replay:** a request reconstructed from proxy-owned logical transcript
  state without depending on the latest `previous_response_id`.
- **Startup event:** a narrow allowlist of non-semantic lifecycle events that
  may be buffered before commitment.
- **Semantic event:** output whose release makes transparent replacement
  unsafe, including text, reasoning, tool, or output-item events.
- **Commit:** release of the accepted attempt's first non-bufferable event, or
  forced release after the startup buffer exceeds its bound.
- **Candidate session:** uncommitted state derived from the current logical
  turn.
- **Committed session:** state from the most recent successfully completed
  accepted turn.

## Retry ownership

The feature deliberately keeps existing narrow retry layers instead of
creating one global retry controller.

| Layer | Owns | Does not own |
|---|---|---|
| Codex executor | One same-auth stale persistent-socket reconnect before an application stream is accepted | Credential failover or transcript degradation |
| Auth manager/base stream handler | Credential selection and pre-bootstrap failures within one execution call | Semantic Responses recovery after a stream is exposed |
| WebSocket turn runner | Pre-downstream-commit attempt replacement and portable request degradation | Socket implementation, scheduler ranking, or downstream protocol restoration |
| Existing WebSocket forwarder | Accepted-stream restoration, tool observation, completion collection, and downstream writes | Retry classification or speculative attempts |

The turn runner's no-repeat invariant applies to attempts exposed to the
runner. The executor's single connection repair is not a second logical
attempt because no application request has been accepted by a provider.

## Target architecture

```text
downstream WebSocket request
          |
          v
normalize logical turn and prepare native/replay bases
          |
          v
begin turn coordination once
          |
          v
+----------------------------------------------+
| private Responses WebSocket turn runner      |
|                                              |
| render -> select -> execute -> buffer         |
|              ^                    |           |
|              | recover pre-commit |           |
|              +--------------------+           |
+----------------------------------------------+
          |
          | accepted stream only
          v
upstream-shaped forwardResponsesWebsocket
  - restore response.completed.output
  - record tool-call state
  - write downstream
  - return completed response state
          |
          v
promote or discard candidate session
```

The primary handler seam remains around the existing direct call to
`ExecuteStreamWithAuthManager`. Request normalization stays before the seam.
`forwardResponsesWebsocket` stays after it.

## Components

### Turn input

The handler prepares an immutable input for the runner:

```go
type responsesWebsocketTurnInput struct {
    ModelName            string
    NativePayload        []byte
    ReplayPayload        []byte
    NativeProviderBound  bool
    Compaction           bool
    InitialPinnedAuthID  string
}
```

The implementation uses this private type name and semantic contract unless a
compile-time collision requires a mechanically different unexported name.

- `NativePayload` is the highest-fidelity normalized request.
- `ReplayPayload` is reconstructed from the last committed transcript and the
  new downstream input.
- For CPA-mediated upstream HTTP/SSE, the two bases may already be equivalent.
- For end-to-end upstream WebSocket passthrough, the native base may be an
  incremental request while the replay base contains the portable transcript.
- Both byte slices are immutable after construction.

### Representation constraints

```go
type replayConstraints uint8

const (
    requireReplay replayConstraints = 1 << iota
    omitEncryptedContent
    omitProviderIdentifiers
)
```

`omitEncryptedContent` and `omitProviderIdentifiers` imply `requireReplay`.
Normalization enforces that invariant.

```text
                         {R,E}
                        /     \
{} Native -> {R} Replay         {R,E,I} Portable
                        \     /
                         {R,I}
```

Constraints may only be added. They are never removed during a logical turn.

The existing `responsesreplay` package becomes a pure renderer and classifier:

```go
Render(source, constraints) -> payload, changed
Classify(status, message) -> failureKind
Advance(constraints, failureKind, compact) -> next, changed
```

Rendering order is fixed:

1. Select native or replay base.
2. Remove provider identifiers when requested.
3. Remove non-portable encrypted fields when requested.
4. Preserve compaction and compaction-summary encrypted content.
5. Validate the resulting JSON.

Rendered bytes and their SHA-256 digest are cached per constraint set for the
duration of the logical turn. There are at most five reachable effective
representations.

### Delivery phase

```go
type deliveryPhase uint8

const (
    phaseSpeculative deliveryPhase = iota
    phaseCommitted
    phaseTerminal
)
```

Only this dimension is a lifecycle state machine.

```text
SPECULATIVE --semantic/unknown/overflow--> COMMITTED --> TERMINAL
     |
     +--recoverable failure--> another speculative attempt
```

### Attempt memory

Attempt identity is:

```text
(selected auth ID, SHA-256(rendered payload))
```

The runner stores:

```go
hardExcludedAuths       set[authID]
attemptedByPayload      map[payloadDigest]set[authID]
```

Before each execution call, the scheduler exclusion set is:

```text
hardExcludedAuths union attemptedByPayload[current digest]
```

The digest is stored only in memory. Request bytes or user content are never
included in transition logs.

The scheduler remains the sole authority for choosing among eligible auths.
The runner never asks for a specific replacement credential or backend.

### Turn stream

The runner returns a synthetic stream compatible with the current forwarder:

```go
type responsesWebsocketTurnStream struct {
    Data    <-chan []byte
    Errors  <-chan *interfaces.ErrorMessage
    outcome <-chan responsesWebsocketTurnOutcome
}
```

One goroutine owns the runner's mutable state and every attempt lifecycle. The
output channels provide backpressure after commitment. The outcome channel is
buffered by one and is completed exactly once before the output channels are
closed.

The accepted outcome contains only orchestration metadata needed after
forwarding:

- Selected auth ID, when known.
- Final representation constraints.
- Whether downstream commitment occurred.
- Whether the stream reached a completion event.
- Terminal failure kind.

It does not duplicate completed response output; the upstream forwarder
remains the source of truth for that data.

## Failure taxonomy

```go
type responsesWebsocketFailureKind uint8

const (
    failureNone responsesWebsocketFailureKind = iota
    failurePreviousResponseMissing
    failureProviderItemMissing
    failureInvalidEncryptedContent
    failureAuthOrRoute
    failureRequest
    failureProtocol
    failureCanceled
    failureCompactionTakeover
)
```

Classification rules are narrow and fail closed.

Classification precedence is fixed:

1. Caller cancellation or compaction takeover.
2. Structured state-portability codes and parameters.
3. Narrow state-portability message fallbacks.
4. Auth/route statuses and transport closure markers.
5. Deterministic request errors.
6. Protocol or unclassified terminal errors.

This ordering ensures, for example, that a structured
`invalid_encrypted_content` HTTP 400 is a state transition, while an unrelated
HTTP 400 is terminal, and a structured `previous_response_not_found` HTTP 404
is not mistaken for a generic route failure.

### Previous response missing

Recognize structured `previous_response_not_found`, a
`previous_response_id` error parameter, or the existing narrowly matched
message forms.

- Native request: add `requireReplay`.
- Already replaying: add `omitProviderIdentifiers`.
- Already replaying without provider IDs: no progress; finalize the failure.

### Provider item missing

Recognize structured `item_not_found`, an `input.*.id` or `output.*.id` error
parameter, or the existing narrowly matched provider-item messages.

- Add `requireReplay | omitProviderIdentifiers`.
- If the constraint set does not change, finalize the failure.

### Invalid encrypted content

Recognize structured `invalid_encrypted_content` and the existing narrow
verification/prefix messages.

- Normal response turn: add `requireReplay | omitEncryptedContent`.
- Compaction turn: do not generically degrade; preserve executor ownership.
- If the constraint set does not change, finalize the failure.

### Auth or route failure

This category includes credential rejection, quota/rate-limit exhaustion,
provider unavailability, qualifying gateway failures, and EOF before any
semantic downstream commitment.

- Hard-exclude every auth selected and rejected internally before the active
  stream, when reported by the selected-auth callback.
- Hard-exclude the active selected auth.
- If the current native request contains provider-bound continuation state,
  add `requireReplay` before selecting another auth.
- Do not exclude an entire provider or backend group.

### Request failure

Malformed requests, unsupported schemas, policy rejections, and other
deterministic client-visible errors are terminal. They do not change
credential or representation.

### Protocol failure

Malformed upstream JSON, unrecognized internal framing, or an invariant
violation is terminal. Unknown valid Responses events are not protocol
failures; they force downstream commitment and are forwarded.

An event whose type is `error` but whose contents do not match a recoverable
state or auth/route failure is a terminal error event. It is never treated as
an ordinary unknown event.

## Event processing

Each payload in a chunk is processed in order. A chunk containing several JSON
payloads may cross the commitment boundary in the middle of the chunk.

The only bufferable event types are:

- `codex.rate_limits`
- `response.created`
- `response.in_progress`

All other valid event types are commit events unless they are a known
pre-commit error selected for recovery.

The startup buffer has a strict one-mebibyte byte limit. An event that would
make the buffer larger than the limit causes the existing buffer and that
event to be released in order, and permanently commits the attempt. Exactly
one mebibyte remains bufferable.

### Speculative transition table

| Input event | Condition | Action |
|---|---|---|
| Bufferable startup payload | Total remains at or below 1 MiB | Append to startup buffer |
| Bufferable startup payload | Total would exceed 1 MiB | Flush in order, relay payload, enter committed phase |
| Known recoverable error | Policy and rendered payload make progress | Cancel attempt, discard its buffer/error, advance constraints or exclusions, retry |
| Known recoverable error | No new constraints, auth, or payload pair remains | Flush final attempt buffer, forward final error, enter terminal phase |
| Deterministic request error | Any | Flush final attempt buffer, forward error, enter terminal phase |
| Unclassified `error` event | Any | Flush final attempt buffer, forward error in its original form, enter terminal phase |
| Unknown or semantic event | Any | Flush buffer, relay event, enter committed phase |
| Completion event | Any | Flush buffer, relay completion, enter terminal phase |
| Data stream closes before completion | Replacement candidate exists | Treat as auth/route failure and retry |
| Data stream closes before completion | No replacement candidate exists | Flush buffer, synthesize current close-before-completion error, enter terminal phase |
| Client cancellation | Any | Cancel attempt, discard speculative buffer, enter terminal phase |
| Compaction takeover | Any | Cancel attempt, discard speculative buffer, enter terminal phase |

### Committed transition table

| Input event | Action |
|---|---|
| Normal payload | Relay unchanged |
| Completion event | Relay and enter terminal phase |
| Error event or error channel | Relay final error and enter terminal phase; never retry |
| EOF before completion | Preserve already emitted output, report close-before-completion, enter terminal phase |
| Client cancellation or compaction takeover | Cancel and enter terminal phase; never retry |

The runner does not interpret or collect accepted output items. It only decides
whether payloads are hidden or released.

## Auth selection and exclusions

An attempt always installs the selected-auth callback, even when the first
attempt carries a pinned auth ID. Callback composition must preserve the
existing selected-upstream recording in `GetContextWithCancel`.

The callback records every selected auth for that execution call:

- Auths selected before a returned stream represent internal bootstrap or
  preparation failures and become hard exclusions for this logical turn.
- The final selected auth is the active stream auth.
- A state-portability error on the active stream does not hard-exclude that
  auth; it may retry a changed payload.
- An auth/route failure hard-excludes the active auth.

Because callback invocation may come from execution internals, the
attempt-local trace uses a small mutex. The runner snapshots the trace before
making a transition. No other runner state is shared across goroutines.

### Scheduler metadata

Add one execution metadata key for excluded auth IDs and one handler context
helper that copies a normalized set into request metadata. The auth conductor
seeds the local `tried` map in `executeStreamMixedOnce` from this metadata.

Requirements:

- Empty and duplicate IDs are ignored.
- The metadata value is defensively copied.
- Exclusions apply to legacy, Home, and plugin scheduler selection through the
  shared `tried` argument.
- Exclusions do not mark credentials globally unhealthy and do not mutate
  scheduler cooldown state.
- Existing max-retry-credentials accounting counts only credentials attempted
  by the current manager call, not pre-seeded exclusions.

### Pinning

A committed session's auth ID is a first-attempt preference.

- Success retains or replaces the pin with the accepted auth when native
  continuation is supported.
- A state failure may reuse the same pin with a changed payload.
- An auth/route failure clears and excludes the pin.
- If a pin cannot be found before any auth is selected, clear it. Add replay
  when the native request is provider-bound, then run normal scheduling.
- A pinned auth that is also excluded is never sent to the scheduler as a
  contradictory instruction; the runner clears the pin first.

### Missing auth identity

Plugin executors may not report an auth ID. Such an attempt uses one synthetic
turn-local identity.

- A changed payload may retry after a state failure.
- The same payload cannot retry after a health failure.
- The synthetic identity is never sent to the scheduler as an auth ID.

## Retry bounds

Correctness does not depend on an incrementing retry counter.

The loop is finite because:

1. Constraints only move upward through a finite lattice.
2. A given `(auth ID, payload digest)` pair is attempted at most once by the
   runner.
3. The scheduler has a finite eligible credential set.
4. A renderer transition that produces identical bytes is a fixed point and
   cannot retry.

One caller context spans the entire logical turn. It is never reset between
attempts. No new per-attempt network deadline is introduced.

## Session transaction

The handler keeps one committed session snapshot and one candidate for the
current logical turn.

```text
last committed session
          |
          v
 build candidate from new downstream input
          |
      accepted stream
       /           \
completed       failed/canceled
   |                 |
promote            discard
```

The committed snapshot contains the existing logical fields:

- Last normalized logical request.
- Last completed response output.
- Last completed response ID.
- Pending tool-call IDs.
- Pinned auth ID and passthrough model information needed for native
  continuation.

Rules:

1. Request normalization never mutates the committed snapshot.
2. Failed speculative attempts never mutate tool caches, completion output,
   response ID, or auth pinning.
3. A completion recognized by the existing
   `isResponsesWebsocketCompletionEvent` helper promotes the candidate. This
   currently includes `response.completed` and `response.done`.
4. A pre-commit terminal failure discards the candidate and preserves the
   previous committed snapshot.
5. A failure after commitment discards the candidate and clears native
   continuation pinning. It does not invent partial assistant history.
6. The canonical replay snapshot is derived from the replay base under the
   accepted attempt's final constraints, not from an incremental native
   payload. Fields deliberately removed during recovery are not silently
   reintroduced on the next replay.
7. Native response IDs and auth pins are optional accelerators layered over
   that canonical replay snapshot; they are never the only continuity source.
8. Tool events released after commitment may update the existing tool cache
   because the downstream client observed them, even if the stream later
   fails. Only speculative attempts are forbidden from updating the cache.
9. A locally handled prewarm retains its current explicit session behavior and
   bypasses the runner.

This removes optimistic `lastRequest` mutation followed by snapshots and
rollback.

## Turn coordination and cancellation

`beginResponsesMainTurn` is called once per logical downstream turn, outside
the attempt loop. Every attempt receives a child context of that coordinated
turn context.

Before retrying, the runner cancels the failed attempt. The existing base
stream adapter selects on context cancellation when sending data or errors, so
an abandoned speculative stream cannot remain blocked on an unconsumed output
channel.

If compaction takes over the turn:

- The coordinated root context is canceled.
- The active attempt stops.
- The startup buffer is discarded.
- No replacement attempt starts.
- The outcome records compaction takeover rather than provider failure.

The runner has one state-owning goroutine per active logical turn, not one
goroutine per retry.

## Forwarder boundary

`forwardResponsesWebsocket` must receive only the accepted stream.

It continues to own:

- Splitting chunks into individual JSON payloads.
- Collecting `response.output_item.done` by `output_index`.
- Restoring an empty `response.completed.response.output`.
- Recording accepted tool calls and pending tool-call IDs.
- Writing payloads and errors to the downstream WebSocket.
- Returning completed output, response ID, pending tool-call IDs, and the
  downstream-visible error.

The runner must not duplicate these functions. This ensures that upstream
changes like `aa05fb27` can be accepted primarily in the upstream-owned
forwarder instead of reimplemented in fork code.

## Deliberate removals

The migration removes these WebSocket-handler mechanisms:

1. Previous-checkpoint rollback and `responsesWebsocketCheckpoint` state.
2. The per-auth/model speculative Responses-state support cache.
3. Dynamic state probing and retry decisions based on that cache.
4. `forceTranscriptReplayNextRequest`; replay eligibility is derived from the
   committed snapshot and current pin.
5. Retry interception options and retry results inside the forwarder.
6. Handler-level upstream-disconnect subscriptions and their one-shot close
   latch.
7. Snapshot-and-rollback mutations of `lastRequest` and response state.

Shared direct-Responses state-mode metadata and executor behavior remain until
a separate design proves they are unused outside this WebSocket flow.
Executor-side disconnect observation may also remain for executor-owned
connection management; only the downstream handler subscription is removed.

## Upstream disconnect behavior

The downstream WebSocket is no longer closed merely because an idle persistent
upstream socket disconnects.

- During an active attempt, the executor's read error reaches the runner and
  is handled according to the commitment phase.
- Between turns, a dead persistent upstream connection is repaired lazily by
  the executor on the next request.
- The downstream session remains usable while no upstream attempt is active.

This removes the current one-shot disconnect notification race and aligns the
observable connection lifetime with downstream rather than upstream transport
lifetime.

## Observability

Add one structured debug log for every transition and one info/warn summary at
logical-turn completion. Reuse existing request/session IDs and auth-selection
logging.

Transition fields:

- Session or turn correlation ID.
- Attempt ordinal for diagnostics only.
- Representation constraint string.
- Failure kind.
- Delivery phase.
- Buffered event count and bytes.
- Selected auth ID only under the repository's existing auth-ID logging
  policy.
- Action: `retry`, `commit`, `complete`, `final_error`, or `cancel`.

Completion fields:

- Total attempt count.
- Whether failover occurred.
- Final constraint set.
- Final selected auth when permitted.
- Final outcome.

Do not log request payloads, encrypted content, credentials, or transcript
content. Failed speculative payloads are not appended as downstream timeline
events.

No metrics framework is introduced as part of this refactor. Structured logs
provide the first measurement surface; metrics can be added later if existing
operational tooling demonstrates a concrete need.

## Edge-case decisions

| Edge case | Required behavior |
|---|---|
| Startup frames followed by a recoverable error | Discard every startup frame and the error before retry |
| Semantic event and error in the same chunk | Process in order; semantic event commits, so the later error cannot retry |
| Two recovery errors in either order | Reach the same union of constraints |
| Constraint changes but rendered JSON does not | Stop at the digest fixed point |
| Scheduler returns a previously attempted auth for the same digest | Reject the repeated pair without executing it again |
| Only one auth and a state failure | Retry only when rendered bytes change |
| Only one auth and a health failure | Do not retry at runner layer |
| Stale pin points to missing auth | Clear pin; replay if provider-bound; schedule normally |
| Oversized startup event | Flush prior buffer and event, commit permanently |
| Silent upstream after connection | Wait for caller cancellation or existing allowed liveness mechanism |
| Accepted stream closes before completion | Never promote candidate; clear native continuation after downstream commitment |
| Unknown valid Responses event | Commit and forward, preserving upstream compatibility |
| Malformed upstream JSON | Terminal protocol failure, never speculative retry |
| Failed attempt emitted response/output IDs | IDs never enter forwarder, tool cache, or committed session |
| Plugin executor has no auth callback | Use synthetic identity and allow changed-payload state recovery only |
| Generic invalid encrypted error on compaction | Do not strip required compact content in runner |
| Downstream disconnect during retry | Cancel active attempt and terminate without another selection |
| Compaction starts during retry | Cancel the whole logical turn and discard speculative output |

## File boundaries

Expected production changes:

- `sdk/api/handlers/openai/openai_responses_websocket.go`
  - Prepare immutable turn input.
  - Begin coordination once.
  - Invoke the runner at the execution seam.
  - Promote or discard candidate session state.
  - Remove superseded handler-local machinery.
- `sdk/api/handlers/openai/openai_responses_websocket_turn_runner.go`
  - Private runner, startup buffer, attempt trace, and stream adapter.
- `sdk/api/handlers/openai/responsesreplay/planner.go`
  - Pure constraint renderer, classifier, and transition policy.
- `sdk/api/handlers/handlers.go`
  - Context-to-execution-metadata plumbing for excluded auth IDs.
- `sdk/cliproxy/executor/types.go`
  - One excluded-auth metadata key.
- `sdk/cliproxy/auth/conductor.go`
  - Seed stream selection's local `tried` set from exclusions.

Expected test changes remain adjacent to these packages. No changes are
planned under `internal/translator/`.

Production abstraction budget:

- One runner type.
- One constraint value type.
- One startup buffer type, private to the runner.

If implementation requires more than these three concepts or spreads the
behavior across more than the listed production areas, stop and revisit the
design.

## Characterization and test matrix

Tests are added before deleting legacy behavior.

### Pure policy tests

1. Every failure kind advances from every reachable constraint set exactly as
   specified.
2. Constraints are monotone: `next | current == next`.
3. Invalid-encrypted then item-missing equals item-missing then
   invalid-encrypted.
4. Reapplying the same failure reaches a fixed point.
5. Native, replay, no-encrypted, no-ID, and portable rendering preserve valid
   JSON.
6. Provider identifiers are removed only from defined fields.
7. Compaction-like encrypted content survives every generic representation.
8. Renderer input slices are never mutated.
9. Different constraint sets that render equal bytes produce the same digest.
10. Fuzz rendering with valid JSON to assert validity, immutability, and no
    reintroduction of removed fields.
11. Structured state codes take precedence over generic HTTP status
    classification.
12. Unclassified `error` events are terminal and never treated as ordinary
    unknown events.

### Runner tests

1. Provider A fails before payload; provider B completes.
2. Provider A emits only lifecycle events then fails; no A event leaks.
3. Provider A emits text, reasoning, tool arguments, or output item then
   fails; no transparent retry occurs.
4. Previous-response missing moves native to replay.
5. Previous-response missing during replay removes provider IDs.
6. Provider-item missing moves directly to replay without IDs.
7. Invalid encrypted content moves to replay without encrypted content.
8. Sequential state errors converge to portable replay in both orders.
9. Same auth may process a changed state-degraded payload.
10. Same auth cannot process the same payload digest twice.
11. Health failure excludes the active auth across every later
    representation.
12. Earlier auths rejected within one manager call become hard exclusions.
13. A stale pin is cleared before normal scheduling.
14. An absent auth callback uses the synthetic identity policy.
15. Exactly 1 MiB of startup payload remains speculative.
16. Exceeding 1 MiB commits and forbids later retry.
17. Unknown event commits immediately.
18. Multi-payload chunks preserve ordering at the commit boundary.
19. EOF pre-commit may fail over; EOF post-commit may not.
20. Client cancellation stops the active attempt and prevents reselection.
21. Compaction takeover cancels the logical turn across retries.
22. Outcome is published exactly once on every terminal path.
23. Backpressure after commitment does not cause unbounded buffering.

### Session transaction tests

1. Failed speculative attempts leave the committed snapshot byte-for-byte
   unchanged.
2. Only a completed accepted attempt updates response output and ID.
3. A post-commit failure clears native pinning but preserves the last completed
   transcript.
4. Failed attempt tool calls never enter the shared tool cache.
5. A committed partial tool call may remain in the tool cache because it was
   visible downstream, while the native response checkpoint remains unset.
6. A degraded accepted replay becomes the next canonical replay base and does
   not reintroduce removed provider-bound fields.
7. Both `response.completed` and `response.done` follow the existing
   completion helper's promotion semantics.
8. A locally handled prewarm retains existing behavior and does not invoke the
   runner.

### Scheduler tests

1. Excluded IDs seed `tried` before the first pick.
2. Empty and duplicate exclusions are ignored.
3. Exclusions work with legacy, Home, and plugin scheduler paths.
4. Exclusions do not consume `maxRetryCredentials` accounting.
5. Pinned-plus-excluded input is normalized before selection.
6. No global auth health state is changed solely by turn-local exclusion.

### Upstream integration tests

1. `response.output_item.done` events from the accepted attempt restore an
   empty `response.completed.response.output` in index order.
2. Output items from failed attempts never appear in restored output.
3. Existing continuation behavior from `aa05fb27` remains intact.
4. Direct downstream-WebSocket to upstream-HTTP/SSE execution uses the same
   commit policy.
5. End-to-end upstream-WebSocket passthrough retains pinning and native
   continuation on success.
6. The handler does not close an idle downstream WebSocket after an upstream
   persistent-socket disconnect.

### Validation gates

After restoring the current branch to a green baseline:

1. Focused policy, runner, handler, auth-conductor, and executor tests.
2. `go test -race` for the modified handler/auth packages.
3. `go test ./...`.
4. Required compile command from `AGENTS.md`.
5. Repository invariant and symbol-survival gates.
6. Upstream-sync `replay-plan` to verify the new handler seam remains
   composable with current original and Plus refs.

The current resolution branch has an unrelated compile blocker in
`internal/registry/model_updater.go` because `oldData.CommandCode` is missing.
That baseline must be repaired before implementation behavior can be
characterized or completion can be claimed.

## Performance and resource bounds

- Startup memory is capped at 1 MiB plus slice overhead per logical turn.
- Rendered representations are cached for the turn to avoid repeated JSON
  scans.
- SHA-256 adds one linear pass per distinct rendered payload.
- One state-owning goroutine exists per active logical turn.
- After commitment, synthetic output channels remain bounded and apply
  backpressure rather than accumulating response data.
- No reflection, generic FSM library, provider probe, or extra network request
  is introduced.

## Migration sequence

Every implementation commit must compile and keep existing behavior available
until the integration switch.

1. **Restore green baseline.** Resolve the unrelated `CommandCode` overlay gap
   and run the existing focused WebSocket tests.
2. **Characterize invariants.** Add or tighten tests for startup buffering,
   retry suppression after semantic output, output restoration, pin release,
   compaction cancellation, and current failover behavior.
3. **Add exclusion plumbing.** Introduce the metadata key, handler helper,
   conductor parsing, and scheduler tests. No caller uses it yet.
4. **Evolve the pure planner.** Add constraints, split failure kinds, immutable
   rendering, digesting, and exhaustive tests. Preserve temporary adapters for
   existing planner callers until the runner lands.
5. **Add and directly test the runner.** Keep it private and unreferenced by
   production handler flow until its policy and stream tests pass.
6. **Switch the handler seam.** Route one logical turn through the runner while
   retaining the existing forwarder and completion collector.
7. **Make session promotion transactional.** Remove optimistic mutations and
   derive native continuation from the committed snapshot.
8. **Delete superseded machinery.** Remove checkpoint rollback, handler-local
   route probing/cache, force-replay state, forwarding interception, and the
   upstream-disconnect subscriber.
9. **Run full validation.** Include race, build, full tests, invariant gates,
   symbol-survival checks, and upstream `replay-plan`.

No runtime feature flag is planned. The old and new implementations coexist
only across development commits, not as two long-term production paths.

## Rollback

The change introduces no persistent schema, configuration migration, or
external API change.

Rollback is a source revert in reverse integration order:

1. Revert the handler seam and transactional session promotion.
2. Restore the legacy retry chain if operational rollback is required.
3. Leave inert exclusion metadata parsing in place or revert it separately.

Because session state is in memory, restarting the process clears any state
created by the new runner. No data migration is required.

## Acceptance criteria

The refactor is complete only when all of the following are true:

1. The handler calls one private turn runner between normalization and the
   upstream forwarder.
2. `forwardResponsesWebsocket` receives only accepted-attempt events and
   retains upstream output restoration behavior.
3. No exact `(auth ID, rendered payload digest)` pair repeats at runner level.
4. State-degradation order is commutative and monotone.
5. A health failure excludes the exact auth for the logical turn.
6. A state failure may reuse an auth only with changed rendered bytes.
7. No semantic downstream output is ever followed by transparent failover.
8. Session state is promoted only after an accepted completion event.
9. Previous-checkpoint rollback, WebSocket route-state probing/cache,
   force-replay state, retry-aware forwarding, and handler disconnect closure
   are absent.
10. Compaction-specific encrypted-content behavior remains executor-owned.
11. No translator changes or new dependencies are introduced.
12. Focused, race, full-build, full-test, invariant, symbol-survival, and
    upstream replay-plan gates pass from a green baseline.

## Not in scope

- Provider/backend-group health identities and exclusions: defer until the
  scheduler exposes a trustworthy grouping contract.
- Mid-response continuation: unsafe without a client-visible continuation
  protocol.
- Metrics backend integration: structured logs first; add metrics only from an
  observed operational need.
- General state-machine framework: it would increase the long-term fork
  surface without improving this bounded flow.
- Direct Responses HTTP/SSE retry redesign: this specification changes only
  the downstream WebSocket orchestration boundary.

## Resolved decisions

- Use exact auth exclusions, not provider-wide exclusions.
- Allow the same auth to retry only when state degradation changes rendered
  bytes.
- Use a one-mebibyte startup buffer with no post-connect timer.
- Treat unknown valid events as commitment.
- Keep scheduler selection policy outside the runner.
- Keep output restoration in the upstream forwarder.
- Keep compaction encrypted-content fallback in the executor.
- Remove previous-checkpoint rollback and handler-local route probing.
- Keep one turn-level context and one state-owning goroutine.
- Bound retries structurally rather than by a semantic retry counter.

There are no unresolved architectural decisions in this specification.
