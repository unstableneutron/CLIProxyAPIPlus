---
name: validating-local-pi-cliproxyapi
description: Use when validating this repo's local CLIProxyAPI dev instance through Pi, provider-scoped model selectors, Cursor Composer, pi --mode json, or multi-turn agent route checks
---

# Validating Local Pi + CLIProxyAPI

## Overview

Validate the full local route: Pi CLI → local CLIProxyAPI provider selector → local CLIProxyAPI server → target auth/model. Use direct `bash` for `pi --mode json`; do not use an interactive shell for this finite JSON-mode command.

## Required Flow

1. **Server: reuse or start local-only**
   - First check `http://127.0.0.1:8317/healthz`.
   - If it responds, reuse it and do not kill it later.
   - If not, start a temporary server bound to `127.0.0.1:8317` with a temp config using `auth-dir: './auths'`, `--local-model`, and a throwaway API key. Record PID, temp config path, temp binary path, and log under `scratch/`.
   - `--local-model` disables the registry remote updater, not executor/auth-discovered live catalogs such as CommandCode `/provider/v1/models`.
   - Never print auth file contents or env secrets.

2. **Find the exact Pi model selector**
   - Do not guess raw names like `cursor-composer-2.5:medium`.
   - Do not use a separate `--provider` flag; the provider is part of the `--model` selector.
   - Do not run `pi --mode json --list-models`; list models separately and grep for the provider/model under test:
     ```bash
     pi --list-models | grep -i -E 'cliproxy|cursor|composer|<provider>|<model>'
     ```
   - Cursor smoke-test selector example:
     ```text
     <local-cliproxy-provider>/cursor/composer-2.5:medium
     ```
   - The emitted JSON reports provider/model separately as `"provider":"<local-cliproxy-provider>"` and `"model":"cursor/composer-2.5"`; the `:medium` effort suffix is not part of the emitted model. That slash-form ID is Pi-facing; executor internals may use hyphen names such as `cursor-composer-2.5`.

3. **Run Pi in JSON mode with a multi-tool prompt**
   - Use the exact target-provider selector in `--model`; do not add `--provider`.
   - Do not validate only Cursor unless Cursor is the changed provider; the command below is a Cursor smoke-test example.
   ```bash
   out="scratch/pi-local-cliproxy-provider-composer-overview.jsonl"
   err="scratch/pi-local-cliproxy-provider-composer-overview.stderr"
   timeout 180s pi \
     --model <local-cliproxy-provider>/cursor/composer-2.5:medium \
     --mode json \
     "Explore and tell me a high level overview of what this repo does. Use the repository files in the current working directory. Be curious: inspect multiple areas of the codebase before answering, and give a high-level overview rather than making changes." \
     >"$out" 2>"$err"
   ```

4. **Wait for auth/provider model registration when needed**
   - Immediately after server start, `/v1/models` can race auth-discovered provider registration.
   - For provider-specific checks, retry for up to 30s until expected IDs appear before declaring a catalog failure.
   - Save captured model responses under `scratch/` and record both total count and expected IDs.
   - For CommandCode, prove live catalog discovery with `/provider/v1/models`-derived IDs while remembering generation still uses `/alpha/generate`.

5. **Prove it worked**
   Required evidence for the target selector/model under test:
   - exit code is `0`
   - stderr has no fatal/auth/upstream/model-resolution errors
   - stdout JSONL is non-empty and not truncated
   - final records include separate fields like `"provider":"<local-cliproxy-provider>"` and the expected target `"model":"<target-model-id>"`
   - Do **not** look for one combined emitted string like `<local-cliproxy-provider>/<provider>/<model>:medium`; that is the CLI selector, not the JSON model field.
   - Do **not** require the emitted model to include `:medium`; effort suffixes are selector-only.
   - transcript includes tool-call/tool-result events, proving multiple agent rounds
   - final assistant answer exists and is grounded in repository inspection
   - usage appears, e.g. `"totalTokens":...`

   Useful summary commands:
   ```bash
   target_model='cursor/composer-2.5' # replace with the emitted model field for the provider under test
   wc -l "$out" "$err"
   grep -o '"type":"toolCall"' "$out" | wc -l
   grep -o '"role":"toolResult"' "$out" | wc -l
   grep -o '"provider":"<local-cliproxy-provider>"' "$out" | tail -1
   grep -o '"model":"'"$target_model"'"' "$out" | tail -1
   grep -o '"totalTokens":[0-9]*' "$out" | tail -1
   ```

## Provider Catalog Evidence

Useful `/v1/models` retry shape, using an expected ID from docs, config, test fixture, or provider catalog response:
```bash
expected='expected-model-id'
for i in $(seq 1 30); do
  curl -fsS -H "Authorization: Bearer $KEY" http://127.0.0.1:8317/v1/models > "scratch/models-$i.json" || true
  grep -q "$expected" "scratch/models-$i.json" && break
  sleep 1
done
```

Treat a missing expected model on the first response as inconclusive, not failed.

## Model Selector Error Recovery

If Pi says `Model ... not found`:

1. Run `pi --list-models | grep -i -E 'cliproxy|cursor|composer|<provider>|<model>'`.
2. Retry with the exact provider-scoped selector from the list.
3. Do not add `--provider`; fix the `--model` selector.
4. Treat selector errors as model-routing/config problems, not prompt problems.

## CommandCode-Specific Checks

- Live catalog: expected IDs such as `cc/deepseek/deepseek-v4-flash` should appear after registration.
- Streaming generation: validate the `/alpha/generate` translation path separately from `/provider/v1/models` discovery.
- Usage evidence should avoid cache-token double counting when provider `totalTokens` is present.

## Cleanup

- If you started a temporary server, kill only that recorded PID and delete the temp binary/config.
- If you reused an existing server on `8317`, leave it running.
- Keep validation artifacts in `scratch/`; do not write transcripts containing request context outside the repo.

## Insufficient Evidence

- “It printed an answer” without JSONL evidence.
- Empty stderr alone.
- Provider/model present but no final answer or no tool events.
- Checking for `<local-cliproxy-provider>/cursor/composer-2.5:medium` as an emitted JSON model; the emitted fields are separate and the model omits `:medium`.
- A plausible overview that lacks local CLIProxyAPI provider evidence.
- A command using `--provider cliproxyapi`; this bypasses the required provider-scoped selector pattern.
- A killed/timeout run unless it still has a complete final message and exit status is understood.
