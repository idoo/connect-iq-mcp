#!/bin/sh
# Pack goreleaser's dist/ binaries into one cross-platform Claude Desktop
# bundle: dist/ciq-mcp-<version>.mcpb plus dist/ciq-mcp-<version>.mcpb.sha256.
#
# Bundle layout (server/):
#   ciq-mcp-darwin               universal (arm64 + amd64)
#   ciq-mcp-linux                sh dispatcher on `uname -m`
#   ciq-mcp-linux-amd64, ciq-mcp-linux-arm64
#   ciq-mcp-windows-amd64.exe    (Windows on Arm runs it under x64 emulation)
# The manifest's tool list is generated from docs/tools.json (default
# toolsets, tools listed in read-only mode).
#
# Usage: scripts/build-mcpb.sh [version]   (default: dist/metadata.json)
# Needs: jq, zip. Validates with the pinned mcpb CLI when npx exists.
set -eu

mcpb_cli_version=2.1.2
default_toolsets='["core","store","sdk","simulator"]'

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
dist="$root/dist"
[ -d "$dist" ] || { echo "build-mcpb: $dist missing; run goreleaser first" >&2; exit 1; }
[ -f "$root/docs/tools.json" ] || { echo "build-mcpb: docs/tools.json missing" >&2; exit 1; }

version=${1:-}
if [ -z "$version" ]; then
	version=$(jq -r .version "$dist/metadata.json")
fi
version=${version#v}

# Find one binary in dist/ by goreleaser target directory prefix.
find_bin() {
	for d in "$dist"/$1; do
		for f in "$d/ciq-mcp" "$d/ciq-mcp.exe"; do
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

cp "$(find_bin 'ciq-mcp-universal_darwin_all')" "$stage/server/ciq-mcp-darwin"
cp "$(find_bin 'ciq-mcp_linux_amd64*')" "$stage/server/ciq-mcp-linux-amd64"
cp "$(find_bin 'ciq-mcp_linux_arm64*')" "$stage/server/ciq-mcp-linux-arm64"
cp "$(find_bin 'ciq-mcp_windows_amd64*')" "$stage/server/ciq-mcp-windows-amd64.exe"
cat >"$stage/server/ciq-mcp-linux" <<'EOF'
#!/bin/sh
# mcpb platform overrides are per-OS; pick the Linux binary for this CPU.
dir=$(dirname -- "$0")
case $(uname -m) in
aarch64 | arm64) exec "$dir/ciq-mcp-linux-arm64" "$@" ;;
*) exec "$dir/ciq-mcp-linux-amd64" "$@" ;;
esac
EOF
chmod 0755 "$stage/server/ciq-mcp-darwin" "$stage/server/ciq-mcp-linux" \
	"$stage/server/ciq-mcp-linux-amd64" "$stage/server/ciq-mcp-linux-arm64"

cp "$root/packaging/mcpb/icon.png" "$root/LICENSE" "$stage/"
jq --arg v "$version" --argjson ts "$default_toolsets" --slurpfile tools "$root/docs/tools.json" '
  .version = $v
  | .tools = [ $tools[0][]
      | select(.toolset as $t | $ts | index($t))
      | select(.class == "R")
      | {name, description} ]' \
	"$root/packaging/mcpb/manifest.json" >"$stage/manifest.json"

if command -v npx >/dev/null 2>&1; then
	npx -y "@anthropic-ai/mcpb@$mcpb_cli_version" validate "$stage/manifest.json" >&2
else
	echo "build-mcpb: npx not found, skipping manifest validation" >&2
fi

out="$dist/ciq-mcp-$version.mcpb"
rm -f "$out" "$out.sha256"
(cd "$stage" && zip -qrX "$out" manifest.json icon.png LICENSE server)
if command -v sha256sum >/dev/null 2>&1; then
	sum=$(sha256sum "$out" | awk '{ print $1 }')
else
	sum=$(shasum -a 256 "$out" | awk '{ print $1 }')
fi
# Same "<sha256>  <name>" format as checksums.txt, so `sha256sum -c` works.
printf '%s  %s\n' "$sum" "ciq-mcp-$version.mcpb" >"$out.sha256"
echo "$out"
