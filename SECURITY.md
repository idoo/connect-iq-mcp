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
- Setups that switch safety off on purpose: auto-approve everything, `allow_no_elicit`, `--no-confirm` with `GARMIN_MCP_CI=1`.
- Processes running as you reading your keychain.
- Garmin's own portal and API behaviour.

## Threat model, short

- **Prompt injection.** Review text, store listings and repo files can carry instructions. Tool results mark them as untrusted data; the real guard is that nothing public happens without a preview you approve.
- **Approval integrity.** A preview is bound to the exact action it shows. Changing the arguments after approval, or approving a different preview, must fail.
- **SDK paths.** The model can only pick an SDK the SDK Manager installed, never an arbitrary path; Java is only taken from absolute locations, so a repo cannot plant its own.
- **Local files.** Builds and screenshots are written inside the project root (`--root` or `GARMIN_MCP_ROOT`, else the client's MCP roots, else the git root); paths that escape it are rejected.

## Credentials

Garmin MCP keeps the developer-portal `session` cookie (nothing else from the apps.garmin.com cookie jar) and the Garmin Connect token in the OS keychain (service `garmin-mcp`, split into chunks). Only when there is no keychain (headless Linux without Secret Service, or `GARMIN_MCP_KEYRING=off`) do they go to a 0600 file in the state directory (`~/.local/state/garmin-mcp/credentials`, `%LocalAppData%\garmin-mcp` on Windows, where the file is DPAPI-encrypted). `garmin-mcp auth status` and `doctor` say which store is used and why. The config file never holds secrets. Credentials change only through `garmin-mcp auth`, the `auth_login` tool (you still sign in yourself in the browser) and automatic renewal. Cookies and tokens are sent only to Garmin hosts and redacted from everything Garmin MCP prints.

`GARMIN_MCP_KEY` names your Connect IQ signing key. It is the only path tools may use outside the project root, and it is only read.

HTTP mode (`serve --http`) needs a bearer token of at least 32 characters (`GARMIN_MCP_HTTP_TOKEN`, or one is generated and printed). Wrong tokens get a growing per-IP backoff.

## Accepted risks

- The sign-in browser profile (`browser/<profile>` in the state directory) holds the long-lived Garmin session. Only file permissions protect it. Password saving is off in it, and on macOS it is excluded from Time Machine.
- During sign-in and renewal the browser's DevTools port is open on 127.0.0.1. On a shared multi-user machine another local user could reach it.

No telemetry. Garmin MCP talks to apps.garmin.com, sso.garmin.com for sign-in and renewal, Garmin Connect (only with the opt-in connect toolset) and your local Connect IQ SDK. If no Chromium browser is installed, `auth login` downloads Chrome for Testing from storage.googleapis.com. `garmin-mcp config install claude-code` runs your local `claude`.
