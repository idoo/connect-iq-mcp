<p align="center"><img src="docs/assets/hero.jpg" alt="A goose in a headband and a guy in a grey hoodie with a smartwatch for a head run along a night lakeside trail past a PENDING APPROVAL sign, laptop open, while a cartoon Viking on a kid's bike films them laughing." width="100%"></p>

# Connect IQ MCP

MCP server for Garmin Connect IQ developers. Your agent gets the store (versions, listings, reviews, installs), SDK builds, the simulator and, if you want it, your Garmin Connect data. Read-only by default, and it asks before anything goes public. The binary is `connect-iq-mcp`.

[![CI](https://github.com/idoo/connect-iq-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/idoo/connect-iq-mcp/actions/workflows/ci.yml)
[![CodeQL](https://github.com/idoo/connect-iq-mcp/actions/workflows/codeql.yml/badge.svg)](https://github.com/idoo/connect-iq-mcp/actions/workflows/codeql.yml)
[![Release](https://img.shields.io/github/v/release/idoo/connect-iq-mcp)](https://github.com/idoo/connect-iq-mcp/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/idoo/connect-iq-mcp.svg)](https://pkg.go.dev/github.com/idoo/connect-iq-mcp)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![MCP](https://img.shields.io/badge/MCP-server-black)](https://modelcontextprotocol.io)
[![Glama score](https://glama.ai/mcp/servers/idoo/connect-iq-mcp/badges/score.svg)](https://glama.ai/mcp/servers/idoo/connect-iq-mcp)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/idoo/connect-iq-mcp/badge)](https://scorecard.dev/viewer/?uri=github.com/idoo/connect-iq-mcp)

## Install

Paste this into whatever AI is writing your watch face right now:

```text
Set up the connect-iq-mcp server for me: https://github.com/idoo/connect-iq-mcp/blob/main/docs/INSTALL.md
```

Or by hand, in three steps:

```sh
curl -fsSL https://github.com/idoo/connect-iq-mcp/releases/latest/download/install.sh | sh   # or: brew install idoo/tap/connect-iq-mcp
connect-iq-mcp auth login                    # opens Garmin's sign-in page, tick Remember me
connect-iq-mcp config install claude-code    # or claude-desktop, codex, cursor, vscode, windsurf
```

Builds and the simulator also need Garmin's [SDK Manager](https://developer.garmin.com/connect-iq/sdk/) and Java 17+. Details, Windows and Linux: [docs/INSTALL.md](docs/INSTALL.md). Sign-in, signing keys, CI, upgrades and fixes: [docs/FAQ.md](docs/FAQ.md).

## Why you can trust it

You're handing it your Garmin developer account, so here's what you can check yourself instead of taking my word for it:

- **All of it is here.** MIT, no closed parts. Build it yourself with `go install github.com/idoo/connect-iq-mcp/cmd/connect-iq-mcp@latest`.
- **It only talks to Garmin.** The developer portal at `apps.garmin.com`, Garmin's SSO in the sign-in browser, and Garmin Connect if you turn it on. No telemetry. [SECURITY.md](SECURITY.md#network)
- **No tool sees your password or cookies.** You sign in on Garmin's own page in a separate browser profile. The session cookie goes to the OS keychain, and no tool takes or returns it. [SECURITY.md](SECURITY.md#credentials)
- **Read-only until you say otherwise.** Store write tools aren't listed until you turn writes on, and anything public shows a preview and waits for your yes. [docs/SAFETY.md](docs/SAFETY.md)
- **Releases are signed and traceable to their source.** Every release is built by [release.yml](.github/workflows/release.yml) from a tag, published as an [immutable release](https://docs.github.com/en/code-security/supply-chain-security/understanding-your-software-supply-chain/immutable-releases), and ships an SPDX SBOM per archive, a cosign keyless signature on `checksums.txt`, and [build provenance attestations](https://github.com/idoo/connect-iq-mcp/attestations). The macOS binaries are also signed with my Developer ID and notarized by Apple.
- **CI scans every push** with CodeQL and `govulncheck`, and a tag can't ship with a known reachable vulnerability.

Check a download with the GitHub CLI:

```sh
gh attestation verify connect-iq-mcp_<version>_<os>_<arch>.tar.gz --repo idoo/connect-iq-mcp \
  --signer-workflow idoo/connect-iq-mcp/.github/workflows/release.yml --source-ref refs/tags/v<version>
```

or with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/):

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity 'https://github.com/idoo/connect-iq-mcp/.github/workflows/release.yml@refs/tags/v<version>' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing
```

On a Mac, `spctl -a -vv -t install connect-iq-mcp` should say `source=Notarized Developer ID`.

The `.mcpb` and `tools.json` are listed in `checksums-extra.txt` with their own `checksums-extra.txt.sigstore.json`. Both checks above work on them the same way.

## Unofficial

Garmin has no public store API, so Connect IQ MCP uses the same private endpoints the developer portal website uses, plus Garmin's sign-in. Garmin can change them and break this any day, or object to it. Renewal runs a headless browser that doesn't announce itself as automated, and automated access may be against the portal's terms: it's your account and your call. Request rates stay low.

Not affiliated with or endorsed by Garmin. Garmin, Connect IQ and fēnix are trademarks of Garmin. If Garmin objects to the name, the name changes.

## Compared

| | **this one** | [web portal](https://apps.garmin.com) | `monkeyc` + manual upload | [wroblisko](https://github.com/wroblisko/garmin-simulator-crossplatform-mcp) | [andreycooper](https://github.com/andreycooper/connectiq-simulator-mcp) |
|---|:-:|:-:|:-:|:-:|:-:|
| Works from your agent | + | − | − | + | + |
| Store: reviews, installs, listings | + | + | − | − | − |
| Publish a version | + | + | ~ | − | − |
| SDK builds | + | − | + | + | + |
| Simulator screenshots and input | + | − | ~ | + | + |
| Unit tests in the simulator | + | − | + | − | + |
| Prebuilt signed binary | + | | | − | − |

`~` by hand. Screenshots and simulator input are macOS only here and in andreycooper's; wroblisko's also does Windows.

## Security

It can't publish anything until you let it. Publishing a version and editing the listing wait for your yes; builds and the simulator don't ask. A 1-star review that says "ignore previous instructions" is still just a 1-star review. More in [docs/SAFETY.md](docs/SAFETY.md). Report holes privately: [SECURITY.md](SECURITY.md).

## What it does

- "How are my apps doing? Downloads, ratings, last version."
- "Any new reviews? Draft replies to the angry ones."
- "Build the app for fenix 8, run it in the simulator, take a screenshot."
- "Build the store .iq for all devices and publish it as 1.4.0." (asks you first)
- "Update the store description, keep the rest as is." (asks you first)
- "Which devices does the app support, and which new ones am I missing?"

Tools come in toolsets, so your agent only sees what you use. `store`, `sdk` and `simulator` are on out of the box. `connect` (your Garmin Connect data, after `auth login --connect`) and `store_experimental` (portal calls nobody has tried live yet) are off. Pick with `CIQ_MCP_TOOLSETS` (`all`, `+connect`) or ask the agent mid-chat. Every tool: [docs/TOOLS.md](docs/TOOLS.md).

## License

[MIT](LICENSE) © Ivan Verevkin
