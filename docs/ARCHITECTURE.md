# Architecture

One binary, one Go module. The MCP server sits in the middle, the tools are plain functions behind a small contract, and everything that touches Garmin, the OS or the SDK is an adapter.

## Packages

- `cmd/connect-iq-mcp`: the CLI (`serve`, `auth`, `config`, `doctor`).
- `internal/app`: wires config, credentials, HTTP and state into a `tool.Env` and runs the server.
- `internal/mcpserver`: builds the MCP server from the registry: toolsets, read-only hiding, approvals, transports.
- `internal/tool`: the contract every tool is written against: `Meta`, registry, `Env`, results, errors, confirm gates.
- `internal/tool/tooltest`: in-memory fakes of every `Env` dependency, plus `Call`, `MustOK`, `MustFail`, `AssertGolden`.
- `internal/tools/{core,store,sdk,simulator,connect}`: one package per toolset; tools register in `init()`.
- `internal/tools/all`: blank-imports every toolset package.
- `internal/tools/docsgen`: writes `docs/TOOLS.md` and `docs/tools.json` from the registry.
- `internal/auth`: credential types, the `auth.Store` interface and the sign-in sentinels (`ErrAuthRequired`, `ErrRateLimited`, `ErrInteractionRequired`).
- `internal/auth/login`: interactive sign-in orchestration (`auth.Loginer`) and `Chromium`, the `credstore.Browser` used for sign-in and silent renewal. `login.Open` is the store `app` and `doctor` use.
- `internal/credstore`: implements `auth.Store`: keychain or 0600 file, locks, locked refresh, renewal policy and stamps. It runs browser sign-ins only through the injected `credstore.Browser` and never imports `internal/browser` (depguard).
- `internal/browser`: drives the dedicated Chromium profile for sign-in and renewal.
- `cmd/connect-iq-mcp`: also holds the `connect-iq-mcp auth` CLI (`auth.go`).
- `internal/portal`: client for the apps.garmin.com developer portal's internal API.
- `internal/connectapi`: read-only Garmin Connect client.
- `internal/ciqsdk`: finds SDKs, devices and Java, parses manifests, runs monkeyc, handles keys. `ciqsdktest` is test-only.
- `internal/sim`: the simulator process, monkeydo sessions, input, screenshots, crash logs.
- `internal/config`: profiles, project pins, env and flags.
- `internal/state`: cursors and upload resume records in the state dir.
- `internal/redact`: strips secrets from text before logs and tool output.
- `internal/flock`, `internal/httpx`, `internal/upstream`: leaf helpers (file locks, Retry-After parsing, the shared `upstream.HTTPError`).

## Who imports whom

From `go list -deps`, roughly bottom to top:

- `auth`, `ciqsdk`, `flock`, `httpx`, `upstream` import nothing internal. `tool` imports only `auth` and `upstream`.
- `state` and `redact` import `tool`; `config` imports `state` and `tool`; `browser` imports `state`.
- `portal` and `connectapi` import `auth`, `httpx` and `upstream`. `sim` imports `ciqsdk`, `flock` and `state`.
- `credstore` is the heavy one: `auth`, `browser`, `config`, `flock`, `portal`, `state`, `tool`.
- `tools/*` import `tool` plus the adapters they need; `tools/simulator` also imports `tools/sdk`.
- `mcpserver` imports `config`, `redact`, `tool`. `app` imports `mcpserver`, `credstore`, `sim`, `config`, `state`, `tool`.

depguard in `.golangci.yml` enforces three of these: only `mcpserver` and `docsgen` import the MCP SDK; only those two and `internal/tool` import jsonschema-go; `internal/tools/**` (except `docsgen`) never imports `app` or `mcpserver`. The rest is convention, so check `go list -deps` before adding an import.

## Where state lives

- Keychain, service `connect-iq-mcp`: the portal session cookie and Connect tokens, in chunks.
- State dir (`~/.local/state/connect-iq-mcp`, `$XDG_STATE_HOME/connect-iq-mcp`, `%LocalAppData%\connect-iq-mcp`): `credentials/<profile>.json` when there's no keychain, `locks/`, `browser/<profile>` (the sign-in profile), `browser-bin` (downloaded Chrome for Testing), `cursors/`, `uploads/`, `sim.lock`, `sim-launch.log`.
- Config: `~/.config/connect-iq-mcp/config.toml` (`$XDG_CONFIG_HOME`, `%AppData%` on Windows), plus an optional `.connect-iq-mcp.toml` pin in the project, which can only narrow permissions.
- The client's own config holds the env: `CIQ_MCP_KEY`, `JAVA_HOME`, `CIQ_MCP_PROFILE`.
- In-process, per server: `tool.Env.Shared` holds the store app cache, the sdk build jobs and the simulator sessions (`sim.Manager`, which owns launched simulators). Tool packages own their state types and reach this server's single value through `tool.SharedValue`; none of it lives in package-level variables.
- `app.Serve` creates that `Shared` once and every session uses it, so all HTTP sessions share one cache, one build-job registry and one simulator manager; on return it calls `sim.Manager.KillAll`. `tooltest.New` makes a fresh one per test kit; a hand-built `Env` without one gets call-private state.

## Errors

A failed tool returns `tool.ErrorBody`: `error` (a stable code), `message`, `hint`, and `next` steps the agent can call. Codes come from `*tool.Error`. Shared ones are in `internal/tool/errors.go` (`invalid_arguments`, `writes_disabled`, `needs_confirm`, `preview_required`, `declined`, `auth_required`, `rate_limited`, `outcome_unknown`, `unavailable`, `human_approval_required`, `internal_error`, `write_guard`). Toolsets add their own: `conflict` (`sdk_build` writing the same file, `sim_stop` on a simulator this server didn't start), `sim_not_ready`, `sim_busy`, `sdk_mismatch`, `build_failed` and so on. Every Garmin HTTP client returns a non-auth, non-2xx answer as one `*upstream.HTTPError` (status, code, message, rate-limit flags). `tool.FromUpstream(err, hints)` is the one mapper: it turns the auth sentinels into `auth_required`/`rate_limited` and an `HTTPError` into `api_error` with the upstream code inside. Each toolset passes only its `tool.UpstreamHints` (hint texts, its client's sentinels). Anything unclassified is plain `error`, which agents read as a server fault, so classify new failures.

## Docs

`make docs` runs `go run ./internal/tools/docsgen` and fails on a diff. `TestGeneratedDocsAreFresh` checks the same in `go test`. Tool descriptions live in the code; edit them there, never in `docs/TOOLS.md`.

## Tests

Tests sit next to the code (`package x`, plus a few external `tool_test` files in `internal/tool`). Tool tests run handlers directly through `tooltest` fakes, and nothing writes to a real Garmin account. Platform-specific code and tests use `_darwin`, `_unix`, `_windows` files and build tags. `ciqsdktest.Key` shares one 4096-bit key per test binary. `go test ./... -update` rewrites goldens. `TestInstructionsBudget` in `mcpserver` caps the server instructions.
