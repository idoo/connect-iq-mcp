# Garmin MCP

MCP server for Garmin Connect IQ developers. Your Connect IQ Store apps, reviews, SDK builds and the simulator, from Claude, Codex, Cursor or any other MCP client.

## Install

Paste this into your coding agent:

```
Set up Garmin MCP for me: https://github.com/idoo/garmin-mcp. Install it, run `garmin-mcp auth login` so I can sign in, then `garmin-mcp config install <your client>`.
```

Or by hand:

```sh
brew install idoo/tap/garmin-mcp
# or: curl -fsSL https://raw.githubusercontent.com/idoo/garmin-mcp/main/install.sh | sh
# or: go install github.com/idoo/garmin-mcp/cmd/garmin-mcp@latest   (fetches Go 1.26.10 if yours is older)

garmin-mcp auth login
garmin-mcp config install claude-code   # or claude-desktop, codex, cursor, vscode, windsurf
garmin-mcp doctor
```

`garmin-mcp config install vscode` writes to every VS Code it finds: Code, Code - OSS, VSCodium, and their Flatpaks on Linux. There is no Docker image: sign-in needs a browser, and builds need your SDK and simulator.

- macOS: after upgrading from an older version, run `garmin-mcp auth login` once more. The sign-in browser now encrypts its cookies with its own Safe Storage keychain key and can't read the old ones. Chrome for Testing still uses Chromium's fixed mock key.
- Linux over SSH: the keyring is usually there but locked, and that's an error, not a reason to fall back to the file. Unlock it (`gnome-keyring-daemon --unlock`) or set `GARMIN_MCP_KEYRING=off`. With no D-Bus session at all, the 0600 file is used.
- Ubuntu 24.04: AppArmor won't let the downloaded Chrome for Testing start, so install Chrome, Brave or Edge from their .deb. On arm64 Linux use the distro's `chromium` package (not the snap) or Brave.
- Linux: the simulator is a GUI app. With no desktop, start the server under `xvfb-run`.
- Windows: the installer works in Git Bash, or grab the zip from [releases](https://github.com/idoo/garmin-mcp/releases). The `.mcpb` bundle is x64 only: Windows 11 on Arm runs it emulated, and on Windows 10 on Arm you take the arm64 zip and run `garmin-mcp config install claude-desktop`, which also finds the MSIX build of Claude Desktop.
- WSL: `config install claude-desktop` and `vscode` write nothing and print a `wsl.exe` snippet for the Windows app's config.

## Sign-in

`garmin-mcp auth login` opens a separate profile of a Chromium browser (Chrome, Brave, Edge; on macOS only vendor-signed copies), or a downloaded Chrome for Testing if you have none. Safari and Firefox can't be driven. Sign in once and tick Remember me. The portal session (about 15 days) then renews in the background in a headless browser. If renewal fails, tools return `auth_required`: run `garmin-mcp auth login` again. If Garmin rate-limits sign-ins instead (Cloudflare 1015), renewal pauses for 90 minutes and tools return `rate_limited` with the time to wait until. Don't sign in during that wait; it hits the same limit.

Agents can open the same window with the `auth_login` tool, one window at a time, with a 10 minute pause after a window closes without a sign-in.

Garmin Connect health data is a separate opt-in: `garmin-mcp auth login --connect`.

The portal session cookie and Connect tokens go to the OS keychain. Without one (no Secret Service or no D-Bus session on Linux, or `GARMIN_MCP_KEYRING=off`) they go to a 0600 file instead, DPAPI-encrypted on Windows. A keychain that is there but locked is an error until you unlock it or set that variable. `garmin-mcp auth status` says which and why. The browser profile in `~/.local/state/garmin-mcp/browser/` holds the long-lived Garmin session; how its cookies are protected is in [SECURITY.md](SECURITY.md).

Profiles (`--profile`, `[profiles.<name>]` in the config) are named with lowercase letters, digits, `-` and `_`, up to 63 characters. A config with a profile called `Work` no longer loads: rename it to `work` and sign in again under the new name.

## Builds and signing

Store builds must be signed with the key of your first upload. Point at it with `GARMIN_MCP_KEY=/path/to/developer_key.der`; it can live outside the repo and is only read. `sdk_key_generate` is for a brand-new app, and in a git repo it refuses unless the key path is git-ignored. `sdk_build device:"all"` writes `release/<project>-<version>.iq`; long builds return a `build_id` to poll, and asking again for the same output while it runs gives the same `build_id`.

Builds and the simulator need Java 17+ (`JAVA_HOME` as an absolute path, or `java` on PATH). Garmin MCP runs the SDK's jar itself, never its `monkeyc`/`monkeydo` scripts. Every path in `monkey.jungle` must stay inside the project root.

## What you can ask

- "How are my apps doing? Downloads, ratings, last version."
- "Any new reviews? Draft replies to the angry ones."
- "Build the app for fenix 8 and run it in the simulator, take a screenshot."
- "Build the store .iq for all devices and publish it as 1.4.0." (asks you first)
- "Update the store description, keep the rest as is." (asks you first)
- "Which devices does the app support, and which new ones am I missing?"

## Toolsets

| Toolset | What | Default |
|---|---|---|
| core | status, sign-in, toolset switching | on |
| store | apps, listing, versions, reviews; publish a version | on |
| sdk | monkeyc builds, devices, manifests, keys | on |
| simulator | run apps in the simulator, screenshots | on |
| store_experimental | portal endpoints never tried live: app create/delete, screenshots, review replies | off |
| connect | Garmin Connect data, needs `auth login --connect` | off |

Pick with `GARMIN_MCP_TOOLSETS` (`all`, `+connect`). Every tool: [docs/TOOLS.md](docs/TOOLS.md).

## Safety

- Read-only until you say otherwise: ask the agent to enable writes, or set `GARMIN_MCP_ALLOW_WRITES=1` (or `--allow-writes`). SDK builds and the simulator work either way.
- Anything public (publishing a version, editing the listing, replying to a review) shows you a preview and waits for your yes.
- Review text is written by strangers. The server treats it as untrusted input, and so should your agent.

## CI

No unattended long-lived sign-in. `garmin-mcp auth export --portal | gh secret set GARMIN_MCP_PORTAL_COOKIES`, then set that env var in the job. It holds only the portal `session` cookie, which lasts about 15 days and is never renewed.

## Unofficial

Garmin MCP is not made, endorsed or supported by Garmin. It uses the developer portal's private web API and Garmin's sign-in, which can change and break it any day. Renewal runs a headless browser that doesn't announce itself as automated. Automated access may be against the portal's terms; it's your account and your call. It keeps request rates low. If Garmin objects to the name, the name changes.

## License

MIT
