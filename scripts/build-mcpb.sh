#!/bin/sh
# Pack goreleaser's dist/ binaries into one cross-platform Claude Desktop
# bundle: dist/garmin-mcp-<version>.mcpb plus dist/garmin-mcp-<version>.mcpb.sha256.
#
# Bundle layout (server/):
#   garmin-mcp-darwin               universal (arm64 + amd64)
#   garmin-mcp-linux                sh dispatcher on `uname -m`
#   garmin-mcp-linux-amd64, garmin-mcp-linux-arm64
#   garmin-mcp-windows-amd64.exe    win32 is amd64-only: mcpb's platform_overrides
#                                   are per OS, not per arch, and win32 needs a real
#                                   .exe (no sh dispatcher). Windows 11 on Arm runs it
#                                   under x64 emulation; Windows 10 on Arm can't, so
#                                   there use the windows_arm64 release archive and
#                                   `garmin-mcp config install claude-desktop`.
# The manifest's tool list is generated from docs/tools.json (default
# toolsets, tools listed in read-only mode).
#
# Usage: scripts/build-mcpb.sh [version]   (default: dist/metadata.json)
#        scripts/build-mcpb.sh --validate-manifest <version>
# Needs: jq, zip. Validates the manifest with the pinned mcpb CLI when npx
# exists, unless MCPB_SKIP_VALIDATE=1. The release workflow skips it in the
# privileged job and runs --validate-manifest (manifest only, no dist/, no
# secrets) in a separate unprivileged job: npx runs a floating npm tree.
set -eu

mcpb_cli_version=2.1.2
default_toolsets='["core","store","sdk","simulator"]'

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
dist="$root/dist"
[ -f "$root/docs/tools.json" ] || { echo "build-mcpb: docs/tools.json missing" >&2; exit 1; }

# make_manifest <version> <out>: the bundle manifest with the default
# toolsets' read-only tools.
make_manifest() {
	jq --arg v "$1" --argjson ts "$default_toolsets" --slurpfile tools "$root/docs/tools.json" '
  .version = $v
  | .tools = [ $tools[0][]
      | select(.toolset as $t | $ts | index($t))
      | select(.class == "R")
      | {name, description} ]' \
		"$root/packaging/mcpb/manifest.json" >"$2"
}

validate_manifest() {
	if command -v npx >/dev/null 2>&1; then
		npx -y "@anthropic-ai/mcpb@$mcpb_cli_version" validate "$1" >&2
	else
		echo "build-mcpb: npx not found, skipping manifest validation" >&2
	fi
}

if [ "${1:-}" = "--validate-manifest" ]; then
	[ -n "${2:-}" ] || { echo "build-mcpb: --validate-manifest needs a version" >&2; exit 2; }
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM
	make_manifest "${2#v}" "$tmp/manifest.json"
	validate_manifest "$tmp/manifest.json"
	exit 0
fi

[ -d "$dist" ] || { echo "build-mcpb: $dist missing; run goreleaser first" >&2; exit 1; }

version=${1:-}
if [ -z "$version" ]; then
	version=$(jq -r .version "$dist/metadata.json")
fi
version=${version#v}

# Find one binary in dist/ by goreleaser target directory prefix.
find_bin() {
	for d in "$dist"/$1; do
		for f in "$d/garmin-mcp" "$d/garmin-mcp.exe"; do
			if [ -f "$f" ]; then
				printf '%s\n' "$f"
				return 0
			fi
		done
	done
	echo "build-mcpb: no binary matching dist/$1" >&2
	return 1
}

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT INT TERM
mkdir -p "$stage/server"

cp "$(find_bin 'garmin-mcp-universal_darwin_all')" "$stage/server/garmin-mcp-darwin"
cp "$(find_bin 'garmin-mcp_linux_amd64*')" "$stage/server/garmin-mcp-linux-amd64"
cp "$(find_bin 'garmin-mcp_linux_arm64*')" "$stage/server/garmin-mcp-linux-arm64"
cp "$(find_bin 'garmin-mcp_windows_amd64*')" "$stage/server/garmin-mcp-windows-amd64.exe"
cat >"$stage/server/garmin-mcp-linux" <<'EOF'
#!/bin/sh
# mcpb platform overrides are per-OS; pick the Linux binary for this CPU.
dir=$(dirname -- "$0")
case $(uname -m) in
aarch64 | arm64) exec "$dir/garmin-mcp-linux-arm64" "$@" ;;
*) exec "$dir/garmin-mcp-linux-amd64" "$@" ;;
esac
EOF
chmod 0755 "$stage/server/garmin-mcp-darwin" "$stage/server/garmin-mcp-linux" \
	"$stage/server/garmin-mcp-linux-amd64" "$stage/server/garmin-mcp-linux-arm64"

cp "$root/packaging/mcpb/icon.png" "$root/LICENSE" "$stage/"
make_manifest "$version" "$stage/manifest.json"

if [ "${MCPB_SKIP_VALIDATE:-}" != 1 ]; then
	validate_manifest "$stage/manifest.json"
fi

out="$dist/garmin-mcp-$version.mcpb"
rm -f "$out" "$out.sha256"
(cd "$stage" && zip -qrX "$out" manifest.json icon.png LICENSE server)
if command -v sha256sum >/dev/null 2>&1; then
	sum=$(sha256sum "$out" | awk '{ print $1 }')
else
	sum=$(shasum -a 256 "$out" | awk '{ print $1 }')
fi
# Same "<sha256>  <name>" format as checksums.txt, so `sha256sum -c` works.
printf '%s  %s\n' "$sum" "garmin-mcp-$version.mcpb" >"$out.sha256"
echo "$out"
