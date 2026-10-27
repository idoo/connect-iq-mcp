# Security policy

Only the latest release gets fixes, so upgrade before reporting.

## Reporting

Use [private vulnerability reporting](https://github.com/idoo/garmin-mcp/security/advisories/new), not a public issue. It's one person on the other end, so expect a reply within a few days and a fix or plan within two weeks for anything serious. You get credit in the advisory unless you'd rather not.

## In scope

- Getting a write, public or destructive action past the preview and approval, via tool input, store or review text, or a repo file.
- Cookies, tokens or signed URLs leaking into results, errors or logs.
- Portal cookies going anywhere but apps.garmin.com, or the Connect token anywhere but Garmin Connect.
- Getting around the URL policy, cursor checks, file confinement, or the HTTP token, Origin and loopback checks.
- Broken release artifacts, checksums or provenance.

## Out of scope

- A model fooled by injected text *and* a human clicking yes.
- Setups that switch safety off on purpose: auto-approve everything, `allow_no_elicit` (public actions then run on the agent's confirm, with no human yes), `--no-confirm` with `GARMIN_MCP_CI=1`.
- Processes running as you reading your keychain.
- Garmin's own portal and API behaviour.

## Threat model, short

- **Prompt injection.** Review text, store listings and repo files can carry instructions. Tool results mark them as untrusted data; the real guard is that nothing public happens without a preview you approve.
- **Approval integrity.** A preview is bound to the exact action it shows. Changing the arguments after approval, or approving a different preview, must fail.
- **SDK paths.** The model can only pick an SDK the SDK Manager installed, never an arbitrary path; Java is only taken from absolute locations, so a repo cannot plant its own. Garmin MCP runs `java -classpath monkeybrains.jar` itself and never the SDK's `monkeyc`/`monkeydo` wrapper scripts.
- **Jungles.** The check runs inside the build itself, so it covers `sdk_build`, `sim_run` and `sim_run` with `test`, before the compiler starts. Values are read the way monkeyc reads them: one pair of quotes stripped, `\` as a separator, `$(...)` expanded. Absolute, drive-letter, UNC and `~` values are refused even when they point inside the root. Everything else may use `..` but must resolve, through any symlinks (`link/..` follows the link), inside the project root. Named and default paths (`source*`, `resources*`, `manifest.xml`) are scanned for symlinks leading out. Jungles inside a `.barrel` may not use `..`, and an unreadable barrel is refused. The jungle file itself must be inside the root, with no `;` or `$(` in its path.
- **Git.** `sdk_key_generate` asks git whether the key is ignored with `core.fsmonitor` off, `core.hooksPath` set to the null device, `check-ignore --no-index`, no `GIT_*` variables and no system config (`GIT_CONFIG_NOSYSTEM=1`), so a cloned repo's `.git/config` can't run commands. Your global git config still applies.
- **Local files.** Builds and screenshots are written inside the project root (`--root` or `GARMIN_MCP_ROOT`, else the client's MCP roots, else the git root); paths that escape it are rejected. The git root is used only when the client sends no roots: if it sends roots and none is a local folder (a remote or virtual workspace, a deleted folder), or listing them fails, file tools return `unavailable` and ask for `--root`.
- **Uploads.** Screenshots, icons and `.iq` files must be regular files inside the root, with no symlink anywhere below it, and must start with the bytes their extension promises (PNG, JPEG, GIF, ZIP for `.iq`). The approval preview shows the root-relative path.

## Credentials

Garmin MCP keeps the developer-portal `session` cookie (nothing else from the apps.garmin.com cookie jar) and the Garmin Connect token in the OS keychain (service `garmin-mcp`, split into chunks). Only when there is no keychain do they go to a 0600 file in the state directory (`~/.local/state/garmin-mcp/credentials`, `%LocalAppData%\garmin-mcp` on Windows, where the file is DPAPI-encrypted). "No keychain" means no backend, no D-Bus session on Linux (Garmin MCP doesn't start one), a dead bus socket, no Windows logon session, or `GARMIN_MCP_KEYRING=off`. A keychain that exists but is locked, or can't show its unlock prompt within 5 seconds, is an error and never falls back to the file: unlock it (on Linux, log in to the desktop or run `gnome-keyring-daemon --unlock`) or set `GARMIN_MCP_KEYRING=off`. `garmin-mcp auth status` and `doctor` say which store is used and why, and `auth status` exits non-zero when stored credentials are locked or unreadable. `auth logout` clears the keychain items even with `GARMIN_MCP_KEYRING=off`, along with the credentials file and the sign-in browser profile. The config file never holds secrets. Credentials change only through `garmin-mcp auth`, the `auth_login` tool (you still sign in yourself in the browser) and automatic renewal. Cookies and tokens are sent only to Garmin hosts and redacted from everything Garmin MCP prints.

While a keychain write is in flight, a `<profile>#pending` item records its chunk generation, so the next save or `auth logout` removes chunks left behind by a crash or timeout. Cleanup leaves a generation recorded less than 30 seconds ago alone, in case another garmin-mcp process with a different state directory is still writing it.

`GARMIN_MCP_KEY` names your Connect IQ signing key. It is the only path tools may use outside the project root, and it is only read.

HTTP mode (`serve --http`) needs a bearer token of at least 32 characters (`GARMIN_MCP_HTTP_TOKEN`, or one is generated and printed). The token is checked first, so the right one always gets in. Only requests with a wrong `Authorization` header count as failures and get a growing backoff, per IP (per /64 for IPv6); a missing header is a plain 401. Loopback peers are never throttled. The failure table has a fixed size and drops the oldest entries.

## Accepted risks

- The sign-in browser profile (`browser/<profile>` in the state directory) holds the long-lived Garmin session. How its cookies are protected depends on the platform:
  - macOS with Chrome, Brave or Edge: encrypted with the browser's own "<Browser> Safe Storage" keychain key. Another process needs keychain access to decrypt them. Profiles from older versions can't be read with that key, so you sign in once more after upgrading.
  - macOS with Chrome for Testing: Chromium's fixed mock-keychain key, so only file permissions (0700) protect it. Install a vendor-signed browser if that matters to you.
  - Linux: `--password-store=basic`, a fixed key (a server has no unlocked keyring), so only file permissions protect it.
  - Windows: the browser's DPAPI-wrapped key, tied to your Windows account.

  On all platforms any process running as you can drive or copy the profile. Password saving is off in it, and on macOS it is excluded from Time Machine.
- During sign-in and renewal the browser's DevTools port is open on 127.0.0.1. On a shared multi-user machine another local user could reach it.

No telemetry. Garmin MCP talks to apps.garmin.com, sso.garmin.com for sign-in and renewal, Garmin Connect (only with the opt-in connect toolset) and your local Connect IQ SDK. If no Chromium browser is installed, `auth login` downloads Chrome for Testing from storage.googleapis.com. `garmin-mcp config install claude-code` runs your local `claude`.
