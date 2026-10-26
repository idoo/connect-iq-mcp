package config

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/idoo/garmin-mcp/internal/tool"
)

// Mode environment variables.
const (
	EnvProfile          = "GARMIN_MCP_PROFILE"
	EnvAllowWrites      = "GARMIN_MCP_ALLOW_WRITES"
	EnvToolsets         = "GARMIN_MCP_TOOLSETS"
	EnvDiscovery        = "GARMIN_MCP_DISCOVERY"
	EnvMaxResponseBytes = "GARMIN_MCP_MAX_RESPONSE_BYTES"
	EnvMaxRetries       = "GARMIN_MCP_MAX_RETRIES"
	EnvCI               = "GARMIN_MCP_CI"
	EnvRoot             = "GARMIN_MCP_ROOT"
)

// DefaultProfileName is used when no profile is configured or selected.
const DefaultProfileName = "default"

// Defaults and bounds for limits.
const (
	DefaultMaxResponseBytes = 60000
	MinMaxResponseBytes     = 4096
	MaxMaxResponseBytes     = 10 << 20
	DefaultMaxRetries       = 3
	MaxMaxRetries           = 10
)

// Flags are command-line overrides; nil or empty means "not given".
type Flags struct {
	Profile     string
	AllowWrites *bool
	Toolsets    *string
	Discovery   *bool
	// NoConfirm skips confirmations; honored only when GARMIN_MCP_CI is true.
	NoConfirm bool
}

// Options are the inputs of Resolve. Zero values use the process environment.
type Options struct {
	Flags Flags
	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// ConfigPath defaults to DefaultPath.
	ConfigPath string
	// Cwd defaults to the working directory; Home to the user's home.
	Cwd, Home string
}

// ResolvedProfile is the effective profile. It never holds secrets.
type ResolvedProfile struct {
	Name          string
	AllowWrites   bool
	AllowNoElicit bool
	// PinSource is the .garmin-mcp.toml path when a pin was applied.
	PinSource string
	// Problems are user-fixable issues doctor reports; the server still starts.
	Problems []string
}

// Info maps the profile to the tool contract's ProfileInfo.
func (p ResolvedProfile) Info() tool.ProfileInfo {
	return tool.ProfileInfo{
		Name:          p.Name,
		AllowWrites:   p.AllowWrites,
		PinSource:     p.PinSource,
		AllowNoElicit: p.AllowNoElicit,
	}
}

// Resolved is the effective runtime configuration.
type Resolved struct {
	Profile            ResolvedProfile
	Toolsets           []tool.Toolset
	Discovery          bool
	MaxResponseBytes   int
	MaxRetries         int
	CI                 bool
	NoConfirm          bool
	AllowProfileSwitch bool
	// Config is the loaded config file, for per-call profile switching.
	Config   *Config
	Warnings []string
	// WritesLocked forbids turning writes on at runtime: a pin sets
	// allow_writes = false, or an untrusted pin chose the profile.
	WritesLocked bool
}

// Resolve loads the config file and project pin and applies precedence:
// flag > env > pin > profile > default. A pin can only narrow writes.
func Resolve(opts Options) (*Resolved, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	home, cwd, err := dirs(opts, getenv)
	if err != nil {
		return nil, err
	}
	path := opts.ConfigPath
	if path == "" {
		if path, err = DefaultPath(getenv); err != nil {
			return nil, err
		}
	}
	cfg, warnings, err := Load(path)
	if err != nil {
		return nil, err
	}
	pin, err := FindPin(cwd, home)
	if err != nil {
		return nil, err
	}
	r := &Resolved{Config: cfg, AllowProfileSwitch: cfg.AllowProfileSwitch, Warnings: warnings}
	if err := r.resolveProfile(opts.Flags, getenv, cfg, pin); err != nil {
		return nil, err
	}
	if err := r.resolveModes(opts.Flags, getenv); err != nil {
		return nil, err
	}
	return r, nil
}

func dirs(opts Options, getenv func(string) string) (home, cwd string, err error) {
	home, cwd = opts.Home, opts.Cwd
	if home == "" {
		if home = getenv("HOME"); home == "" {
			if home, err = os.UserHomeDir(); err != nil {
				return "", "", fmt.Errorf("resolve: home: %w", err)
			}
		}
	}
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return "", "", fmt.Errorf("resolve: cwd: %w", err)
		}
	}
	return home, cwd, nil
}

func (r *Resolved) resolveProfile(f Flags, getenv func(string) string, cfg *Config, pin *Pin) error {
	trusted := pin != nil && slices.ContainsFunc(cfg.TrustedPins, func(d string) bool { return sameDir(d, pin.Dir()) })

	name, pinSelected := "", false
	switch {
	case f.Profile != "":
		name = f.Profile
	case getenv(EnvProfile) != "":
		name = strings.TrimSpace(getenv(EnvProfile))
	case pin != nil && pin.Profile != "":
		target, ok := cfg.Profiles[pin.Profile]
		switch {
		case !ok:
			return fmt.Errorf("pin %s: profile %q is not defined in the config", pin.Path, pin.Profile)
		case trusted || !target.AllowWrites:
			name, pinSelected = pin.Profile, true
		default:
			r.Warnings = append(r.Warnings, fmt.Sprintf("ignoring profile %q from untrusted pin %s: it allows writes; add the directory to trusted_pins to allow this", pin.Profile, pin.Path))
		}
	}
	if err := r.loadProfile(name, cfg); err != nil {
		return err
	}
	if err := r.resolveWrites(f, getenv, pin); err != nil {
		return err
	}
	if pinSelected && !trusted {
		if r.Profile.AllowWrites {
			r.Warnings = append(r.Warnings, fmt.Sprintf("ignoring profile %q from untrusted pin %s: writes are on (%s or --allow-writes); add the directory to trusted_pins to allow this", pin.Profile, pin.Path, EnvAllowWrites))
			pinSelected = false
			r.Profile = ResolvedProfile{}
			if err := r.loadProfile("", cfg); err != nil {
				return err
			}
			if err := r.resolveWrites(f, getenv, pin); err != nil {
				return err
			}
		} else {
			r.WritesLocked = true
			r.Warnings = append(r.Warnings, fmt.Sprintf("writes locked: profile %q was chosen by untrusted pin %s", pin.Profile, pin.Path))
		}
	}
	if pinSelected {
		r.Profile.PinSource = pin.Path
	}
	return nil
}

// loadProfile sets r.Profile to the named profile; "" selects the default
// profile, the only one, or an implicit "default" profile.
func (r *Resolved) loadProfile(name string, cfg *Config) error {
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name == "" && len(cfg.Profiles) == 1 {
		name = cfg.ProfileNames()[0]
	}
	if name == "" {
		name = DefaultProfileName
	}
	prof, ok := cfg.Profiles[name]
	if !ok && name != DefaultProfileName {
		return fmt.Errorf("profile %q is not defined (known: %s)", name, strings.Join(cfg.ProfileNames(), ", "))
	}
	r.Profile = ResolvedProfile{
		Name:          name,
		AllowWrites:   prof.AllowWrites,
		AllowNoElicit: prof.AllowNoElicit,
	}
	return nil
}

// resolveWrites applies flag > env > profile, then lets the pin narrow.
func (r *Resolved) resolveWrites(f Flags, getenv func(string) string, pin *Pin) error {
	allow := r.Profile.AllowWrites
	if v, ok, err := envBool(getenv, EnvAllowWrites); err != nil {
		return err
	} else if ok {
		allow = v
	}
	if f.AllowWrites != nil {
		allow = *f.AllowWrites
	}
	if pin != nil && pin.AllowWrites != nil {
		if *pin.AllowWrites {
			r.Warnings = append(r.Warnings, fmt.Sprintf("ignoring allow_writes = true in pin %s: a pin can only disable writes", pin.Path))
		} else {
			if allow {
				r.Warnings = append(r.Warnings, fmt.Sprintf("writes disabled by pin %s", pin.Path))
			}
			allow = false
			r.WritesLocked = true
			r.Profile.PinSource = pin.Path
		}
	}
	r.Profile.AllowWrites = allow
	return nil
}

func (r *Resolved) resolveModes(f Flags, getenv func(string) string) error {
	expr := getenv(EnvToolsets)
	if f.Toolsets != nil {
		expr = *f.Toolsets
	}
	ts, err := ParseToolsets(expr)
	if err != nil {
		return err
	}
	r.Toolsets = ts

	discovery, _, err := envBool(getenv, EnvDiscovery)
	if err != nil {
		return err
	}
	if f.Discovery != nil {
		discovery = *f.Discovery
	}
	r.Discovery = discovery

	if r.MaxResponseBytes, err = envInt(getenv, EnvMaxResponseBytes, DefaultMaxResponseBytes, MinMaxResponseBytes, MaxMaxResponseBytes); err != nil {
		return err
	}
	if r.MaxRetries, err = envInt(getenv, EnvMaxRetries, DefaultMaxRetries, 0, MaxMaxRetries); err != nil {
		return err
	}
	if r.CI, _, err = envBool(getenv, EnvCI); err != nil {
		return err
	}
	r.NoConfirm = f.NoConfirm && r.CI
	if f.NoConfirm && !r.CI {
		r.Warnings = append(r.Warnings, "--no-confirm ignored: it takes effect only with "+EnvCI+"=1")
	}
	return nil
}

func envBool(getenv func(string) string, name string) (value, set bool, err error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return false, false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false, fmt.Errorf("%s=%q: want true/false/1/0", name, raw)
	}
	return v, true, nil
}

func envInt(getenv func(string) string, name string, def, lo, hi int) (int, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%s=%q: want an integer in [%d, %d]", name, raw, lo, hi)
	}
	return v, nil
}
