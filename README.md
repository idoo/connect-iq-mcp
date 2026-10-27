# Connect IQ MCP

MCP server for Garmin Connect IQ developers. Your Connect IQ Store apps, reviews, SDK builds and the simulator, from Claude, Codex, Cursor or any other MCP client.

## Install

Paste this into your coding agent:

```
Set up Connect IQ MCP for me: https://github.com/idoo/connect-iq-mcp. Install it, run `connect-iq-mcp auth login` so I can sign in, then `connect-iq-mcp config install <your client>`.
```

Or by hand:

```sh
brew install idoo/tap/connect-iq-mcp
# or: curl -fsSL https://raw.githubusercontent.com/idoo/connect-iq-mcp/main/install.sh | sh
# or: go install github.com/idoo/connect-iq-mcp/cmd/connect-iq-mcp@latest   (fetches Go 1.26.10 if yours is older)

connect-iq-mcp auth login
connect-iq-mcp config install claude-code   # or claude-desktop, codex, cursor, vscode, windsurf
connect-iq-mcp doctor
```

Builds and the simulator need the Connect IQ SDK. Install Garmin's [SDK Manager](https://developer.garmin.com/connect-iq/sdk/) first and download an SDK and your devices in it; `connect-iq-mcp doctor` says whether it found the SDK.

`connect-iq-mcp config install vscode` writes the user mcp.json (the one "MCP: Open User Configuration" opens) of every VS Code it finds: Code, Code - OSS, VSCodium, and their Flatpaks on Linux. There is no Docker image: sign-in needs a browser, and builds need your SDK and simulator.

- macOS: after upgrading from an older version, run `connect-iq-mcp auth login` once more. The sign-in browser now encrypts its cookies with its own Safe Storage keychain key and can't read the old ones. Chrome for Testing still uses Chromium's fixed mock key.
- Linux over SSH: the keyring is usually there but locked, and that's an error, not a reason to fall back to the file. Unlock it (`gnome-keyring-daemon --unlock`) or set `CIQ_MCP_KEYRING=off`. With no D-Bus session at all, the 0600 file is used.
- Ubuntu 24.04: AppArmor won't let the downloaded Chrome for Testing start, so install Chrome, Brave or Edge from their .deb. On arm64 Linux use the distro's `chromium` package (not the snap) or Brave. `connect-iq-mcp doctor` warns when Chrome for Testing is blocked like that or is missing shared libraries.
- Linux: the simulator is a GUI app. With no desktop, start the server under `xvfb-run`.
- Windows: the installer works in Git Bash, or grab the zip from [releases](https://github.com/idoo/connect-iq-mcp/releases). The `.mcpb` bundle is x64 only: Windows 11 on Arm runs it emulated, and on Windows 10 on Arm you take the arm64 zip and run `connect-iq-mcp config install claude-desktop`, which also finds the MSIX build of Claude Desktop.
- WSL: `config install claude-desktop` and `vscode` write nothing and print a `wsl.exe` snippet for the Windows app's config.

## Sign-in

`connect-iq-mcp auth login` opens a separate profile of a Chromium browser (Chrome, Brave, Edge; on macOS only vendor-signed copies), or a downloaded Chrome for Testing if you have none. Safari and Firefox can't be driven. Sign in once and tick Remember me. The portal session (about 15 days) then renews in the background in a headless browser. If renewal fails, tools return `auth_required`: run `connect-iq-mcp auth login` again. If Garmin rate-limits sign-ins instead (Cloudflare 1015), renewal pauses for 90 minutes and tools return `rate_limited` with the time to wait until. Don't sign in during that wait; it hits the same limit. A renewal that didn't extend the session pauses it for 12 hours. `connect-iq-mcp auth status` shows either pause and when it ends.

No display (SSH, a server)? `connect-iq-mcp auth login --paste` takes the Cookie header, or the Copy as cURL text, of any apps.garmin.com request in a browser where you're signed in (DevTools → Network).

Agents can open the same window with the `auth_login` tool, one window at a time, with a 10 minute pause after a window closes without a sign-in. `connect-iq-mcp auth renew` while that window is open says so; close or finish it, then retry.

Garmin Connect health data is a separate opt-in: `connect-iq-mcp auth login --connect`. Until then `doctor` lists it as not enabled, which is fine.

The portal session cookie and Connect tokens go to the OS keychain. Without one (no Secret Service or no D-Bus session on Linux, or `CIQ_MCP_KEYRING=off`) they go to a 0600 file instead, DPAPI-encrypted on Windows. A keychain that is there but locked is an error until you unlock it or set that variable. `connect-iq-mcp auth status` says which and why. The browser profile in `~/.local/state/connect-iq-mcp/browser/` holds the long-lived Garmin session; how its cookies are protected is in [SECURITY.md](SECURITY.md).

Profiles keep separate Garmin accounts. Any name works without a config section: `connect-iq-mcp auth login --profile work`, then `connect-iq-mcp config install <client> --profile work`. Settings (`allow_writes`, `allow_no_elicit`, `auto_renew`) go under `[profiles.work]` in `~/.config/connect-iq-mcp/config.toml` (`$XDG_CONFIG_HOME/connect-iq-mcp/` if set, `%AppData%\connect-iq-mcp\` on Windows), and config errors name that file. Adding a profile doesn't move anything off `default`; `default_profile` does. Sign-in hints carry the `--profile` they need.

Names are lowercase letters, digits, `-` and `_`, up to 63 characters. A config with a profile called `Work` no longer loads: rename it to `work` and sign in again under the new name.

## Builds and signing

Store builds must be signed with the key of your first upload. `connect-iq-mcp config install <client> --key /abs/path/developer_key.der` puts it in the client entry's env as `CIQ_MCP_KEY`; add `--java-home /abs/jdk` if Java isn't found. Both paths must be absolute. Restart the client afterwards. By hand it's the same `env` block: a shell export doesn't reach a client started from the Dock or Start menu. `doctor` in your shell can't see the client's env, so it shows an unset key as `info`, not an error. Without a key each build needs a key file inside the project. The key can live outside the repo and is only read. `sdk_key_generate` is for a brand-new app, and in a git repo it refuses unless the key path is git-ignored. `sdk_build device:"all"` writes `release/<project>-<version>.iq`; long builds return a `build_id` to poll. Asking again with the same arguments while it runs gives the same `build_id`; a different build writing the same file gets `conflict`.

Devices can be named like people name them: "Fenix 7" becomes `fenix7`, and the result says so. A name that fits several devices gets the list of ids back instead of a guess.

Builds and the simulator need Java 17+ (`JAVA_HOME` as an absolute path, set in that same client env, or `java` on PATH). Connect IQ MCP runs the SDK's jar itself, never its `monkeyc`/`monkeydo` scripts.

Paths in `monkey.jungle` must be relative and stay inside the project root once symlinks are followed. `..` is fine; absolute, drive-letter and `~` paths are refused, even when they point inside. Jungles inside barrels are checked too. `sdk_build` and `sim_run` refuse before the compiler runs.

`sim_stop` only stops the simulator this server started. One you or another agent started keeps running: the agent gets `conflict` and is told to ask you before calling `sim_stop` with `force:true`. Nothing but that instruction stops it from skipping the asking.

## First upload of a new app

Do it once by hand on [apps.garmin.com](https://apps.garmin.com). `store_app_create` exists, but it sits in the off-by-default `store_experimental` toolset and has never been run against the real portal, so you'd be its first tester. After that Connect IQ MCP takes over: `store_version_publish` ships every new version. The key you sign that first upload with must sign every update forever, so back it up somewhere that isn't this laptop.

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

Pick with `CIQ_MCP_TOOLSETS` (`all`, `+connect`). Every tool: [docs/TOOLS.md](docs/TOOLS.md).

## Safety

- Read-only until you say otherwise: ask the agent to enable writes, or set `CIQ_MCP_ALLOW_WRITES=1` (or `--allow-writes`). SDK builds and the simulator work either way.
- Anything public (publishing a version, editing the listing, replying to a review) shows you a preview and waits for your yes. Clients that can't show that dialog need writes on and `allow_no_elicit = true` in the profile, and then those actions go through on the agent's confirm alone. Nobody asks you.
- Once a version is posted, `store_version_publish` says `published: true` even if saving what's new failed afterwards. The warning tells the agent to re-run it with the same version.
- Review text is written by strangers. The server treats it as untrusted input, and so should your agent.

## CI

No unattended long-lived sign-in. `connect-iq-mcp auth export --portal | gh secret set CIQ_MCP_PORTAL_COOKIES`, then set that env var in the job. It holds only the portal `session` cookie, which lasts about 15 days and is never renewed.

## Uninstall

```sh
connect-iq-mcp auth logout                 # and once per other profile: --profile <name>
claude mcp remove connect-iq-mcp -s user   # or delete connect-iq-mcp from your client's config
brew uninstall connect-iq-mcp              # or delete the binary
```

`auth logout` deletes that profile's keychain items, credentials file and sign-in browser profile. The config and state folders (`~/.config/connect-iq-mcp`, `~/.local/state/connect-iq-mcp`) stay; delete them if you want nothing left.

## Unofficial

Connect IQ MCP is not made, endorsed or supported by Garmin. It uses the developer portal's private web API and Garmin's sign-in, which can change and break it any day. Renewal runs a headless browser that doesn't announce itself as automated. Automated access may be against the portal's terms; it's your account and your call. It keeps request rates low. If Garmin objects to the name, the name changes.

## License

MIT
