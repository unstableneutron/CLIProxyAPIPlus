---
name: validating-cliproxy-transports
description: Use when validating CLIProxyAPI provider REST, SSE, or websocket transport support; upstream-vs-downstream websocket evidence; streaming behavior; cooldown side effects; or redacted transport reports.
---

# Validating CLIProxyAPI Transports

## Core rule

Do not mark a transport supported from a successful user-visible response alone. Prove the upstream leg used the transport under test, and keep published evidence redacted.

## Required sequence

1. **Redact first:** In notes and reports, use neutral names (`provider-under-test`, `auth-slot`, `model-under-test`). Do not paste raw hostnames, backend labels, auth IDs, account identifiers, tokens, or provider-specific error bodies.
2. **Use one request shape:** Same model, prompt, token budget, and synthetic marker across REST/SSE/websocket. If a low token budget yields reasoning-only or empty text, retry once with a larger budget before declaring failure.
3. **REST before SSE before websocket:** Positive probes first. On auth, quota, permission, or availability failure, stop transport assertions for that auth slot and inspect cooldown/runtime state before retrying.
4. **SSE proof:** Require `text/event-stream`, streamed event frames/deltas, terminal event, and evidence that frames are incremental rather than one buffered response re-emitted downstream.
5. **Websocket proof:** A downstream `101` only proves client-to-proxy websocket. Upstream websocket support requires log/instrumentation evidence for the same marker showing upstream websocket transport, websocket request timeline, handshake `101`, and model events on that path. If upstream is HTTP/SSE, report `downstream-only` or `not proven`.
6. **Cooldown hygiene:** Do not keep probing after 401/403/429-like failures. Wait, restart isolated runtime state, or switch to a clean auth slot. Treat cooldown as environment/auth state, not a transport verdict.

## Evidence checklist

Record only redacted fields:

- Transport: `rest`, `sse`, `websocket-downstream`, `websocket-upstream`
- Result: `supported`, `failed`, `not configured`, `not proven`, `blocked by auth/cooldown`
- Status class, event shape, terminal event, text preview, usage/token presence
- For websocket: downstream transport, upstream transport, handshake status, upstream scheme class (`ws/wss` vs `http/https`), sanitized log filename

## Common mistakes

- Counting downstream websocket as upstream websocket.
- Treating buffered downstream SSE as upstream streaming.
- Letting a failed websocket probe poison auth state, then misclassifying later REST failures.
- Publishing raw logs that include hostnames, auth IDs, provider labels, or tokens.
- Changing prompts/models between transports and blaming transport for model-specific behavior.
