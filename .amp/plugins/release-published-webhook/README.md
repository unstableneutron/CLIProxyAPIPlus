# Release published deployment webhook

This isolated Amp plugin admits GitHub's stable-release publication actions (`release.released` and `release.published`) only after raw-byte HMAC and independent GitHub, Git/tag/main, state-file, receipt, asset/checksum, exact-run Actions artifact, and GHCR manifest provenance checks. Draft and prerelease lifecycle actions remain ineligible. The plugin re-samples the complete mutable GitHub/GHCR snapshot immediately before durably claiming the immutable release and creating exactly one built-in `high` thread on runner `vn3`; the two eligible actions converge on the same immutable claim. A fresh release whose final workflow artifact is not visible yet remains retryable (Actions artifact listings are eventually consistent); a historical missing/expired artifact and malformed, stale, moved, drifted, or superseded releases fail permanently without a state write or thread creation.

Only `.github/workflows/upstream-sync-v2.yml` (upstream) and `.github/workflows/hotfix-release.yml` (hotfix), running from exact `main`, are accepted deployment release workflows. Other sync/tag workflows are not deployment paths.

## Private setup

The repository gitignores `.amp/private/`. Create `.amp/private/release-published-webhook` mode `0700`; files must be `0600`: `github-webhook-secret` (at least 32 bytes), `github-token` (nonempty read token), `notification-thread-id`, and generated `url`. Environment alternatives are `RELEASE_PUBLISHED_WEBHOOK_SECRET`, `RELEASE_PUBLISHED_GITHUB_TOKEN`, and `RELEASE_PUBLISHED_NOTIFICATION_THREAD_ID`. Environment values take precedence. Runtime reads need repository metadata/contents/releases, Actions, and public packages; repository administration and write scopes are not required. The URL is an Amp capability credential. Audit shell history, logs, backups, and permissions for leaked secrets.

From the workspace root, create and verify the file-backed setup without printing values:

```sh
install -d -m 0700 .amp/private/release-published-webhook
umask 077
openssl rand -hex 32 >.amp/private/release-published-webhook/github-webhook-secret
gh auth token >.amp/private/release-published-webhook/github-token
printf '%s' "$NOTIFICATION_THREAD_ID" >.amp/private/release-published-webhook/notification-thread-id
chmod 0600 .amp/private/release-published-webhook/{github-webhook-secret,github-token,notification-thread-id}
test "$(stat -c '%a' .amp/private/release-published-webhook)" = 700
test "$(stat -c '%a' .amp/private/release-published-webhook/github-token)" = 600
test -s .amp/private/release-published-webhook/github-webhook-secret
test -s .amp/private/release-published-webhook/github-token
grep -Eq '^T-[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$' .amp/private/release-published-webhook/notification-thread-id
```

Amp automatically discovers workspace plugins under `.amp/plugins`; there is no `amp plugins load` command. Restart or reopen Amp in this workspace after setup. Verify only the sanitized `release webhook registered` log and that the private URL file exists with mode `0600` (`test -s .../url` and `stat`); never `cat` it.

In **Settings > Webhooks**, use the private URL, `application/json`, the matching secret, **Releases only**, SSL verification on, and Active. Never print the URL, token, secret, payload, or receipt. Re-registering the same plugin webhook key preserves its capability URL. Rotate the HMAC secret by coordinating the private file and GitHub hook update during a maintenance window; the plugin intentionally accepts only one secret. Rotating the capability URL requires a reviewed webhook-key change (or Amp-side revocation), a new GitHub hook, verification, and removal of the old hook.

## Operations

Fresh deliveries with still-missing publish assets or an incomplete Actions run are retried by delivery infrastructure without a claim; historical missing assets fail permanently. Once provenance is complete, an atomic empty claim file named by the SHA-256 of the immutable release key is created and fsynced in the private `claims/` directory (`0700`, files `0600`), then the directory is fsynced, before configuration state or thread creation. Only its filesystem winner may create a thread. Workspace state retains the release and delivery bindings for reporting and append recovery. `claimed` and `creation-uncertain` records without a thread, and claim files left before a state write, intentionally block forever; reconcile them manually by correlating Amp event and GitHub delivery IDs with thread/configuration audit records. A saved thread permits append retry, and full user-message marker inspection recovers an interrupted final state save without creating a second thread. Never delete uncertain state or a claim merely to retry; back up state before an approved repair.

Routine deployment success does not notify the configured source thread. Failures, blockers, approvals, rollbacks, and issues do when the callback thread is available. For negative testing use signed malformed/stale fixtures only—never create a synthetic release, webhook, or deployment thread. Observe sanitized outcome/reason logs and workspace state. Manual rollback follows the deployment evidence: restore the private compose backup and immutable prior digest service-only, then verify the recorded baseline. Unload the plugin before editing state; preserve evidence and obtain operator approval. Source repair, a hotfix PR, and the dedicated hotfix release workflow are separate operator-controlled work; this plugin never patches source or production to make a candidate pass.

The plugin itself appends only a sanitized, marker-deduplicated issue for durable dispatch blockers such as identity rebind, an existing claim, or uncertain thread creation. Invalid-HMAC traffic, routine duplicates/successes, and retryable not-ready timing remain silent. Callback text never includes webhook payload or release title/body.

The deployment thread reads the current production `AGENTS.md` and `SETUP.md` and records every endpoint as an exact URL/path/status rather than applying a generic public-404 rule. CPA loopback `/healthz` succeeds; unsupported CPA `/health`, `/status`, and `/status/` are 404; CPAMP loopback/public routed `/health` has its own success baseline. Any externally routed health 404 found in the production baseline is preserved and attributed to that exact URL. Source validation uses a separate read-only checkout; evidence is private and gitignored, and production remains unchanged during canary.

Run `bun test ./.amp/plugins/release-published-webhook`, a Bun import check, and the repository-required Go build locally. Restart Amp only after private setup is complete. Auto-loading registers the webhook and creates a capability URL; tests and import checks do not load it. For manual rollback, disable the GitHub hook, stop Amp to prevent registration while investigating, preserve private state/evidence, and restore an approved state backup rather than deleting uncertain claims.
