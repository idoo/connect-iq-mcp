#!/bin/sh
# Print a Homebrew formula that installs the prebuilt release archives
# (formulae are not quarantined, unlike casks). release.yml pushes the
# output to idoo/homebrew-tap/Formula/connect-iq-mcp.rb after the release is published.
#
# Usage: scripts/brew-formula.sh <version> <checksums.txt>
set -eu

[ $# -eq 2 ] || { echo "usage: $0 <version> <checksums.txt>" >&2; exit 2; }
version=${1#v}
sums=$2

sha() {
	f="connect-iq-mcp_${version}_$1.tar.gz"
	s=$(awk -v f="$f" '$2 == f { print $1 }' "$sums")
	[ -n "$s" ] || { echo "brew-formula: $f not in $sums" >&2; exit 1; }
	printf '%s' "$s"
}

base="https://github.com/idoo/connect-iq-mcp/releases/download/v$version"
darwin_arm64=$(sha darwin_arm64)
darwin_amd64=$(sha darwin_amd64)
linux_arm64=$(sha linux_arm64)
linux_amd64=$(sha linux_amd64)

cat <<EOF
class ConnectIqMcp < Formula
  desc "MCP server for Garmin Connect IQ developers"
  homepage "https://github.com/idoo/connect-iq-mcp"
  version "$version"
  license "MIT"

  livecheck do
    url :stable
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "$base/connect-iq-mcp_${version}_darwin_arm64.tar.gz"
      sha256 "$darwin_arm64"
    end
    on_intel do
      url "$base/connect-iq-mcp_${version}_darwin_amd64.tar.gz"
      sha256 "$darwin_amd64"
    end
  end

  on_linux do
    on_arm do
      url "$base/connect-iq-mcp_${version}_linux_arm64.tar.gz"
      sha256 "$linux_arm64"
    end
    on_intel do
      url "$base/connect-iq-mcp_${version}_linux_amd64.tar.gz"
      sha256 "$linux_amd64"
    end
  end

  def install
    bin.install "connect-iq-mcp"
  end

  def caveats
    <<~TEXT
      Sign in to the Connect IQ developer portal once, then connect your client:
        connect-iq-mcp auth login
        connect-iq-mcp config install <client>   # claude-code, claude-desktop, codex, cursor, vscode, windsurf
    TEXT
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/connect-iq-mcp version")
  end
end
EOF
