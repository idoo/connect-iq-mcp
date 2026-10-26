// Command ciq-mcp is the Garmin Connect IQ MCP server.
//
// With no arguments it serves MCP over stdio. Subcommands manage credentials
// profiles, diagnose the setup and configure MCP clients; they never run the
// server and print to stdout normally.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/idoo/ciq-mcp/internal/auth"
)

const usage = `ciq-mcp — Garmin Connect IQ MCP server

Usage:
  ciq-mcp [serve] [flags]          serve MCP over stdio (or --http ADDR); ciq-mcp serve --help lists flags
  ciq-mcp auth <subcommand>        manage Connect IQ portal and Garmin Connect credentials
  ciq-mcp doctor [--profile NAME]  check config and credentials
  ciq-mcp config snippet <client>  print client config (claude-code, claude-desktop, codex, cursor, vscode, windsurf)
  ciq-mcp config install <client>  write it into the client's config (with backup)
  ciq-mcp version

Any subcommand with --help prints its usage and does nothing else.
`

// subUsage is the usage text printed for `ciq-mcp <cmd> --help`.
var subUsage = map[string]string{
	"config":  configUsage,
	"doctor":  "usage: ciq-mcp doctor [--profile NAME]\n  checks the config and credentials",
	"version": "usage: ciq-mcp version",
}

// usageError is a command-line misuse; it exits with status 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErr(msg string) error { return &usageError{msg: msg} }

// exitError is a subcommand that already reported its failure; main exits
// with the code and prints nothing more.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr) //nolint:forbidigo // CLI output.
	stop()
	var ue *usageError
	var ee exitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		os.Exit(int(ee))
	case errors.As(err, &ue):
		fmt.Fprintln(os.Stderr, "ciq-mcp:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "ciq-mcp:", err)
		os.Exit(1)
	}
}

// run dispatches a subcommand. Only serve writes the MCP protocol to stdout.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		if _, err := io.WriteString(stdout, usage+"\nserve flags:\n"); err != nil {
			return err
		}
		return runServe(ctx, []string{"-h"}, stdout)
	}
	if len(args) == 0 || (args[0] != "" && args[0][0] == '-') {
		return runServe(ctx, args, stderr)
	}
	cmd, rest := args[0], args[1:]
	if u, ok := subUsage[cmd]; ok && wantsHelp(rest) {
		_, err := fmt.Fprintln(stdout, u)
		return err
	}
	switch cmd {
	case "serve":
		if wantsHelp(rest) {
			return runServe(ctx, []string{"-h"}, stdout)
		}
		return runServe(ctx, rest, stderr)
	case "auth":
		if code := auth.CLI(rest, stdout, stderr); code != 0 {
			return exitError(code)
		}
		return nil
	case "doctor":
		return runDoctor(ctx, rest, stdout, stderr)
	case "config":
		return runConfig(rest, stdout, stderr)
	case "version", "--version":
		_, err := fmt.Fprintln(stdout, "ciq-mcp", buildVersion())
		return err
	default:
		return usageErr(fmt.Sprintf("unknown command %q\n\n%s", cmd, usage))
	}
}
