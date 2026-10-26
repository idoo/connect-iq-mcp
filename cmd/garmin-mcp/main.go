// Command garmin-mcp is the Garmin Connect IQ MCP server.
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

	"github.com/idoo/garmin-mcp/internal/auth"
	_ "github.com/idoo/garmin-mcp/internal/credstore" // sets auth.Open and auth.CLI
)

const usage = `garmin-mcp — Garmin Connect IQ MCP server

Usage:
  garmin-mcp [serve] [flags]          serve MCP over stdio (or --http ADDR); garmin-mcp serve --help lists flags
  garmin-mcp auth <subcommand>        manage Connect IQ portal and Garmin Connect credentials
  garmin-mcp doctor [--profile NAME] [--probe]  check config, credentials, SDK, Java and browser
  garmin-mcp config snippet <client>  print client config (claude-code, claude-desktop, codex, cursor, vscode, windsurf)
  garmin-mcp config install <client>  write it into the client's config (with backup)
  garmin-mcp version

Any subcommand with --help prints its usage and does nothing else.
`

// subUsage is the usage text printed for `garmin-mcp <cmd> --help`.
var subUsage = map[string]string{
	"config":  configUsage,
	"doctor":  "usage: garmin-mcp doctor [--profile NAME] [--probe]\n  checks the config, credentials, Connect IQ SDK, Java and sign-in browser\n  --probe also asks the developer portal who is signed in (one network call)",
	"version": "usage: garmin-mcp version",
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
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr) //nolint:forbidigo // CLI output.
	stop()
	var ue *usageError
	var ee exitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		os.Exit(int(ee))
	case errors.As(err, &ue):
		fmt.Fprintln(os.Stderr, "garmin-mcp:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "garmin-mcp:", err)
		os.Exit(1)
	}
}

// run dispatches a subcommand. Only serve writes the MCP protocol to stdout.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
		_, err := fmt.Fprintln(stdout, "garmin-mcp", buildVersion())
		return err
	default:
		return usageErr(fmt.Sprintf("unknown command %q\n\n%s", cmd, usage))
	}
}
