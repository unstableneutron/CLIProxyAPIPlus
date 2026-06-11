---
name: triaging-cliproxy-responses-logs
description: Use when investigating CLIProxyAPI /v1/responses logs, repeated token patterns, websocket timelines, usage.sqlite rows, request IDs, final assistant text, stop/error status, client IPs, or account/model-specific traffic.
---

# Triaging CLIProxy Responses Logs

## Overview

Treat `/v1/responses` triage as request-id forensics. Map usage rows, access logs, and websocket frame logs before interpreting tokens or final output.

## Quick Start

Use the helper first when possible:

```bash
uv run --script .agents/skills/triaging-cliproxy-responses-logs/triage_responses_log.py \
  final --ssh-host vn3 --root ~/CLIProxyAPI \
  --model gpt-5.5-nomoderation --around '2026-06-07 17:47:25' --timezone Asia/Bangkok
```

For repeated bursts:

```bash
uv run --script .agents/skills/triaging-cliproxy-responses-logs/triage_responses_log.py \
  burst --ssh-host vn3 --root ~/CLIProxyAPI \
  --account-prefix 87 --around '2026-06-07 17:48:23' --timezone Asia/Bangkok
```

## Required Workflow

1. **Normalize time first.** `usage.sqlite.timestamp` is UTC (`...Z`). Log frames may include offsets (`+08:00`), and shell/server wall clock may differ. Convert before matching.
2. **Map by request id.** Prefer `cpa-manager/usage.sqlite.request_id` for model/time questions, then open `logs/v1-responses-*-<request_id>.log`. Do not choose by mtime alone.
3. **Join access only after id.** Use `main.log [request_id]` for duration and proxy-visible client IP. `172.18.x.x` is often Docker/proxy, not the true external IP unless forwarded headers are logged.
4. **Parse JSON frames.** Avoid grepping huge websocket frames. Read JSON lines whose `type` is `response.output_text.done`, `response.completed`, `response.failed`, or error-like.
5. **Dedupe mirrored events.** Logs can contain downstream and API/upstream timelines with duplicate output/completion events. Dedupe exact final texts and compare item ids/timestamps.
6. **Interpret prewarm correctly.** `request_kind=prewarm`, `input=[]`, `generate=false`, `prompt_cache_key=<thread>`, and zero output means cache/model warmup, not a user turn.
7. **Report raw status.** Say `status=completed`, `error=<none>`, `incomplete=<none>`, and `stop_field=absent` when no `stop` field exists. Do not invent `stop=true` or `stop=false`.

## Evidence To Report

| Question | Evidence |
|---|---|
| Which request? | request id, usage row timestamp, sanitized log filename |
| What model/account? | requested/resolved model, account prefix only |
| Same or new thread? | session id, thread id, parent id, first/last seen counts |
| What kind of call? | request_kind, thread_source, input count, generate flag |
| Final answer? | last unique `response.output_text.done` text |
| Finished cleanly? | last `response.completed` status/error/incomplete/usage |
| Requester IP? | `main.log` id join, with proxy caveat |

## Common Mistakes

- Matching UI-local timestamps directly against UTC `usage.sqlite` strings.
- Treating a management `/usage-queue` poll at the same second as the target model request.
- Selecting the largest or nearest-mtime log instead of `request_id`.
- Concatenating every `response.output_text.delta` and duplicating mirrored timelines.
- Calling repeated ~11.6k-token prewarms user prompts; those are usually system prompt plus tool schemas.
- Publishing raw bearer tokens, auth file names, full account IDs, upstream host labels, or provider-specific backend URLs.

## Helper Validation

Run the embedded tests after editing the helper:

```bash
uv run --script .agents/skills/triaging-cliproxy-responses-logs/triage_responses_log.py --self-test
```
