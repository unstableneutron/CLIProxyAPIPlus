---
name: cliproxy-codex-fingerprint
description: Use when working in CLIProxyAPIPlus on Codex/ChatGPT TLS fingerprinting, uTLS profile updates, request/header capture, Codex version bumps, or local smoke validation for ChatGPT backend passthrough and Codex Responses/WebSocket traffic.
---

# CLIProxy Codex Fingerprint

Use this skill from the CLIProxyAPIPlus repository root when updating or validating Codex TLS/header behavior. Keep artifacts under `scratch/request-fingerprint-probe/` and avoid printing tokens, cookies, or full auth files.

## Version Bump Workflow

1. Capture the new Codex CLI/app baseline.

   Start the raw capture proxy:

   ```bash
   go run ./tools/request-fingerprint-probe serve \
     --out scratch/request-fingerprint-probe/runs/codex-<version>-baseline
   ```

   In another shell, record binary provenance and run the target Codex binary with the capture proxy from the `serve` output:

   ```bash
   RUN=scratch/request-fingerprint-probe/runs/codex-<version>-baseline
   CODEX_BIN=/path/to/codex
   realpath "$CODEX_BIN" | tee "$RUN/codex-bin-path.txt"
   "$CODEX_BIN" --version | tee "$RUN/codex-version.txt"
   shasum -a 256 "$CODEX_BIN" | tee "$RUN/codex-bin.sha256"

   HTTPS_PROXY=http://127.0.0.1:<port> \
   HTTP_PROXY=http://127.0.0.1:<port> \
   ALL_PROXY=http://127.0.0.1:<port> \
   "$CODEX_BIN" exec "Reply with exactly: ok" || true
   ```

   Let the CLI fail after ClientHello capture if the proxy closes the connection. If no empty-ALPN `chatgpt.com` capture appears, the run did not exercise websocket TLS; try a Codex model/config path that prefers websockets before updating the WS profile.

2. Pick representative captures.

   ```bash
   HTTPS_REF=$(jq -r 'select(.mode=="connect" and .tls.server_name=="chatgpt.com" and ((.tls.alpn_protocols // []) | index("h2"))) | input_filename' scratch/request-fingerprint-probe/runs/codex-<version>-baseline/connect-*.json | head -1)
   WS_REF=$(jq -r 'select(.mode=="connect" and .tls.server_name=="chatgpt.com" and (((.tls.alpn_protocols // []) | length) == 0)) | input_filename' scratch/request-fingerprint-probe/runs/codex-<version>-baseline/connect-*.json | head -1)
   ```

   Treat HTTPS as the `h2,http/1.1` capture and websocket as the empty-ALPN capture.

3. Generate reusable profile artifacts.

   ```bash
   go run ./tools/request-fingerprint-probe generate-profile \
     --reference "$HTTPS_REF" \
     --name codex-rustls-macos-arm64-<version>-https \
     --out scratch/request-fingerprint-probe/runs/codex-<version>-baseline/codex-https-profile.json

   go run ./tools/request-fingerprint-probe generate-profile \
     --reference "$WS_REF" \
     --name codex-rustls-macos-arm64-<version>-ws \
     --out scratch/request-fingerprint-probe/runs/codex-<version>-baseline/codex-ws-profile.json
   ```

4. Validate CLIProxy’s emitted fingerprints.

   ```bash
   go run ./tools/request-fingerprint-probe probe \
     --out scratch/request-fingerprint-probe/runs/profile-after-update \
     --host chatgpt.com \
     --path /backend-api/codex/responses

   go run ./tools/request-fingerprint-probe compare \
     --left "$HTTPS_REF" \
     --right scratch/request-fingerprint-probe/runs/profile-after-update/cliproxy-utls.json

   go run ./tools/request-fingerprint-probe compare \
     --left "$WS_REF" \
     --right scratch/request-fingerprint-probe/runs/profile-after-update/cliproxy-utls-websocket.json
   ```

   Passing TLS validation means `no normalized drift detected` for HTTPS and websocket. Compare normalized fields, not raw bytes; ClientHello randomness makes byte-for-byte TLS equality unrealistic.

## Updating Code

- Update embedded profile names and raw ClientHello constants in `internal/runtime/executor/helps/utls_profile.go`.
- Keep the nested transport profile shape:
  ```yaml
  codex:
    tls-profile:
      https: auto
      websocket: auto
  ```
- Preserve `auto` behavior: Codex auth picks Codex profiles by transport; non-Codex auth stays on `chrome-auto`.
- Add or update tests in `internal/runtime/executor/helps/utls_client_test.go` before changing production selector behavior.
- Keep generated profile JSONs in `scratch/` unless the user explicitly asks to track fixtures.

## Header Capture

Use the `serve` sink captures for ordered request-line/header evidence. Expected useful paths include `/v1/models`, `/v1/responses`, websocket upgrade on `/v1/responses`, `/backend-api/codex/models`, and backend passthrough paths.

For plaintext header capture, point a temporary Codex config at the HTTP sink base URL printed by `serve`; do not edit the user’s real `~/.codex/config.toml`.

```bash
TMP_CODEX_HOME=scratch/request-fingerprint-probe/tmp-codex-home
mkdir -p "$TMP_CODEX_HOME"
cat > "$TMP_CODEX_HOME/config.toml" <<EOF
openai_base_url = "http://127.0.0.1:<port>/v1"
chatgpt_base_url = "http://127.0.0.1:<port>"
EOF
CODEX_HOME="$TMP_CODEX_HOME" "$CODEX_BIN" exec "Reply with exactly: ok" || true
```

Use the resulting `http-*.json` files for ordered headers. If the Codex version no longer honors these config keys, stop and inspect the current Codex config surface before inferring header drift.

Header pass bar is practical parity, not impossible upstream byte identity:

- Preserve incoming Codex app identity headers when present: `Version`, `Originator`, `User-Agent`, `X-Codex-Turn-Metadata`, `X-Client-Request-Id`, session/thread headers, `Authorization`, and `ChatGPT-Account-ID`.
- Do not log secrets. Redact auth tokens, account IDs when not needed, cookies, and large body payloads.
- Note whether header drift is caused by Go/Gorilla serialization rather than code-level header selection.

## Local Smoke

Use a local config under `scratch/` and auth files under `scratch/smoke-auths` or a user-approved copied auth directory. Do not leave the smoke server running.

Minimum smoke:

```bash
go run ./cmd/server --config scratch/<smoke-config>.yaml --no-browser
curl -sS -H 'Authorization: Bearer <local-key>' http://127.0.0.1:<port>/v1/models?client_version=<version>
curl -sS -H 'Authorization: Bearer <local-key>' http://127.0.0.1:<port>/backend-api/codex/models?client_version=<version>
curl -sS -H 'Authorization: Bearer <local-key>' -H 'Content-Type: application/json' \
  http://127.0.0.1:<port>/v1/responses \
  --data '{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"Reply with exactly: ok"}]}],"stream":false}'
```

Also smoke one streaming `/v1/responses` request. Backend passthrough paths may return upstream 400s such as missing workspace or missing plugin scope; that is acceptable when logs show `chatgpt backend passthrough completed`.

## Required Verification

Run these before reporting success:

```bash
gofmt -w <changed-go-files>
go test ./tools/request-fingerprint-probe
go test ./internal/runtime/executor/helps ./internal/runtime/executor
go test ./...
go build -o test-output ./cmd/server && rm test-output
git diff --check
```

Report the exact fingerprint hashes, smoke status codes, expected upstream 400s, whether the smoke server was stopped, and any remaining header-order risk.
