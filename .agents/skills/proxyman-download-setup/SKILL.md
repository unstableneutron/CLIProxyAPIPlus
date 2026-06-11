---
name: proxyman-download-setup
description: >
  Download, install, launch, and prepare Proxyman from scratch on macOS, Windows,
  or Linux. Use this skill when Proxyman is not installed, the user asks to install
  Proxyman, download Proxyman, get started with Proxyman, set up Proxyman from zero,
  or prepare Proxyman before configuring MCP.
---

# Proxyman Download & Setup

Guide the user through installing and launching Proxyman. This skill is shell-first because Proxyman and its MCP tools may not be available yet.

## Operating Rules

1. Do not use Proxyman MCP tools until Proxyman is installed, launched, and MCP is configured.
2. Do not hardcode app versions. Use Proxyman's latest release redirects.
3. On Windows, prefer PowerShell. If the active shell is not PowerShell, wrap PowerShell snippets with `powershell.exe -Command '...'`.
4. Only install the macOS Helper Tool when system proxy automation is needed or the user asks for it.
5. Only install a root certificate when the user needs HTTPS decryption. Basic HTTP capture and app installation do not require certificate installation.
6. On Windows and Linux, launch Proxyman at least once before MCP setup so the app can expose or prepare the bundled MCP bridge.

## Phase 1: Check Existing Installation

### macOS

```bash
if [ -d "/Applications/Proxyman.app" ]; then
  VERSION=$(/usr/libexec/PlistBuddy -c "Print CFBundleShortVersionString" "/Applications/Proxyman.app/Contents/Info.plist" 2>/dev/null)
  echo "INSTALLED: ${VERSION:-unknown}"
elif mdfind 'kMDItemCFBundleIdentifier == "com.proxyman.NSProxy"' | grep -q "Proxyman.app"; then
  echo "INSTALLED: found via Spotlight"
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

if ($installed) {
  Write-Host "INSTALLED: $($installed.DisplayVersion)"
} else {
  Write-Host "NOT_INSTALLED"
}
```

### Linux

```bash
if command -v proxyman >/dev/null 2>&1; then
  echo "INSTALLED: $(command -v proxyman)"
elif ls "$HOME"/Downloads/Proxyman*.AppImage "$HOME"/Downloads/proxyman*.AppImage >/dev/null 2>&1; then
  echo "INSTALLED: AppImage in Downloads"
else
  echo "NOT_INSTALLED"
fi
```

If Proxyman is already installed, launch it and continue to preparation. If the user needs the latest build, download using the platform redirect below.

## Phase 2: Download And Install

### macOS: Homebrew Preferred

If Homebrew is installed, use the cask:

```bash
brew install --cask proxyman
```

If Homebrew is unavailable or the user prefers a direct download, use the latest DMG redirect:

```bash
curl -L "https://proxyman.com/release/osx/Proxyman_latest.dmg" -o "$HOME/Downloads/Proxyman.dmg"
hdiutil attach "$HOME/Downloads/Proxyman.dmg" -nobrowse
cp -R "/Volumes/Proxyman/Proxyman.app" /Applications/
hdiutil detach "/Volumes/Proxyman"
```

If the volume name differs, list attached volumes and copy from the mounted Proxyman volume:

```bash
ls /Volumes
```

### Windows

The Windows endpoint name ends in `.dmg` for legacy reasons, but it redirects to the latest Windows installer.
The installer is an NSIS one-click installer and normally launches Proxyman after completion.

```powershell
$installer = "$env:USERPROFILE\Downloads\ProxymanSetup.exe"
Invoke-WebRequest "https://proxyman.com/release/windows/Proxyman_latest.dmg" -OutFile $installer -MaximumRedirection 5
Start-Process $installer -ArgumentList "/S" -Wait
```

If silent installation fails, run the installer interactively:

```powershell
Start-Process $installer -Wait
```

### Linux

Download the latest AppImage redirect, make it executable, and launch it:

```bash
curl -L "https://proxyman.com/release/linux/proxyman_latest" -o "$HOME/Downloads/Proxyman.AppImage"
chmod +x "$HOME/Downloads/Proxyman.AppImage"
nohup "$HOME/Downloads/Proxyman.AppImage" >/dev/null 2>&1 &
```

The packaged Linux app prepares its MCP bridge on first launch. After Proxyman has opened, the stable bridge path should be:

```bash
BRIDGE_PATH="${XDG_CONFIG_HOME:-$HOME/.config}/Proxyman/bin/mcp-server"
test -x "$BRIDGE_PATH" && echo "MCP_BRIDGE: $BRIDGE_PATH"
```

## Phase 3: Launch Proxyman

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
if ($proxymanExe) {
  Start-Process $proxymanExe
  Start-Sleep 10
} else {
  Write-Host "Proxyman executable not found. Launch Proxyman from the Start menu, then continue."
}
```

After launch, the bundled MCP bridge should live next to the app executable. For the default per-user install:

```powershell
$bridge = "$env:LOCALAPPDATA\Programs\Proxyman\mcp-server.exe"
if (Test-Path $bridge) { Write-Host "MCP_BRIDGE: $bridge" }
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

If the Linux MCP bridge path is missing after launch, ask the user to open Settings > MCP and enable MCP Server, then relaunch Proxyman so the AppImage can copy and `chmod` the bridge into the config folder.

## Windows And Linux MCP Bridge Notes

Windows and Linux Proxyman builds use the same MCP model as macOS: an AI agent launches a bundled `mcp-server` process over stdio, and that bridge talks to the running local Proxyman app through an authenticated localhost handshake.

Use these bridge paths when moving on to `proxyman-mcp-setup`:

| Platform | Bridge path |
|----------|-------------|
| Windows default install | `%LOCALAPPDATA%\Programs\Proxyman\mcp-server.exe` |
| Windows custom install | `mcp-server.exe` in the same folder as `Proxyman.exe` |
| Linux AppImage | `${XDG_CONFIG_HOME:-$HOME/.config}/Proxyman/bin/mcp-server` |

Do not configure agents to use `Proxyman.exe` or the `.AppImage` as the MCP command. The command must be the stdio bridge executable.

## Phase 4: Prepare Proxy Capture

Proxyman can capture different targets in different ways. Ask what the user wants to capture before changing system state.

- Desktop apps and browsers often use the system proxy.
- CLI runtimes such as Node.js, Python, Ruby, and Go may need Proxyman Automatic Setup or Manual Setup.
- Localhost traffic may need Reverse Proxy because many clients bypass the system proxy for localhost.
- iOS apps behind VPNs may need Atlantis instead of classic proxy capture.

## macOS Helper Tool

The Helper Tool is macOS-only. Install it when the user wants Proxyman to override system proxy settings reliably across network interfaces, or when system proxy automation fails because privileged network settings are required.

Tell the user that macOS may show a visible password or approval dialog, then run:

```bash
open -a "Proxyman" --args --install-privileged-components
```

If macOS shows an approval prompt in System Settings, ask the user to approve Proxyman's Helper Tool and retry the system proxy action.

## Root Certificate

Root certificate setup is required for HTTPS decryption, not for merely installing Proxyman.

Preferred sequence:

1. Finish app installation.
2. Configure MCP with `proxyman-mcp-setup` if the user wants agent automation.
3. Use `get_certificate_status`.
4. If HTTPS decryption is needed and the certificate is missing or untrusted, use `install_certificate` through MCP or Proxyman's Certificate menu.

Without MCP, guide the user to Proxyman's in-app Certificate menu and choose the setup path for the target platform.

On Windows, automatic certificate installation may show a UAC prompt and can use an elevated `certutil` flow. On Linux, automatic trust-store installation is designed for Ubuntu/Debian, Fedora/RHEL, openSUSE/SUSE, and Arch-family systems; unsupported distros may require manual trust-store commands from the Proxyman UI.

Some Proxyman builds expose `proxyman-cli`. Use it only after confirming it exists with `command -v proxyman-cli` or `Get-Command proxyman-cli`. For custom root certificates only, a CLI build that supports root import can use:

```bash
proxyman-cli install-root-cert /path/to/custom-root.p12 --password "password" --trust
```

Do not use the custom root certificate CLI command for the default Proxyman-generated root CA.

## Phase 5: Suggest MCP Setup

After Proxyman is installed and running, offer to configure MCP:

```text
Proxyman is installed and running. The next step is to connect your AI agent to Proxyman MCP so it can inspect traffic, manage rules, install certificates when needed, and use Proxyman setup guidance. Use the proxyman-mcp-setup skill next.
```

## Troubleshooting

| Issue | Action |
|-------|--------|
| macOS DMG volume name is different | Run `ls /Volumes`, find the Proxyman volume, and copy `Proxyman.app` from it. |
| macOS app will not open | Run `xattr -cr "/Applications/Proxyman.app"` and launch again. |
| Windows SmartScreen blocks installer | Ask the user to approve the installer from the visible Windows prompt. |
| Linux AppImage does not run | Ensure it is executable and that required AppImage/FUSE dependencies are installed. |
| System proxy cannot be enabled on macOS | Install or approve the Helper Tool, then retry. |
| HTTPS traffic is still opaque | Install and trust the root certificate, then enable SSL Proxying for the target host. |
