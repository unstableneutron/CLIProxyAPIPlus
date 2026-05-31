---
name: validating-local-pi-cliproxyapi
description: Use when validating this repo's local CLIProxyAPI dev instance through Pi, localhost-cliproxyapi, Cursor Composer, pi --mode json, or multi-turn agent route checks
---

# Validating Local Pi + CLIProxyAPI

## Overview

Validate the full local route: Pi CLI → `localhost-cliproxyapi` provider → local CLIProxyAPI server → Cursor auth/model. Use direct `bash` for `pi --mode json`; do not use an interactive shell for this finite JSON-mode command.

## Required Flow

1. **Server: reuse or start local-only**
   - First check `http://127.0.0.1:8317/healthz`.
   - If it responds, reuse it and do not kill it later.
   - If not, start a temporary server bound to `127.0.0.1:8317` with a temp config using `auth-dir: './auths'`, `--local-model`, and a throwaway API key. Record PID, temp config path, temp binary path, and log under `scratch/`.
   - Never print auth file contents or env secrets.

2. **Find the exact Pi model selector**
   - Do not guess raw names like `cursor-composer-2.5:medium`.
   - Do not use a separate `--provider` flag; the provider is part of the `--model` selector.
   - Do not run `pi --mode json --list-models`; list models separately:
     ```bash
     pi --list-models | grep -i -E 'cliproxy|cursor|composer'
     ```
   - For this repo, expected selector is usually:
     ```text
     localhost-cliproxyapi/cursor/composer-2.5:medium
     ```
   - The emitted JSON reports provider/model separately as `"provider":"localhost-cliproxyapi"` and `"model":"cursor/composer-2.5"`; the `:medium` effort suffix is not part of the emitted model. That is expected.

3. **Run Pi in JSON mode with a multi-tool prompt**
   - Use the exact selector in `--model`; do not add `--provider`.
   ```bash
   out="scratch/pi-localhost-cliproxyapi-composer-overview.jsonl"
   err="scratch/pi-localhost-cliproxyapi-composer-overview.stderr"
   timeout 180s pi \
     --model localhost-cliproxyapi/cursor/composer-2.5:medium \
     --mode json \
     "Explore and tell me a high level overview of what this repo does. Use the repository files in the current working directory. Be curious: inspect multiple areas of the codebase before answering, and give a high-level overview rather than making changes." \
     >"$out" 2>"$err"
   ```

4. **Prove it worked**
   Required evidence:
   - exit code is `0`
   - stderr has no fatal/auth/upstream/model-resolution errors
   - stdout JSONL is non-empty and not truncated
   - final records include separate fields `"provider":"localhost-cliproxyapi"` and `"model":"cursor/composer-2.5"`
   - Do **not** look for one combined emitted string like `localhost-cliproxyapi/cursor/composer-2.5:medium`; that is the CLI selector, not the JSON model field.
   - Do **not** require the emitted model to include `:medium`; effort suffixes are selector-only.
   - transcript includes tool-call/tool-result events, proving multiple agent rounds
   - final assistant answer exists and is grounded in repository inspection
   - usage appears, e.g. `"totalTokens":...`

   Useful summary commands:
   ```bash
   wc -l "$out" "$err"
   grep -o '"type":"toolCall"' "$out" | wc -l
   grep -o '"role":"toolResult"' "$out" | wc -l
   grep -o '"provider":"localhost-cliproxyapi"' "$out" | tail -1
   grep -o '"model":"cursor/composer-2.5"' "$out" | tail -1
   grep -o '"totalTokens":[0-9]*' "$out" | tail -1
   ```

## Model Selector Error Recovery

If Pi says `Model ... not found`:

1. Run `pi --list-models | grep -i -E 'cliproxy|cursor|composer'`.
2. Retry with the exact provider-scoped selector from the list.
3. Do not add `--provider`; fix the `--model` selector.
4. Treat selector errors as model-routing/config problems, not prompt problems.

## Cleanup

- If you started a temporary server, kill only that recorded PID and delete the temp binary/config.
- If you reused an existing server on `8317`, leave it running.
- Keep validation artifacts in `scratch/`; do not write transcripts containing request context outside the repo.

## Insufficient Evidence

- “It printed an answer” without JSONL evidence.
- Empty stderr alone.
- Provider/model present but no final answer or no tool events.
- Checking for `localhost-cliproxyapi/cursor/composer-2.5:medium` as an emitted JSON model; the emitted fields are separate and the model omits `:medium`.
- A plausible overview that lacks `localhost-cliproxyapi` provider evidence.
- A command using `--provider cliproxyapi`; this bypasses the required provider-scoped selector pattern.
- A killed/timeout run unless it still has a complete final message and exit status is understood.
