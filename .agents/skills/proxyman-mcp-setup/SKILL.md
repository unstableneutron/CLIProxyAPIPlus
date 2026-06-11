---
name: proxyman-mcp-setup
description: >
  Connect an AI coding agent to Proxyman MCP. Use this skill when Proxyman is
  installed but the agent cannot see Proxyman MCP tools, the user asks to set up
  Proxyman MCP, configure Codex, Claude, Cursor, VS Code, Copilot, or troubleshoot
  missing Proxyman MCP tools, handshake errors, or "tool not found" issues.
---

# Proxyman MCP Setup

Configure an agent to talk to Proxyman MCP through Proxyman's bundled stdio bridge.

## Operating Rules

1. This skill is shell-first. MCP tools may not be connected yet.
2. Do not use API-key or direct HTTP MCP configuration. Proxyman MCP uses a local stdio bridge executable.
3. Proxyman must be installed, running, and have Settings > MCP > MCP Server enabled before verification can pass.
4. Preserve existing MCP server entries in agent config files. Add or update only the `proxyman` entry.
5. Prefer the exact command shown in Proxyman Settings > MCP when the app exposes one.

## Mental Model

Proxyman MCP has two local pieces:

1. The AI agent launches Proxyman's bundled `mcp-server` executable over stdio.
2. The bridge reads `mcp-handshake.json` from Proxyman's app data folder.
3. The bridge forwards tool calls to the running app at `http://127.0.0.1:<ephemeral-port>/mcp` with a bearer token from the handshake file.

Do not hardcode the HTTP port or token. The app regenerates them.

Common handshake locations:

| Platform/build | Handshake folder |
|----------------|------------------|
| macOS native app | Proxyman Application Support bundle folder, including regular and Setapp bundle IDs |
| Windows Electron app | `%APPDATA%\Proxyman` |
| Linux Electron app | `${XDG_CONFIG_HOME:-$HOME/.config}/Proxyman` |

Do not edit the handshake file. If it is missing or stale, restart Proxyman and re-enable Settings > MCP.

## Step 1: Verify Proxyman Is Installed

### macOS

```bash
if [ -d "/Applications/Proxyman.app" ]; then
  echo "INSTALLED: /Applications/Proxyman.app"
elif mdfind 'kMDItemCFBundleIdentifier == "com.proxyman.NSProxy"' | grep -q "Proxyman.app"; then
  mdfind 'kMDItemCFBundleIdentifier == "com.proxyman.NSProxy"'
else
  echo "NOT_INSTALLED"
fi
```

### Windows (PowerShell)

```powershell
$installed = Get-ItemProperty `
  "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*",
  "HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*",
  "HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*" `
  -ErrorAction SilentlyContinue |
  Where-Object { $_.DisplayName -like "*Proxyman*" }
if ($installed) { "INSTALLED" } else { "NOT_INSTALLED" }
```

### Linux

```bash
if command -v proxyman >/dev/null 2>&1 || ls "$HOME"/Downloads/Proxyman*.AppImage "$HOME"/Downloads/proxyman*.AppImage >/dev/null 2>&1; then
  echo "INSTALLED"
else
  echo "NOT_INSTALLED"
fi
```

If Proxyman is not installed, stop and use `proxyman-download-setup`.

## Step 2: Launch Proxyman And Enable MCP

### macOS

```bash
open -a "Proxyman"
sleep 10
```

### Windows (PowerShell)

```powershell
$candidates = @(
  "$env:LOCALAPPDATA\Programs\Proxyman\Proxyman.exe",
  "C:\Program Files\Proxyman\Proxyman.exe",
  "C:\Program Files (x86)\Proxyman\Proxyman.exe"
)
$proxymanExe = $candidates | Where-Object { Test-Path $_ } | Select-Object -First 1
if ($proxymanExe) { Start-Process $proxymanExe; Start-Sleep 10 }
```

### Linux

```bash
if command -v proxyman >/dev/null 2>&1; then
  nohup proxyman >/dev/null 2>&1 &
else
  nohup "$HOME/Downloads/Proxyman.AppImage" >/dev/null 2>&1 &
fi
sleep 10
```

Ask the user to open Proxyman Settings > MCP and enable "MCP Server". If the MCP toggle is locked, the user must authorize the plan/license required by their Proxyman build.

Keep "Redact Sensitive Data Before Sending to AI" enabled unless the user explicitly wants raw headers, cookies, query strings, or bodies sent to the agent.

On Linux AppImage builds, launching Proxyman lets the app copy the packaged MCP bridge into a stable config path and mark it executable. If the bridge path below is missing, enable MCP in Settings, restart Proxyman, and check again.

## Step 3: Resolve The Bridge Executable

### macOS

Regular build:

```bash
BRIDGE_PATH="/Applications/Proxyman.app/Contents/MacOS/mcp-server"
test -x "$BRIDGE_PATH" && echo "$BRIDGE_PATH"
```

If the regular path is missing, search installed Proxyman apps:

```bash
mdfind 'kMDItemCFBundleIdentifier == "com.proxyman.NSProxy" || kMDItemCFBundleIdentifier == "com.proxyman.NSProxy-setapp"' |
while read -r app; do
  candidate="$app/Contents/MacOS/mcp-server"
  [ -x "$candidate" ] && echo "$candidate"
done
```

### Windows (PowerShell)

The Windows Electron app copies `mcp-server.exe` beside `Proxyman.exe`. If no bridge is found, use the exact command from Proxyman Settings > MCP.

```powershell
$exeCandidates = @(
  "$env:LOCALAPPDATA\Programs\Proxyman\Proxyman.exe",
  "C:\Program Files\Proxyman\Proxyman.exe",
  "C:\Program Files (x86)\Proxyman\Proxyman.exe"
)
$proxymanExe = $exeCandidates | Where-Object { Test-Path $_ } | Select-Object -First 1

$bridgeCandidates = @()
if ($proxymanExe) {
  $bridgeCandidates += Join-Path (Split-Path $proxymanExe -Parent) "mcp-server.exe"
}
$bridgeCandidates += @(
  "$env:LOCALAPPDATA\Programs\Proxyman\mcp-server.exe",
  "C:\Program Files\Proxyman\mcp-server.exe",
  "C:\Program Files (x86)\Proxyman\mcp-server.exe"
)

$bridge = $bridgeCandidates | Where-Object { Test-Path $_ } | Select-Object -First 1
if ($bridge) { $bridge } else { "NOT_FOUND: copy the path from Proxyman Settings > MCP" }
```

### Linux

The packaged Linux AppImage copies `mcp-server` into Proxyman's config folder after launch. Prefer that stable copied path.

```bash
CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
BRIDGE_PATH="$CONFIG_HOME/Proxyman/bin/mcp-server"

if [ -x "$BRIDGE_PATH" ]; then
  echo "$BRIDGE_PATH"
else
  echo "NOT_FOUND: launch Proxyman, enable Settings > MCP, restart Proxyman, or copy the path from Settings > MCP"
fi
```

Set `BRIDGE_PATH` to the chosen executable path. It must be the stdio bridge, not the Proxyman app binary.

## Step 4: Detect The Agent Config

Use environment variables first, then parent process, then filesystem markers.

```bash
[ -n "$OPENAI_CODEX" ] && echo "codex"
[ -n "$CLAUDE_CODE_ENTRYPOINT" ] && echo "claude-code"
[ -n "$CURSOR_TRACE_ID" ] || [ "$TERM_PROGRAM" = "cursor" ] && echo "cursor"
[ -n "$VSCODE_PID" ] || [ "$TERM_PROGRAM" = "vscode" ] && echo "vscode"
[ -n "$GITHUB_COPILOT_CLI" ] && echo "copilot-cli"
```

Fallback markers:

```bash
test -d "$HOME/.codex" && echo "codex"
test -d "$HOME/.claude" && echo "claude-code"
test -d "$HOME/.cursor" && echo "cursor"
test -d ".vscode" && echo "vscode"
test -f "$HOME/.copilot/mcp-config.json" && echo "copilot-cli"
test -f "$HOME/Library/Application Support/Claude/claude_desktop_config.json" && echo "claude-desktop"
```

If multiple agents are detected, ask the user which agent they want to configure.

## Step 5: Add The MCP Server

### Codex CLI

Preferred command:

```bash
codex mcp add proxyman -- "$BRIDGE_PATH"
```

Equivalent TOML:

```toml
[mcp_servers.proxyman]
enabled = true
command = "BRIDGE_PATH"
args = []
```

Config file: `~/.codex/config.toml`.

### Claude Code

```bash
claude mcp add proxyman --transport stdio -- "$BRIDGE_PATH"
```

Config file: `~/.claude.json`.

### Claude Desktop

Config file:

- macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`
- Windows: `%APPDATA%\Claude\claude_desktop_config.json`

Server entry:

```json
{
  "mcpServers": {
    "proxyman": {
      "command": "BRIDGE_PATH",
      "args": [],
      "env": {}
    }
  }
}
```

### Cursor

Config file: `~/.cursor/mcp.json`.

```json
{
  "mcpServers": {
    "proxyman": {
      "command": "BRIDGE_PATH",
      "args": [],
      "env": {}
    }
  }
}
```

### VS Code / GitHub Copilot

Config file:

- macOS: `~/Library/Application Support/Code/User/mcp.json` or workspace `.vscode/mcp.json`
- Linux: `~/.config/Code/User/mcp.json` or workspace `.vscode/mcp.json`
- Windows: `%APPDATA%\Code\User\mcp.json` or workspace `.vscode\mcp.json`

```json
{
  "servers": {
    "proxyman": {
      "command": "BRIDGE_PATH",
      "args": [],
      "env": {}
    }
  }
}
```

### GitHub Copilot CLI

Config file: `~/.copilot/mcp-config.json`.

```json
{
  "mcpServers": {
    "proxyman": {
      "command": "BRIDGE_PATH",
      "args": [],
      "env": {},
      "tools": ["*"]
    }
  }
}
```

When editing JSON or TOML config manually, parse and merge with a real parser when possible. Never replace unrelated `mcpServers` or `servers` entries.

## Step 6: Verify

Restart or reload the agent after changing config.

Verification sequence:

1. Confirm Proxyman is running.
2. Confirm Settings > MCP shows the server running.
3. Ask the agent to list MCP tools/resources/prompts if it supports discovery.
4. Call `get_version`.
5. Call `get_proxy_status`.

Successful setup means the agent can see Proxyman tools and `get_version` returns a Proxyman app/bridge response.

## Troubleshooting

| Error | Meaning | Action |
|-------|---------|--------|
| `Proxyman is not running or MCP server not started` | Bridge cannot find `mcp-handshake.json`. | Launch Proxyman and enable Settings > MCP. |
| `Invalid handshake file` | Token or port is stale. | Restart Proxyman, then reload the agent. |
| `Cannot connect to Proxyman` | App is closed or MCP server stopped. | Open Proxyman and confirm MCP status. |
| Linux bridge path is missing | The AppImage has not prepared `mcp-server` in the config folder yet. | Launch Proxyman, enable Settings > MCP, restart Proxyman, then check `${XDG_CONFIG_HOME:-$HOME/.config}/Proxyman/bin/mcp-server`. |
| Agent has no Proxyman tools | Config path is wrong or agent was not reloaded. | Re-check `BRIDGE_PATH`, config file, and restart the agent. |
| User sees no traffic after setup | MCP is connected but capture is not configured. | Use `proxyman-traffic-debugging` and start with `get_proxy_status`. |

## Next Step

Once `get_version` and `get_proxy_status` work, use `proxyman-traffic-debugging` for traffic inspection, setup diagnosis, rule creation, Compose, WebSocket debugging, certificates, and Proxyman MCP operations.
