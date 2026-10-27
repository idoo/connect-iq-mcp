# Safety

Connect IQ MCP holds a Garmin session that can publish your apps to everyone with a watch, rewrite your store listing and answer reviews in public. This page says who we assume is out to get you and what stops them.

## Who we worry about

- **Text from strangers.** Reviews, store listings, device nicknames and activity names reach the model as written.
- **An eager model.** Wrong app, wrong version, or a "cleanup" that deletes your screenshots.
- **A sketchy repo.** A cloned `.connect-iq-mcp.toml` or `monkey.jungle` pointing your agent at your writable profile, your key or files outside the project.
- **Local neighbours.** Other processes, another agent's simulator, or a web page poking at a local HTTP server.
- **The supply chain.** Dependencies, Actions, release artifacts.

## What stops them

**Read-only by default.** Store write tools aren't listed until you turn writes on: `--allow-writes`, `CIQ_MCP_ALLOW_WRITES=1` or `allow_writes` in a profile. The agent can ask for writes mid-session through the `toolsets` tool, and that asks you first. Local work (builds, manifest edits, the simulator) works either way. Below the tools, the HTTP client refuses every non-GET request that doesn't come from a write tool.

**Preview first.** A write call without `confirm:true` only returns a preview and changes nothing. The confirm call must match a preview of the same arguments from the last 15 minutes, and each preview works once. Publishing also needs the `iq_sha256` from its preview, so a different `.iq` can't be swapped in. Every result reports `_meta.garmin.approved_by`: `human`, `agent` or `none`.

**A human says yes.** Anything public or permanent needs you to accept an approval form in the client: `store_version_publish`, `store_listing_update`, and in the off-by-default `store_experimental` toolset `store_review_reply`, `store_screenshots`, `store_app_create` and `store_app_delete`. The form says what will happen, then `Irreversible` notes, then stranger text fenced and labelled as data, then `Approve?` last. Hidden characters (zero-width, bidi, tag characters) are shown as escapes, so a review can't hide text in the form. Clients that can't show the form need writes on and `allow_no_elicit = true` in the profile, and then those actions go through on the agent's confirm alone. Nobody asks you.

**Stranger text is marked.** Reviews, listing text, nicknames and activity names come back as `{"untrusted": ...}`, and the server instructions tell the model that's data, not orders. It's a second layer. The approval form is the gate.

**Pins only narrow.** `.connect-iq-mcp.toml` can turn writes off, never on. Outside `trusted_pins` it can't pick a profile that allows writes.

**Files are fenced.** Builds, screenshots and downloads stay inside the project root (`--root` or `CIQ_MCP_ROOT`, else the client's MCP roots, else the git root). Uploads must be regular files inside the root, with no symlink on the way, and must start with the bytes their extension promises. `CIQ_MCP_KEY` is the one path outside the root a tool may use, and it is only read. Jungle paths must resolve inside the root, checked before the compiler runs. The details are in [SECURITY.md](../SECURITY.md#threat-model-short).

**The SDK is the SDK.** The model can only pick an SDK the SDK Manager installed, never an arbitrary path. Java comes only from absolute locations, so a repo can't plant its own. Connect IQ MCP runs `java -classpath monkeybrains.jar` itself, never the SDK's `monkeyc`/`monkeydo` scripts. `sdk_key_generate` asks git whether the key is ignored with hooks and fsmonitor off, so a cloned repo's `.git/config` can't run commands.

**Other people's simulators.** `sim_stop` ends only the simulator this server launched. Anything else returns `conflict` unless the call has `force:true`, and the tool tells the agent to ask you first. That's an instruction to the model, not an approval form.

**Credentials stay out of chat.** No tool takes or returns a cookie, token or password. The agent can open the sign-in window with `auth_login`, but you type the password on Garmin's page. Cookies, tokens, keys and signed URL queries are redacted from results, errors and logs. Results are capped at about 60 KB (`CIQ_MCP_MAX_RESPONSE_BYTES`), so a hostile payload can't flood the context.

**Half-done publishes are reported.** Once a version is posted, `store_version_publish` says `published: true` even if saving what's new failed afterwards, and the warning tells the agent to re-run it with the same version.

**HTTP mode** (`serve --http`) needs a bearer token of at least 32 characters, refuses non-loopback `Origin` headers, and binds to loopback unless you pass `--allow-remote` with `--tls-cert` and `--tls-key`. Wrong tokens get a growing backoff per IP.

**Supply chain.** Pinned modules and Go toolchain, SHA-pinned Actions, no npm in the job that signs, reproducible builds, an SBOM per archive, provenance attestations, govulncheck, CodeQL and gitleaks. Releases are immutable once published. `install.sh`, the `.mcpb` and `tools.json` are covered by cosign-signed checksums. `install.sh` checks the sha256, verifies cosign when cosign is installed, and runs `gh attestation verify` when `gh` is. If neither can check the release, it installs but doesn't run the binary and tells you how to verify it; `CIQ_MCP_REQUIRE_SIGNATURE=1` makes it refuse instead.

## The review that says "ignore previous instructions"

1. `store_reviews_list` returns it as `{"untrusted": "..."}`.
2. `store_review_reply` is in `store_experimental`, which is off. With writes off it isn't listed either.
3. With both on, the first call is a preview.
4. Confirming pops the approval form with their review fenced as data and your reply below it. Decline and nothing happens.

## What this doesn't fix

- The model can still be fooled. Your yes in the client is the real gate, and auto-approving everything or setting `allow_no_elicit` removes it.
- Preview and confirm catch accidents, not a model determined to call twice. That's why public actions also need you.
- The keychain protects the session at rest, not from other processes running as you. The sign-in browser profile can be copied by anything running as you.
- Read-only still reads. Install numbers, reviews and, with `connect` on, your heart rate go to whatever model your client uses.
- HTTP mode is for your own agents. There's no OAuth and no multi-tenant hosting.
- Garmin can change or block the private portal API. See the README's "Unofficial" section.

Found a gap? See [SECURITY.md](../SECURITY.md).
