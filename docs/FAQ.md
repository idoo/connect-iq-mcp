# Sign-in, builds, upgrades and the rest

## Sign-in

`connect-iq-mcp auth login` opens a separate profile of a Chromium browser (Chrome, Brave or Edge; on macOS only vendor-signed copies), or a downloaded Chrome for Testing if you have none. Safari and Firefox can't be driven. Sign in once and tick Remember me.

The portal session lasts about 15 days and renews in the background in a headless browser. If renewal fails, tools return `auth_required`: run `connect-iq-mcp auth login` again.

If Garmin rate-limits sign-ins instead (Cloudflare 1015), renewal pauses for 90 minutes and tools return `rate_limited` with the time to wait. Don't sign in during that wait: it hits the same limit. A renewal that didn't extend the session pauses renewal for 12 hours. `connect-iq-mcp auth status` shows either pause and when it ends.

Agents can open the same window with the `auth_login` tool, one window at a time, with a 10 minute pause after a window closes without a sign-in. `connect-iq-mcp auth renew` while that window is open says so; close or finish it, then retry.

Where the cookie and tokens are kept, and what protects the browser profile: [SECURITY.md](../SECURITY.md#credentials).

## Profiles

Profiles keep separate Garmin accounts. Any name works without a config section: `connect-iq-mcp auth login --profile work`, then `connect-iq-mcp config install <client> --profile work`.

Settings (`allow_writes`, `allow_no_elicit`, `auto_renew`) go under `[profiles.work]` in `~/.config/connect-iq-mcp/config.toml` (`$XDG_CONFIG_HOME/connect-iq-mcp/` if set, `%AppData%\connect-iq-mcp\` on Windows), and config errors name that file. Adding a profile doesn't move anything off `default`; `default_profile` does. Sign-in hints carry the `--profile` they need.

Names are lowercase letters, digits, `-` and `_`, up to 63 characters.

## Builds and signing

Store builds must be signed with the key of your first upload. `config install <client> --key /abs/path/developer_key.der` puts it in the client entry's env as `CIQ_MCP_KEY`; add `--java-home /abs/jdk` if Java isn't found. Restart the client afterwards. Without a key, each build needs a key file inside the project. The key can live outside the repo and is only read.

`sdk_key_generate` is for a brand-new app, and in a git repo it refuses unless the key path is git-ignored.

`sdk_build device:"all"` writes `release/<project>-<version>.iq`. Long builds return a `build_id` to poll. Asking again with the same arguments while it runs gives the same `build_id`; a different build writing the same file gets `conflict`.

Devices can be named like people name them: "Fenix 7" becomes `fenix7`, and the result says so. A name that fits several devices gets the list of ids back instead of a guess.

Builds and the simulator need Java 17+. Connect IQ MCP runs the SDK's jar itself, never its `monkeyc`/`monkeydo` scripts.

Paths in `monkey.jungle` must be relative and stay inside the project root once symlinks are followed. `..` is fine; absolute, drive-letter and `~` paths are refused, even when they point inside. Jungles inside barrels are checked too. `sdk_build` and `sim_run` refuse before the compiler runs.

## The simulator

`sim_screenshot` and `sim_input` are macOS only. Running apps and unit tests (`sim_run`) works everywhere the SDK does.

`sim_stop` only stops the simulator this server started. One you or another agent started keeps running: the agent gets `conflict` and is told to ask you before calling `sim_stop` with `force:true`. Nothing but that instruction stops it from skipping the asking.

## First upload of a new app

Do it once by hand on [apps.garmin.com](https://apps.garmin.com). `store_app_create` exists, but it sits in the off-by-default `store_experimental` toolset and has never been run against the real portal, so you'd be its first tester. After that Connect IQ MCP takes over: `store_version_publish` ships every new version.

The key you sign that first upload with must sign every update forever. Back it up somewhere that isn't this laptop.

## CI

There's no unattended long-lived sign-in. Export the portal cookie and set it as a secret:

```sh
connect-iq-mcp auth export --portal | gh secret set CIQ_MCP_PORTAL_COOKIES
```

Then set `CIQ_MCP_PORTAL_COOKIES` in the job's env. It holds only the portal `session` cookie, which lasts about 15 days and is never renewed.

## Upgrade

- Homebrew: `brew upgrade connect-iq-mcp`.
- install.sh: run the same curl line again. It replaces the binary in `~/.local/bin`.
- `go install`: run it again with `@latest`.
- Claude Desktop: double-click the new `.mcpb`.

Restart your client afterwards. On macOS, if tools say `auth_required` after an upgrade, run `connect-iq-mcp auth login` once more: the sign-in browser encrypts its cookies with its own Safe Storage keychain key and can't read profiles made by older builds. Older builds also allowed capital letters in profile names. A config with a profile called `Work` no longer loads: rename it to `work` and sign in again under the new name.

## Uninstall

```sh
connect-iq-mcp auth logout                 # and once per other profile: --profile <name>
claude mcp remove connect-iq-mcp -s user   # or delete connect-iq-mcp from your client's config
brew uninstall connect-iq-mcp              # or delete the binary
```

`auth logout` deletes that profile's keychain items, credentials file and sign-in browser profile. The config and state folders (`~/.config/connect-iq-mcp`, `~/.local/state/connect-iq-mcp`) stay; delete them if you want nothing left.

## Troubleshooting

Start with `connect-iq-mcp doctor`. It says which check failed and usually how to fix it.

- **`auth_required`.** The session ran out and renewal failed. `connect-iq-mcp auth login`.
- **`rate_limited`.** Garmin's sign-in limit. Wait until the time it gives; signing in during the wait hits the same limit.
- **Locked keychain.** Unlock it, or set `CIQ_MCP_KEYRING=off` to use the 0600 file. `auth status` says which store is in use and why.
- **The client doesn't see the tools.** Restart the client. Store write tools are hidden until writes are on: ask the agent to enable writes, or set `CIQ_MCP_ALLOW_WRITES=1` in the client's env.
- **Build can't find the key or Java.** The server reads the client's env, not your shell's. Run `config install` again with `--key` and `--java-home`.
- **File tools say `unavailable`.** Your client sent no local folder as an MCP root. Pass `--root` or set `CIQ_MCP_ROOT`.
