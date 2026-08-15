# Upstream Sync v2 candidate webhook

This project-local Amp plugin accepts GitHub `pull_request` webhooks for newly opened or reopened daily Upstream Sync v2 candidate PRs. After fail-closed provenance validation, it starts one fresh built-in `high` agent in a new Orb for the immutable candidate.

## Validation contract

The handler requires all of the following before creating a thread:

- valid `x-hub-signature-256` over the exact request bytes;
- exact GitHub event type plus a well-formed GitHub delivery ID and durable Amp event ID;
- repository ID `1247056725` and exact `unstableneutron/CLIProxyAPIPlus` identity;
- `github-actions[bot]` PR author ID, with only that bot or the repository owner allowed to open/reopen it;
- open, non-draft PR against current `main`, with a same-repository candidate head;
- exact title/body template, source commits, branch, tag, and 40-character plan fingerprint;
- fingerprint recomputation using the repository's Git blob-hash algorithm;
- fresh scheduled `.github/workflows/upstream-sync-v2.yml` run;
- live Original tag, Plus tag/head, models head, and fork main matching the body;
- expected fork tag still absent and no newer open upstream-sync candidate.

PR text is never copied into the agent prompt. The prompt contains only validated scalar facts and tells the new thread to treat PR content as untrusted data.

## Secrets and setup

The owning Amp Orb needs:

1. A random webhook secret of at least 32 characters at `.amp/private/upstream-sync-v2-webhook/github-webhook-secret` with mode `0600`, or `UPSTREAM_SYNC_WEBHOOK_SECRET` in the plugin environment.
2. `UPSTREAM_SYNC_GITHUB_TOKEN` (or the existing `GITHUB_TRIGGER_CI_TOKEN`) with read access to repository metadata, Actions runs, pull requests, commits, and refs.

On load, the plugin writes the capability URL to `.amp/private/upstream-sync-v2-webhook/url` with mode `0600`. Treat both files as credentials. Configure a GitHub repository webhook for that URL with content type `application/json`, the same secret, and only the **Pull requests** event. Never commit, print, or paste the URL or secret into a thread.

The registration is owned by the long-lived source thread/Orb. Reloading this plugin in that thread preserves the URL. If the owning thread is replaced, register the new capability URL in GitHub before retiring the old hook.

## Delivery and duplicate behavior

Amp delivery is at least once. The plugin serializes handlers and stores a workspace-scoped ledger keyed by repository ID, PR number, fingerprint, and head SHA. It claims the candidate before creating an Orb and records the thread before sending work. Duplicate GitHub or Amp deliveries reuse the record and never create another thread. Recovery checks the recorded thread for the stable Amp event marker before appending, so an interrupted final state write does not start a duplicate turn.

If thread creation has an ambiguous failure, the candidate becomes `creation-uncertain` and remains blocked rather than risking a duplicate. If appending the prompt fails after the thread ID is durable, a retry targets that same thread. Operators can inspect `upstreamSyncV2WebhookState` in workspace plugin configuration and resolve blocked records manually.

## Safe testing

Run pure validation and idempotency tests:

```bash
bun test ./.amp/plugins/upstream-sync-v2-webhook/dispatch.test.ts \
  ./.amp/plugins/upstream-sync-v2-webhook/provenance.test.ts
```

Use signed malformed or stale payloads for live negative testing. Do not create, reopen, or merge synthetic production upstream-sync PRs. A real daily candidate is the first allowed positive end-to-end delivery.
