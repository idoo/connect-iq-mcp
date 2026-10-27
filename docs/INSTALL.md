# Installing Connect IQ MCP

Written for a coding agent. Humans may read along. Do the steps in order and stop if one fails.

## 1. Binary

Use the first one that works, then check with `connect-iq-mcp version`.

macOS and Linux:

```sh
brew install idoo/tap/connect-iq-mcp
curl -fsSL https://github.com/idoo/connect-iq-mcp/releases/latest/download/install.sh | sh   # checks sha256 and signatures, installs to ~/.local/bin
go install github.com/idoo/connect-iq-mcp/cmd/connect-iq-mcp@latest                          # fetches Go 1.26.10 if yours is older
```

`install.sh` always checks the sha256. It also checks the cosign signature if `cosign` is installed, and the build provenance if `gh` is installed and signed in. If neither is there, it installs the binary but doesn't run it, and prints the commands to verify it yourself. `CIQ_MCP_REQUIRE_SIGNATURE=1` makes it refuse instead. `CIQ_MCP_VERSION` and `CIQ_MCP_INSTALL_DIR` pick another release or directory.

Windows: the same curl line works in Git Bash, or take the zip from [Releases](https://github.com/idoo/connect-iq-mcp/releases) and put `connect-iq-mcp.exe` on your PATH.

Claude Desktop can skip this: double-click `connect-iq-mcp-<version>.mcpb` from [Releases](https://github.com/idoo/connect-iq-mcp/releases). The bundle is x64 only on Windows. Windows 11 on Arm runs it emulated. On Windows 10 on Arm, take the arm64 zip and run `connect-iq-mcp config install claude-desktop`, which also finds the MSIX build of Claude Desktop.

There is no Docker image: sign-in needs a browser, and builds need your SDK and simulator.

## 2. SDK and Java

Store tools work without the SDK. Builds and the simulator need:

- The Connect IQ SDK. Install Garmin's [SDK Manager](https://developer.garmin.com/connect-iq/sdk/) and download an SDK and your devices in it. This is a GUI app, so ask the user if it's missing.
- Java 17 or newer: `java` on PATH, or an absolute `JAVA_HOME` in the client's config (step 4).

## 3. Sign in (the user does this, not you)

Ask the user to run this in a terminal:

```sh
connect-iq-mcp auth login
```

A browser window opens on Garmin's own sign-in page. They sign in and tick Remember me. The password never passes through you or the terminal.

- No display (SSH, a server): `connect-iq-mcp auth login --paste`, then paste the Cookie header or the Copy as cURL text of any apps.garmin.com request from a browser where they're signed in (DevTools → Network).
- Garmin Connect health data is a separate opt-in: `connect-iq-mcp auth login --connect`. Skip it unless they want it.
- Several Garmin accounts: add `--profile <name>` here and in step 4.

## 4. Client

```sh
connect-iq-mcp config install claude-code    # or claude-desktop, codex, cursor, vscode, windsurf
```

If the user ships to the store, add their signing key and, if Java isn't found, the JDK. Both paths must be absolute:

```sh
connect-iq-mcp config install claude-code --key /abs/path/developer_key.der --java-home /abs/path/jdk
```

These go into the client's `env` block, because a shell export doesn't reach a client started from the Dock or Start menu. `connect-iq-mcp config snippet <client>` prints the config instead of writing it. `config install vscode` writes the user `mcp.json` of every VS Code it finds (Code, Code - OSS, VSCodium, and their Flatpaks on Linux). On WSL, `claude-desktop` and `vscode` write nothing and print a `wsl.exe` snippet for the Windows app instead.

Restart the client afterwards.

## 5. Check

```sh
connect-iq-mcp doctor
```

Fix anything marked failed and run it again. `doctor` runs in your shell, so it can't see the client's env: an unset key shows as `info`, not an error. Connect showing "not enabled" is fine.

Platform notes, if doctor complains:

- Linux over SSH: the keyring is usually there but locked, and that's an error, not a reason to fall back to the file. Unlock it (`gnome-keyring-daemon --unlock`) or set `CIQ_MCP_KEYRING=off`. With no D-Bus session at all, a 0600 file is used.
- Ubuntu 24.04: AppArmor won't let the downloaded Chrome for Testing start, so install Chrome, Brave or Edge from their .deb. On arm64 Linux use the distro's `chromium` package (not the snap) or Brave. `doctor` warns when Chrome for Testing is blocked like that or is missing shared libraries.
- Linux without a desktop: the simulator is a GUI app, so start the server under `xvfb-run`.

## 6. Try it

Call the `auth_status` tool, then ask "how are my apps doing?". Connect IQ MCP starts read-only, so the worst it can do is know things. See [SAFETY.md](SAFETY.md). Upgrading, CI, uninstalling and common errors: [FAQ.md](FAQ.md).
