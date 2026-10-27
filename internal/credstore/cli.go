package credstore

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

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/state"
)

// stdin is read by `auth login --paste` and the logout confirmation.
var stdin io.Reader = os.Stdin

// Open returns the credential store for profile, honoring the profile's
// auto_renew setting from the config file.
func Open(_ context.Context, profile string) (auth.Store, error) {
	dir, err := state.DefaultDir()
	if err != nil {
		return nil, err
	}
	autoRenew := true
	if path, err := config.DefaultPath(os.Getenv); err == nil {
		if cfg, _, err := config.Load(path); err == nil {
			if p, ok := cfg.Profiles[profile]; ok {
				autoRenew = p.AutoRenewEnabled()
			}
		}
	}
	return newStore(profile, dir, autoRenew)
}

const cliUsage = `usage:
  garmin-mcp auth login  [--profile P] [--connect] [--browser PATH] [--paste]
  garmin-mcp auth status [--profile P] [--json]
  garmin-mcp auth renew  [--profile P]
  garmin-mcp auth logout [--profile P]
  garmin-mcp auth export --portal [--profile P]   (stdout must not be a terminal)`

// CLI runs `garmin-mcp auth ARGS` and returns the exit code.
func CLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, cliUsage)
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
		_, _ = fmt.Fprintln(stdout, cliUsage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown auth command %q\n%s\n", cmd, cliUsage)
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
	s, err := cliStore(*profile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	switch cmd {
	case "login":
		err = s.cliLogin(ctx, stdout, stderr, connect, paste, browserPath)
	case "status":
		err = s.cliStatus(ctx, stdout, asJSON)
	case "renew":
		err = s.cliRenew(ctx, stdout)
	case "logout":
		err = s.cliLogout(ctx, stdout, stderr)
	case "export":
		err = s.cliExport(stdout, exportPortal)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// cliStore resolves the profile like the rest of the CLI.
func cliStore(flagProfile string) (*Store, error) {
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
	return newStore(name, dir, autoRenew)
}

func (s *Store) cliLogin(ctx context.Context, stdout, stderr io.Writer, connect, paste bool, browserPath string) error {
	var res auth.LoginResult
	var err error
	if paste {
		if connect {
			return errors.New("--paste only covers the developer portal; run login --connect without --paste for Garmin Connect")
		}
		if f, ok := stdin.(*os.File); ok && isTerminal(f) {
			_, _ = fmt.Fprintln(stderr, pastePrompt())
		}
		if res, err = s.pasteLogin(ctx); err != nil {
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
func (s *Store) pasteLogin(ctx context.Context) (auth.LoginResult, error) {
	if err := s.refuseEnv(false); err != nil {
		return auth.LoginResult{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return auth.LoginResult{}, err
	}
	header, err := portal.ParseCookieInput(string(raw))
	if err != nil {
		return auth.LoginResult{}, err
	}
	unlock, err := lockPath(ctx, s.lockFile())
	if err != nil {
		return auth.LoginResult{}, fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	dev, session, err := s.verifyPortal(ctx, header)
	if err != nil {
		return auth.LoginResult{}, err
	}
	r := signInResult{Portal: &portalCred{Cookies: session, DeveloperID: dev.ID, DeveloperName: dev.Name, SavedAt: s.now().Unix()}}
	r.Expires, _ = portal.SealExpiry(session)
	if err := s.savePortal(r.Portal); err != nil {
		return auth.LoginResult{}, err
	}
	r.Warnings = s.takeSaveWarnings()
	return r.result(), nil
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

// cliStatus prints the credential state. It fails (non-zero exit) when a
// stored credential exists but could not be read (locked or unreadable).
func (s *Store) cliStatus(ctx context.Context, stdout io.Writer, asJSON bool) error {
	st := s.Status(ctx)
	var err error
	if state := UnreadableState(st.Portal); state != "" {
		err = fmt.Errorf("stored credentials are %s; see the note above", state)
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if e := enc.Encode(statusJSON{Profile: s.profile, Portal: toCredJSON(st.Portal), Connect: toCredJSON(st.Connect)}); e != nil {
			return e
		}
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Profile: %s\n\n", s.profile)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CREDENTIAL\tSTATE\tSOURCE\tEXPIRES\tNOTE")
	row := func(name string, c auth.CredStatus) {
		state, src, exp := "not signed in", "-", "-"
		if c.Present {
			state, src = "signed in", c.Source
			if !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(s.now()) {
				state = "expired"
			}
		}
		if u := UnreadableState(c); u != "" {
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

func (s *Store) cliRenew(ctx context.Context, stdout io.Writer) error {
	b, ps, cs, err := s.view()
	if err != nil {
		return err
	}
	wantPortal := ps != srcEnv
	wantConnect := b.Connect != nil && cs != srcEnv
	if !wantPortal && !wantConnect {
		return errors.New("credentials come from environment variables; nothing to renew")
	}
	unlock, err := lockPath(ctx, s.lockFile())
	if err != nil {
		return fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	err = s.renewLocked(ctx, wantPortal, wantConnect, true)
	switch {
	case errors.Is(err, errSealNotExtended):
		_, _ = fmt.Fprintln(stdout, "Garmin returned the same developer-portal session, so its expiry did not move; it was kept.")
		if wantConnect {
			_, _ = fmt.Fprintln(stdout, "Garmin Connect renewed.")
		}
	case errors.Is(err, browser.ErrRateLimited):
		return fmt.Errorf("silent renewal failed: %w", err)
	case errors.Is(err, errSignInOpen):
		return errors.New("a sign-in window is open for this profile; finish or close it, then retry `garmin-mcp auth renew`")
	case errors.Is(err, errNoRenewal) || errors.Is(err, browser.ErrInteractionRequired):
		return s.authRequired("silent renewal failed", err)
	case err != nil:
		return err
	default:
		_, _ = fmt.Fprintln(stdout, "Renewed.")
	}
	return s.cliStatus(ctx, stdout, false)
}

func (s *Store) cliLogout(ctx context.Context, stdout, stderr io.Writer) error {
	if isTerminal(os.Stdin) && isTerminalWriter(stderr) {
		_, _ = fmt.Fprintf(stderr, "Delete stored credentials and the browser profile for %q? [y/N] ", s.profile)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("cancelled")
		}
	}
	if err := s.logout(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Signed out of profile %s.\n", s.profile)
	if s.envPortal != nil || s.envConnect != nil {
		_, _ = fmt.Fprintln(stdout, "Note: GARMIN_MCP_* environment variables still provide credentials.")
	}
	return nil
}

func (s *Store) cliExport(stdout io.Writer, exportPortal bool) error {
	if !exportPortal {
		return errors.New("choose what to export: --portal")
	}
	if isTerminalWriter(stdout) {
		return errors.New("refusing to print secrets to a terminal; redirect stdout (e.g. | pbcopy)")
	}
	b, _, _, err := s.view()
	if err != nil {
		return err
	}
	if b.Portal == nil || b.Portal.Cookies == "" {
		return s.authRequired("not signed in to the Connect IQ developer portal", nil)
	}
	_, err = fmt.Fprintln(stdout, b.Portal.Cookies)
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
