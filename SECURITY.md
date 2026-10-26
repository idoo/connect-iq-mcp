# Security policy

Only the latest release gets fixes, so upgrade before reporting.

## Reporting

Use [private vulnerability reporting](https://github.com/idoo/ciq-mcp/security/advisories/new), not a public issue. It's one person on the other end, so expect a reply within a few days and a fix or plan within two weeks for anything serious. You get credit in the advisory unless you'd rather not.

## In scope

- Getting a write, public or destructive action past the preview and approval, via tool input, store or review text, or a repo file.
- Cookies, tokens or signed URLs leaking into results, errors or logs.
- Portal cookies going anywhere but apps.garmin.com, or the Connect token anywhere but Garmin Connect.
- Getting around the URL policy, cursor checks, file confinement, or the HTTP token, Origin and loopback checks.
- Broken release artifacts, checksums or provenance.

## Out of scope

- A model fooled by injected text *and* a human clicking yes.
- Setups that switch safety off on purpose: auto-approve everything, `allow_no_elicit`, `--no-confirm` with `CIQ_MCP_CI=1`.
- Processes running as you reading your keychain.
- Garmin's own portal and API behaviour.

## Credentials

ciq-mcp keeps developer-portal cookies and the Garmin Connect token in the OS keychain (service `ciq-mcp`). The config file never holds secrets. Only `ciq-mcp auth` can change credentials. Cookies and tokens are sent only to Garmin hosts and redacted from everything ciq-mcp prints.

No telemetry. ciq-mcp talks to apps.garmin.com, Garmin Connect (only with the opt-in connect toolset) and your local Connect IQ SDK; `ciq-mcp config install claude-code` runs your local `claude`.
