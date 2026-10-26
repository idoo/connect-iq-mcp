# Garmin MCP

MCP server for Garmin Connect IQ developers. Your Connect IQ Store apps, reviews, SDK builds and the simulator, from Claude, Codex, Cursor or any other MCP client.

## Install

Paste this into your coding agent:

```
Install Garmin MCP from https://github.com/idoo/garmin-mcp (brew install idoo/tap/garmin-mcp, or the install.sh one-liner from its README), run `garmin-mcp auth login` and let me sign in, then `garmin-mcp config install` for yourself.
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

`garmin-mcp auth login` opens a separate browser profile (your default Chromium browser, else Chrome for Testing). Sign in once and tick Remember me. After that the portal session (about 15 days) renews in the background, headless, for as long as Garmin keeps the Remember me cookie.

Garmin Connect health data is a separate opt-in: `garmin-mcp auth login --connect`.

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
| sdk | monkeyc builds, devices, manifests; `sdk_build{device:"all"}` makes the store .iq | on |
| simulator | run apps in the simulator, screenshots | on |
| store_experimental | portal endpoints never tried live: app create/delete, screenshots, review replies | off |
| connect | Garmin Connect data, needs `auth login --connect` | off |

Pick with `GARMIN_MCP_TOOLSETS` (`all`, `+connect`). Every tool: [docs/TOOLS.md](docs/TOOLS.md).

## Safety

- Read-only by default. Local SDK and simulator tools always work; portal writes need `--allow-writes`.
- Anything public (publishing a version, editing the listing, replying to a review) shows you a preview and waits for your yes.
- Review text is written by strangers. The server treats it as untrusted input, and so should your agent.

## CI

No unattended long-lived sign-in. Paste portal cookies into `GARMIN_MCP_PORTAL_COOKIES`; they last about 15 days and are not renewed.

## Unofficial

Garmin MCP is not made, endorsed or supported by Garmin. It uses the developer portal's private web API and Garmin's sign-in, which can change and break it any day. Automated access may be against the portal's terms; it's your account and your call. It keeps request rates low. If Garmin objects to the name, the name changes.

## License

MIT
