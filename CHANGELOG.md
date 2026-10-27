# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions: [SemVer](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-10-28

First public release.

### Added

- Store tools for the Connect IQ developer portal: your apps, listings, versions, install statistics, ratings and reviews (with an inbox across all apps), and publishing a new version from a `.iq`. Publishing and listing edits show a preview and need your yes in the client.
- An off-by-default `store_experimental` toolset for portal calls nobody has tried live yet: creating and deleting apps, screenshots, review replies.
- SDK tools: builds with monkeyc (one device or the store `.iq` for all of them), devices, manifests and developer keys. Device names like "Fenix 7" resolve to ids.
- Simulator tools: run apps and unit tests, read logs and crashes, stop what this server started. On macOS also screenshots and button and GPS input.
- An opt-in `connect` toolset that reads your Garmin Connect data: activities, sleep, heart rate, HRV, stress, training, weight.
- `auth login` signs you in through a browser window on Garmin's own page, and the session renews by itself in the background. Credentials live in the OS keychain. `auth login --paste` and `auth export --portal` cover machines without a screen and CI.
- `config install` for Claude Code, Claude Desktop, Codex, Cursor, VS Code and Windsurf, with `--key` and `--java-home` for builds. `doctor` checks the lot.
- Read-only by default. Store writes are hidden until you turn them on.
- Stdio and Streamable HTTP (loopback with a bearer token, or remote with TLS).

### Security

- Releases are built from a tag by GitHub Actions and published as immutable releases, with cosign-signed checksums, an SPDX SBOM per archive and build provenance attestations. macOS binaries are signed with a Developer ID and notarized by Apple.

[Unreleased]: https://github.com/idoo/connect-iq-mcp/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/idoo/connect-iq-mcp/releases/tag/v0.1.0
