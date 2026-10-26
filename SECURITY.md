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
- **Local files.** Builds and screenshots are written inside the project root (`GARMIN_MCP_ROOT`, default the git root); paths that escape it are rejected.

## Credentials

garmin-mcp keeps developer-portal cookies and the Garmin Connect token in the OS keychain (service `garmin-mcp`). The config file never holds secrets. Only `garmin-mcp auth` can change credentials. Cookies and tokens are sent only to Garmin hosts and redacted from everything garmin-mcp prints.

No telemetry. garmin-mcp talks to apps.garmin.com, Garmin Connect (only with the opt-in connect toolset) and your local Connect IQ SDK; `garmin-mcp config install claude-code` runs your local `claude`.
