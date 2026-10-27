package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/auth"
	"github.com/idoo/connect-iq-mcp/internal/auth/login"
	"github.com/idoo/connect-iq-mcp/internal/config"
	"github.com/idoo/connect-iq-mcp/internal/credstore"
	"github.com/idoo/connect-iq-mcp/internal/portal"
	"github.com/idoo/connect-iq-mcp/internal/state"
)

// authStdin is read by `auth login --paste` and the logout confirmation.
var authStdin io.Reader = os.Stdin

const authUsage = `usage:
  connect-iq-mcp auth login  [--profile P] [--connect] [--browser PATH] [--paste]
  connect-iq-mcp auth status [--profile P] [--json]
  connect-iq-mcp auth renew  [--profile P]
  connect-iq-mcp auth logout [--profile P]
  connect-iq-mcp auth export --portal [--profile P]   (stdout must not be a terminal)`

// authCLI runs `connect-iq-mcp auth ARGS` and returns the exit code.
func authCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, authUsage)
		return 2
	}
	// The LaunchServices browser is not our child: cancel (and clean up) on
	// every terminating signal, not just Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("auth "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "", "credentials profile")
	var (
		connect, paste, asJSON, exportPortal bool
		browserPath                          string
	)
	switch cmd {
	case "login":
		fs.BoolVar(&connect, "connect", false, "also sign in to Garmin Connect")
		fs.BoolVar(&paste, "paste", false, "read a developer-portal Cookie header (or DevTools cURL) from stdin")
		fs.StringVar(&browserPath, "browser", "", "Chromium-based browser executable")
	case "status":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
	case "export":
		fs.BoolVar(&exportPortal, "portal", false, "print the developer-portal Cookie header")
	case "renew", "logout":
	case "help", "-h", "--help":
		_, _ = fmt.Fprintln(stdout, authUsage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown auth command %q\n%s\n", cmd, authUsage)
		return 2
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	s, err := authStore(*profile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	switch cmd {
	case "login":
		err = authLogin(ctx, s, stdout, stderr, connect, paste, browserPath)
	case "status":
		err = authStatus(ctx, s.Store, stdout, asJSON)
	case "renew":
		err = authRenew(ctx, s.Store, stdout)
	case "logout":
		err = authLogout(ctx, s.Store, stdout, stderr)
	case "export":
		err = authExport(s.Store, stdout, exportPortal)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// authStore resolves the profile like the rest of the CLI.
func authStore(flagProfile string) (*login.Store, error) {
	r, err := config.Resolve(config.Options{Flags: config.Flags{Profile: flagProfile}})
	if err != nil {
		return nil, err
	}
	dir, err := state.DefaultDir()
	if err != nil {
		return nil, err
	}
	name := r.Profile.Name
	autoRenew := true
	if r.Config != nil {
		if p, ok := r.Config.Profiles[name]; ok {
			autoRenew = p.AutoRenewEnabled()
		}
	}
	s, err := credstore.New(name, dir, autoRenew, login.Chromium{})
	if err != nil {
		return nil, err
	}
	return &login.Store{Store: s}, nil
}

func authLogin(ctx context.Context, s *login.Store, stdout, stderr io.Writer, connect, paste bool, browserPath string) error {
	var res auth.LoginResult
	var err error
	if paste {
		if connect {
			return errors.New("--paste only covers the developer portal; run login --connect without --paste for Garmin Connect")
		}
		if f, ok := authStdin.(*os.File); ok && isTerminal(f) {
			_, _ = fmt.Fprintln(stderr, pastePrompt())
		}
		if res, err = pasteLogin(ctx, s.Store); err != nil {
			return err
		}
	} else if res, err = s.Login(ctx, auth.LoginOptions{Connect: connect, BrowserPath: browserPath, Log: stderr}); err != nil {
		return err
	}
	name := res.DeveloperName
	if name == "" {
		name = "developer"
	}
	valid := "unknown"
	if !res.PortalExpires.IsZero() {
		valid = res.PortalExpires.Local().Format("2006-01-02")
	}
	_, _ = fmt.Fprintf(stdout, "Signed in as %s (%s). Session valid until %s.\n", name, res.DeveloperID, valid)
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintln(stderr, "warning:", w)
	}
	if res.Connect {
		who := res.ConnectName
		if who == "" {
			who = "your account"
		}
		_, _ = fmt.Fprintf(stdout, "Garmin Connect: signed in as %s.\n", who)
	}
	return nil
}

// pastePrompt tells a terminal user where to copy the cookie from and how to
// end the pasted input: --paste reads to EOF, and a Windows console has no
// Ctrl+D.
func pastePrompt() string {
	eof := "Ctrl+D"
	if runtime.GOOS == "windows" {
		eof = "Ctrl+Z, then Enter"
	}
	return "In a browser signed in to apps.garmin.com, open DevTools → Network, pick any apps.garmin.com request and copy its Cookie from Request Headers (or right-click it → Copy as cURL).\n" +
		"Paste the Cookie header or the \"Copy as cURL\" command, then press Enter and " + eof + "."
}

// pasteLogin stores a developer-portal Cookie header read from stdin.
func pasteLogin(ctx context.Context, s *credstore.Store) (auth.LoginResult, error) {
	if err := s.RefuseEnv(false); err != nil {
		return auth.LoginResult{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(authStdin, 1<<20))
	if err != nil {
		return auth.LoginResult{}, err
	}
	header, err := portal.ParseCookieInput(string(raw))
	if err != nil {
		return auth.LoginResult{}, err
	}
	return s.SavePortalHeader(ctx, header)
}

type statusJSON struct {
	Profile string   `json:"profile"`
	Portal  credJSON `json:"portal"`
	Connect credJSON `json:"connect"`
}

type credJSON struct {
	Present   bool       `json:"present"`
	Source    string     `json:"source,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Renewable bool       `json:"renewable"`
	Note      string     `json:"note,omitempty"`
}

func toCredJSON(c auth.CredStatus) credJSON {
	j := credJSON{Present: c.Present, Source: c.Source, Renewable: c.Renewable, Note: c.Note}
	if !c.ExpiresAt.IsZero() {
		t := c.ExpiresAt.UTC()
		j.ExpiresAt = &t
	}
	return j
}

// authStatus prints the credential state. It fails (non-zero exit) when a
// stored credential exists but could not be read (locked or unreadable).
func authStatus(ctx context.Context, s *credstore.Store, stdout io.Writer, asJSON bool) error {
	st := s.Status(ctx)
	var err error
	if state := credstore.UnreadableState(st.Portal); state != "" {
		err = fmt.Errorf("stored credentials are %s; see the note above", state)
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if e := enc.Encode(statusJSON{Profile: s.Profile(), Portal: toCredJSON(st.Portal), Connect: toCredJSON(st.Connect)}); e != nil {
			return e
		}
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Profile: %s\n\n", s.Profile())
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CREDENTIAL\tSTATE\tSOURCE\tEXPIRES\tNOTE")
	now := time.Now()
	row := func(name string, c auth.CredStatus) {
		state, src, exp := "not signed in", "-", "-"
		if c.Present {
			state, src = "signed in", c.Source
			if !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now) {
				state = "expired"
			}
		}
		if u := credstore.UnreadableState(c); u != "" {
			state = u
		}
		if !c.ExpiresAt.IsZero() {
			exp = c.ExpiresAt.Local().Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, state, src, exp, c.Note)
	}
	row("portal (apps.garmin.com)", st.Portal)
	row("connect (Garmin Connect)", st.Connect)
	if e := tw.Flush(); e != nil {
		return e
	}
	return err
}

func authRenew(ctx context.Context, s *credstore.Store, stdout io.Writer) error {
	r, err := s.Renew(ctx)
	switch {
	case err != nil:
		return err
	case r.NotExtended:
		_, _ = fmt.Fprintln(stdout, "Garmin returned the same developer-portal session, so its expiry did not move; it was kept.")
		if r.Connect {
			_, _ = fmt.Fprintln(stdout, "Garmin Connect renewed.")
		}
	default:
		_, _ = fmt.Fprintln(stdout, "Renewed.")
	}
	return authStatus(ctx, s, stdout, false)
}

func authLogout(ctx context.Context, s *credstore.Store, stdout, stderr io.Writer) error {
	if isTerminal(os.Stdin) && isTerminalWriter(stderr) {
		_, _ = fmt.Fprintf(stderr, "Delete stored credentials and the browser profile for %q? [y/N] ", s.Profile())
		line, _ := bufio.NewReader(authStdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("cancelled")
		}
	}
	if err := s.Logout(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Signed out of profile %s.\n", s.Profile())
	if s.HasEnvCredentials() {
		_, _ = fmt.Fprintln(stdout, "Note: CIQ_MCP_* environment variables still provide credentials.")
	}
	return nil
}

func authExport(s *credstore.Store, stdout io.Writer, exportPortal bool) error {
	if !exportPortal {
		return errors.New("choose what to export: --portal")
	}
	if isTerminalWriter(stdout) {
		return errors.New("refusing to print secrets to a terminal; redirect stdout (e.g. | pbcopy)")
	}
	cookies, err := s.ExportPortal()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, cookies)
	return err
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminal(f)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
