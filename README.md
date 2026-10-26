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
# or: go install github.com/idoo/garmin-mcp/cmd/garmin-mcp@latest

garmin-mcp auth login
garmin-mcp config install claude-code   # or claude-desktop, codex, cursor, vscode, windsurf
garmin-mcp doctor
```

Windows: the installer works in Git Bash, or grab the zip from [releases](https://github.com/idoo/garmin-mcp/releases). There is no Docker image: sign-in needs a browser, and builds need your SDK and simulator.

## Sign-in

`garmin-mcp auth login` opens a separate profile of a Chromium browser (Chrome, Brave, Edge; on macOS only vendor-signed copies), or a downloaded Chrome for Testing if you have none. Safari and Firefox can't be driven. Sign in once and tick Remember me. The portal session (about 15 days) then renews in the background in a headless browser. If renewal fails, tools return `auth_required`: run `garmin-mcp auth login` again.

Agents can open the same window with the `auth_login` tool, one window at a time, with a 10 minute pause after a window closes without a sign-in.

Garmin Connect health data is a separate opt-in: `garmin-mcp auth login --connect`.

Cookies and tokens go to the OS keychain. Without one (headless Linux with no Secret Service, or `GARMIN_MCP_KEYRING=off`) they go to a 0600 file instead, DPAPI-encrypted on Windows. `garmin-mcp auth status` says which and why. The browser profile in `~/.local/state/garmin-mcp/browser/` holds the long-lived Garmin session, guarded only by file permissions; password saving is off there, and macOS keeps it out of Time Machine.

## Signing key

Store builds must be signed with the key of your first upload. Point at it with `GARMIN_MCP_KEY=/path/to/developer_key.der`; it can live outside the repo and is only read. `sdk_key_generate` is for a brand-new app, and in a git repo it refuses unless the key path is git-ignored. `sdk_build device:"all"` writes `release/<project>-<version>.iq`; long builds return a `build_id` to poll.

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

No unattended long-lived sign-in. `garmin-mcp auth export --portal | gh secret set GARMIN_MCP_PORTAL_COOKIES`, then set that env var in the job. The cookies last about 15 days and are never renewed.

## Unofficial

Garmin MCP is not made, endorsed or supported by Garmin. It uses the developer portal's private web API and Garmin's sign-in, which can change and break it any day. Renewal runs a headless browser that doesn't announce itself as automated. Automated access may be against the portal's terms; it's your account and your call. It keeps request rates low. If Garmin objects to the name, the name changes.

## License

MIT
