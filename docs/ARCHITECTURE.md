# Architecture

One binary, one Go module. The MCP server sits in the middle, the tools are plain functions behind a small contract, and everything that touches Garmin, the OS or the SDK is an adapter.

## Packages

- `cmd/garmin-mcp`: the CLI (`serve`, `auth`, `config`, `doctor`).
- `internal/app`: wires config, credentials, HTTP and state into a `tool.Env` and runs the server.
- `internal/mcpserver`: builds the MCP server from the registry: toolsets, read-only hiding, approvals, transports.
- `internal/tool`: the contract every tool is written against: `Meta`, registry, `Env`, results, errors, confirm gates.
- `internal/tool/tooltest`: in-memory fakes of every `Env` dependency, plus `Call`, `MustOK`, `MustFail`, `AssertGolden`.
- `internal/tools/{core,store,sdk,simulator,connect}`: one package per toolset; tools register in `init()`.
- `internal/tools/all`: blank-imports every toolset package.
- `internal/tools/docsgen`: writes `docs/TOOLS.md` and `docs/tools.json` from the registry.
- `internal/auth`: credential types and the `auth.Store` interface.
- `internal/credstore`: implements `auth.Store`: keychain or 0600 file, locked refresh, silent renewal, the `auth` CLI.
- `internal/browser`: drives the dedicated Chromium profile for sign-in and renewal.
- `internal/portal`: client for the apps.garmin.com developer portal's internal API.
- `internal/connectapi`: read-only Garmin Connect client.
- `internal/ciqsdk`: finds SDKs, devices and Java, parses manifests, runs monkeyc, handles keys. `ciqsdktest` is test-only.
- `internal/sim`: the simulator process, monkeydo sessions, input, screenshots, crash logs.
- `internal/config`: profiles, project pins, env and flags.
- `internal/state`: cursors and upload resume records in the state dir.
- `internal/redact`: strips secrets from text before logs and tool output.
- `internal/flock`, `internal/httpx`: leaf helpers (file locks, Retry-After parsing).

## Who imports whom

From `go list -deps`, roughly bottom to top:

- `auth`, `ciqsdk`, `flock`, `httpx` import nothing internal. `tool` imports only `auth`.
- `state` and `redact` import `tool`; `config` imports `state` and `tool`; `browser` imports `state`.
- `portal` and `connectapi` import `auth` and `httpx`. `sim` imports `ciqsdk`, `flock` and `state`.
- `credstore` is the heavy one: `auth`, `browser`, `config`, `flock`, `portal`, `state`, `tool`.
- `tools/*` import `tool` plus the adapters they need; `tools/simulator` also imports `tools/sdk`.
- `mcpserver` imports `config`, `redact`, `tool`. `app` imports `mcpserver`, `credstore`, `sim`, `config`, `state`, `tool`.

depguard in `.golangci.yml` enforces three of these: only `mcpserver` and `docsgen` import the MCP SDK; only those two and `internal/tool` import jsonschema-go; `internal/tools/**` (except `docsgen`) never imports `app` or `mcpserver`. The rest is convention, so check `go list -deps` before adding an import.

## Where state lives

- Keychain, service `garmin-mcp`: the portal session cookie and Connect tokens, in chunks.
- State dir (`~/.local/state/garmin-mcp`, `$XDG_STATE_HOME/garmin-mcp`, `%LocalAppData%\garmin-mcp`): `credentials/<profile>.json` when there's no keychain, `locks/`, `browser/<profile>` (the sign-in profile), `browser-bin` (downloaded Chrome for Testing), `cursors/`, `uploads/`, `sim.lock`, `sim-launch.log`.
- Config: `~/.config/garmin-mcp/config.toml` (`$XDG_CONFIG_HOME`, `%AppData%` on Windows), plus an optional `.garmin-mcp.toml` pin in the project, which can only narrow permissions.
- The client's own config holds the env: `GARMIN_MCP_KEY`, `JAVA_HOME`, `GARMIN_MCP_PROFILE`.

## Errors

A failed tool returns `tool.ErrorBody`: `error` (a stable code), `message`, `hint`, and `next` steps the agent can call. Codes come from `*tool.Error`. Shared ones are in `internal/tool/errors.go` (`invalid_arguments`, `writes_disabled`, `needs_confirm`, `preview_required`, `declined`, `auth_required`, `rate_limited`, `outcome_unknown`, `unavailable`, `human_approval_required`, `internal_error`, `write_guard`). Toolsets add their own: `conflict` (`sdk_build` writing the same file, `sim_stop` on a simulator this server didn't start), `sim_not_ready`, `sim_busy`, `sdk_mismatch`, `build_failed` and so on. Upstream HTTP errors become `api_error` with the portal's code inside. Anything unclassified is plain `error`, which agents read as a server fault, so classify new failures.

## Docs

`make docs` runs `go run ./internal/tools/docsgen` and fails on a diff. `TestGeneratedDocsAreFresh` checks the same in `go test`. Tool descriptions live in the code; edit them there, never in `docs/TOOLS.md`.

## Tests

Tests sit next to the code (`package x`, plus a few external `tool_test` files in `internal/tool`). Tool tests run handlers directly through `tooltest` fakes, and nothing writes to a real Garmin account. Platform-specific code and tests use `_darwin`, `_unix`, `_windows` files and build tags. `ciqsdktest.Key` shares one 4096-bit key per test binary. `go test ./... -update` rewrites goldens. `TestInstructionsBudget` in `mcpserver` caps the server instructions.
