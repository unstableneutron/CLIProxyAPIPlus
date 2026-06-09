# Request Fingerprint Probe

`request-fingerprint-probe` is a diagnostic tool for comparing outbound request
shape across the local Go stdlib client, CLIProxyAPIPlus' ChatGPT uTLS path, and
manually driven Codex.app / Codex CLI traffic.

It is intentionally not part of the proxy server binary. Keep generated captures
under `scratch/request-fingerprint-probe/`; captured files can include request
shape metadata and should not be committed.

## What It Captures

- TLS ClientHello fields observed by a local HTTP `CONNECT` proxy:
  - JA3 string and hash
  - JA3N-style normalized hash with extension order sorted
  - raw ClientHello SHA-256
  - cipher suites
  - extension order
  - supported groups
  - ALPN protocols
  - SNI
- Plain HTTP request shape observed by a local sink:
  - request line
  - header names, order, casing, and redacted values
  - body byte length
  - body SHA-256

Sensitive header values are redacted. This includes authorization, cookies,
account IDs, API keys, tokens, management keys, websocket keys, and volatile
Codex request/session/thread/window metadata.

## What It Cannot Prove

It cannot prove that two full HTTPS requests are byte-for-byte identical.

TLS handshakes include volatile fields such as random bytes, session IDs, key
shares, tickets, and certificate state. HTTP/2 also adds HPACK compression,
stream IDs, frames, flow-control updates, and implementation-specific ordering.

Use this tool for stable drift signals:

- normalized TLS fingerprint drift (`ja3n_hash`)
- ALPN/protocol drift
- HTTP/2 capability drift inferred from ALPN and ClientHello
- semantic header/body drift on the HTTP sink path

Raw `ja3_hash` and `raw_sha256` are still recorded, but Chrome-style clients can
randomize extension order and other handshake details. Treat raw JA3 changes as
diagnostic unless the normalized fields also move.

Use a real MITM setup only when you specifically need decrypted HTTPS headers on
the true upstream path.

## Build

```bash
go build -o scratch/request-fingerprint-probe/request-fingerprint-probe ./tools/request-fingerprint-probe
```

## Self-Contained Probe

This starts a temporary local `CONNECT` proxy and captures two local clients
connecting to `https://chatgpt.com/backend-api/codex/models?...`:

- `go-stdlib`
- `cliproxy-utls`, using `helps.NewUtlsHTTPClient`

```bash
go run ./tools/request-fingerprint-probe probe
```

Artifacts are written to:

```text
scratch/request-fingerprint-probe/runs/<timestamp>/
  summary.json
  compare.md
  go-stdlib.json
  cliproxy-utls.json
```

The upstream request is expected to fail after the ClientHello is captured,
because the probe proxy closes the tunnel after recording the handshake.

## Manual Codex.app / Codex CLI Capture

Start a long-running local capture server:

```bash
go run ./tools/request-fingerprint-probe serve
```

The server prints:

```text
HTTPS_PROXY=http://127.0.0.1:<port>
HTTP sink base URL: http://127.0.0.1:<port>
```

For a TLS fingerprint capture, run the bundled Codex CLI or app-server with the
printed `HTTPS_PROXY` value. The proxy captures the ClientHello before any
application request bytes are sent.

For a header/body-shape capture, point Codex at the printed HTTP sink base URL
with a temporary per-run config override, for example:

```bash
/Applications/Codex.app/Contents/Resources/codex exec \
  --ignore-user-config \
  -c 'openai_base_url="http://127.0.0.1:<port>/v1"' \
  -m gpt-5.5 \
  --skip-git-repo-check \
  'Reply exactly: fingerprint probe'
```

Prefer temporary overrides or a temporary `CODEX_HOME` over editing
`~/.codex/config.toml`.

## Compare Captures

```bash
go run ./tools/request-fingerprint-probe compare \
  --left scratch/request-fingerprint-probe/runs/<run>/go-stdlib.json \
  --right scratch/request-fingerprint-probe/runs/<run>/cliproxy-utls.json
```

`compare` reports TLS fingerprint drift plus HTTP method, target, body hash,
header name, header order, and header value drift.

For the self-contained `probe` command, the main diff is already in
`compare.md`.

## Suggest Tweaks

```bash
go run ./tools/request-fingerprint-probe suggest \
  --reference scratch/request-fingerprint-probe/runs/codex-cli/connect-<id>.json \
  --candidate scratch/request-fingerprint-probe/runs/<run>/cliproxy-utls.json
```

`suggest` maps known drift signals to likely CLIProxy knobs or code paths. It is
not an auto-fixer. It currently points TLS drift at
`internal/runtime/executor/helps/utls_client.go`, Codex header drift at
`applyCodexHeaders` / `applyCodexWebsocketHeaders`, and passthrough-body drift at
the backend passthrough stream path.

## Current Codex CLI Baseline

On the local bundled Codex CLI (`0.137.0-alpha.4`), a sink run with:

```bash
/Applications/Codex.app/Contents/Resources/codex exec \
  --ignore-user-config \
  -c 'openai_base_url="http://127.0.0.1:<port>/v1"' \
  -c 'model_reasoning_effort="low"' \
  -m gpt-5.5 \
  --skip-git-repo-check \
  --sandbox read-only \
  'Reply exactly: fingerprint probe'
```

observed these OpenAI-compatible request shapes:

- `GET /v1/models?client_version=0.137.0`
- `GET /v1/responses` websocket upgrade attempts with
  `openai-beta: responses_websockets=2026-02-06`
- `POST /v1/responses` HTTP fallback attempts

A `CONNECT` run against `https://chatgpt.com/backend-api/codex` also observed
ChatGPT-native backend attempts such as plugin sync, model refresh, WHAM apps,
Codex responses, and analytics. The normal reqwest HTTPS path advertised
`h2,http/1.1`; the websocket path used a separate ClientHello with no ALPN in
the captured baseline.

Use these captures as references, not as universal constants. Codex versions,
host OS TLS libraries, and app-server protocol behavior can change.

## App-Server Capture Notes

Use a controlled app-server instance instead of the live Codex.app daemon when
capturing app-server request shape:

```bash
CODEX_HOME="$tmp_home" \
HTTPS_PROXY="http://127.0.0.1:<port>" \
/Applications/Codex.app/Contents/Resources/codex app-server \
  --listen "unix://$tmp_home/app-server.sock"
```

Starting `app-server` alone is not enough to trigger model/backend requests. A
protocol client must create the session or task that drives outbound traffic.
The `codex app-server proxy` command proxies raw app-server transport bytes, so
keep protocol-harness work separate from the capture server itself.
