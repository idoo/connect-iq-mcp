package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/idoo/connect-iq-mcp/internal/state"
)

// Config is the user's config file (~/.config/connect-iq-mcp/config.toml).
type Config struct {
	// DefaultProfile names the profile used when nothing else selects one.
	DefaultProfile string `toml:"default_profile,omitempty"`
	// TrustedPins lists absolute directories whose .connect-iq-mcp.toml may select any profile.
	TrustedPins []string `toml:"trusted_pins,omitempty"`
	// Profiles maps profile names to settings. Credentials live in the OS
	// keychain, or a 0600 state file when no keychain is available (see
	// internal/credstore), never here.
	Profiles map[string]Profile `toml:"profiles,omitempty"`
}

// Profile is one named Connect IQ developer account's settings. It never
// holds secrets.
type Profile struct {
	AllowWrites   bool `toml:"allow_writes,omitempty"`
	AllowNoElicit bool `toml:"allow_no_elicit,omitempty"`
	// AutoRenew lets connect-iq-mcp silently renew expiring Garmin sessions with the
	// dedicated browser profile. nil means true.
	AutoRenew *bool `toml:"auto_renew,omitempty"`
}

// AutoRenewEnabled reports the effective auto_renew setting (default true).
func (p Profile) AutoRenewEnabled() bool { return p.AutoRenew == nil || *p.AutoRenew }

// DefaultPath returns the config file path: $XDG_CONFIG_HOME/connect-iq-mcp/config.toml,
// ~/.config/connect-iq-mcp/config.toml, or %AppData%\connect-iq-mcp\config.toml on Windows.
func DefaultPath(getenv func(string) string) (string, error) {
	if runtime.GOOS == "windows" {
		if d := getenv("AppData"); d != "" {
			return filepath.Join(d, "connect-iq-mcp", "config.toml"), nil
		}
	}
	if d := getenv("XDG_CONFIG_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "connect-iq-mcp", "config.toml"), nil
	}
	home := getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config path: %w", err)
		}
		home = h
	}
	return filepath.Join(home, ".config", "connect-iq-mcp", "config.toml"), nil
}

// Load reads the config at path. A missing file yields an empty Config.
// Group- or world-writable files are refused; files readable by others load
// with a warning. Unknown keys are rejected so typos never silently drop
// settings.
func Load(path string) (*Config, []string, error) {
	data, info, err := readChecked(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := checkDir(filepath.Dir(path)); err != nil {
		return nil, nil, err
	}
	warnings := permWarnings(path, info)
	cfg, legacy, err := Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, w := range legacy {
		warnings = append(warnings, path+": "+w)
	}
	return cfg, warnings, nil
}

// fileConfig is Config plus retired keys that still load so old config
// files keep working; each one present yields a warning.
type fileConfig struct {
	Config
	AllowProfileSwitch *bool `toml:"allow_profile_switch"`
}

// Parse decodes and validates config TOML. The warnings name retired keys
// the file still sets.
func Parse(data []byte) (*Config, []string, error) {
	var fc fileConfig
	if err := decodeStrict(data, &fc); err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	if err := fc.Validate(); err != nil {
		return nil, nil, err
	}
	var warnings []string
	if fc.AllowProfileSwitch != nil {
		warnings = append(warnings, "allow_profile_switch is not supported and has no effect; remove it")
	}
	return &fc.Config, warnings, nil
}

// decodeStrict decodes TOML rejecting unknown keys, naming them in the error.
func decodeStrict(data []byte, v any) error {
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		keys := make([]string, 0, len(strict.Errors))
		for _, e := range strict.Errors {
			keys = append(keys, strings.Join(e.Key(), "."))
		}
		return fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return fmt.Errorf("line %d, column %d: %s", row, col, de.Error())
	}
	return err
}

// profileNameRE is the profile name rule: the name becomes a file name, a
// browser-profile directory and a keychain account, so it is kept lowercase
// (safe on case-insensitive filesystems) and free of separators.
var profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidateProfileName rejects names outside ^[a-z0-9][a-z0-9_-]{0,62}$.
func ValidateProfileName(name string) error {
	if !profileNameRE.MatchString(name) {
		return fmt.Errorf("invalid profile name %q: use 1-63 lowercase letters, digits, '-' or '_', starting with a letter or digit", name)
	}
	return nil
}

// Validate checks profile names and cross-references.
func (c *Config) Validate() error {
	var errs []error
	for _, name := range c.ProfileNames() {
		if err := ValidateProfileName(name); err != nil {
			errs = append(errs, fmt.Errorf("[profiles.%q]: %w", name, err))
		}
	}
	if c.DefaultProfile != "" {
		if err := ValidateProfileName(c.DefaultProfile); err != nil {
			errs = append(errs, fmt.Errorf("default_profile: %w", err))
		}
	}
	for _, p := range c.TrustedPins {
		if !filepath.IsAbs(p) {
			errs = append(errs, fmt.Errorf("trusted_pins entry %q must be an absolute path", p))
		}
	}
	return errors.Join(errs...)
}

// ProfileNames returns the profile names in sorted order.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Save validates cfg and writes it atomically to path with mode 0600,
// creating the parent directory with mode 0700.
func Save(path string, cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("save config: encode: %w", err)
	}
	return writeAtomic(path, data)
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	f, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("save config: chmod: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("save config: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("save config: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("save config: close: %w", err)
	}
	if err := state.Rename(tmp, path); err != nil {
		return fmt.Errorf("save config: rename: %w", err)
	}
	ok = true
	return nil
}

// readChecked reads a config-like file after refusing unsafe permissions.
func readChecked(path string) ([]byte, fs.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := checkWritable(path, info); err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the user's own config location
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, info, nil
}

// checkWritable refuses files that other users can modify (unix only).
func checkWritable(path string, info fs.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if why := othersCanWrite(info); why != "" {
		return fmt.Errorf("refusing %s: %s (mode %04o); run chmod 600 %s", path, why, info.Mode().Perm(), path)
	}
	return nil
}

// checkDir refuses a config directory other users can write to, since they
// could replace config.toml (unix only; sticky directories are exempt).
func checkDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if info.Mode()&fs.ModeSticky != 0 {
		return nil
	}
	if why := othersCanWrite(info); why != "" {
		return fmt.Errorf("refusing %s: %s (mode %04o); run chmod 700 %s", dir, why, info.Mode().Perm(), dir)
	}
	return nil
}

// Lookups behind the user-private-group rule; tests replace them.
var (
	getgid          = os.Getgid
	lookupGroupName = func(gid int) (string, error) {
		g, err := user.LookupGroupId(strconv.Itoa(gid))
		if err != nil {
			return "", err
		}
		return g.Name, nil
	}
	currentUserName = func() (string, error) {
		u, err := user.Current()
		if err != nil {
			return "", err
		}
		return u.Username, nil
	}
)

// othersCanWrite says why another user could modify info's file ("" = only
// the owner can). World-write is always refused. Group-write is allowed only
// for the user's private group (Debian/Fedora umask 0002): the file's gid is
// the process gid and that group is named after the current user.
func othersCanWrite(info fs.FileInfo) string {
	perm := info.Mode().Perm()
	switch {
	case perm&0o002 != 0:
		return "writable by others"
	case perm&0o020 != 0 && !userPrivateGroup(info):
		return "writable by group"
	}
	return ""
}

func userPrivateGroup(info fs.FileInfo) bool {
	gid, ok := fileGid(info)
	if !ok || gid != getgid() {
		return false
	}
	group, err := lookupGroupName(gid)
	if err != nil {
		return false
	}
	name, err := currentUserName()
	return err == nil && name != "" && group == name
}

func permWarnings(path string, info fs.FileInfo) []string {
	if runtime.GOOS == "windows" || info.Mode().Perm()&0o044 == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%s is readable by other users (mode %04o); run chmod 600 %s", path, info.Mode().Perm(), path)}
}
