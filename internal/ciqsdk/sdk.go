// Package ciqsdk locates Connect IQ SDKs, devices and Java, parses manifests,
// runs monkeyc and manages developer keys. It never touches the network.
package ciqsdk

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Resolution sources, in precedence order.
const (
	SourceArg    = "arg"
	SourceEnv    = "env"
	SourceConfig = "current-sdk.cfg"
	SourceNewest = "newest"
)

// Env vars naming an SDK (path or version); GARMIN_MCP_SDK wins over CIQ_SDK.
var EnvVars = []string{"GARMIN_MCP_SDK", "CIQ_SDK"}

// SDK is one installed SDK.
type SDK struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Dir     string `json:"dir"`
}

// Bin returns the SDK's bin directory.
func (s SDK) Bin() string { return filepath.Join(s.Path, "bin") }

// Resolved is the SDK picked by [Resolve] and why.
type Resolved struct {
	SDK    SDK    `json:"sdk"`
	Source string `json:"source"`
	Detail string `json:"detail,omitempty"`
}

// DataRoot returns the ConnectIQ data root (Sdks/, Devices/, current-sdk.cfg)
// for the running OS.
func DataRoot(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	switch runtime.GOOS {
	case "windows":
		if a := getenv("APPDATA"); a != "" {
			return filepath.Join(a, "Garmin", "ConnectIQ"), nil
		}
		return "", errors.New("APPDATA is not set")
	}
	home := getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = h
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Garmin", "ConnectIQ"), nil
	}
	return filepath.Join(home, ".Garmin", "ConnectIQ"), nil
}

var sdkDirRE = regexp.MustCompile(`^connectiq-sdk-(?:mac|lin|win)-(\d+\.\d+\.\d+)`)

// VersionFromDir parses the version from an SDK dir name
// (connectiq-sdk-<os>-<X.Y.Z>-<date>-<hash>), else "".
func VersionFromDir(name string) string {
	if m := sdkDirRE.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// CompareVersion compares dotted numeric versions ("10.0.0" > "9.2.0").
// Non-numeric parts compare as strings.
func CompareVersion(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		xi, ex := strconv.Atoi(x)
		yi, ey := strconv.Atoi(y)
		if x == "" {
			xi, ex = 0, nil
		}
		if y == "" {
			yi, ey = 0, nil
		}
		if ex == nil && ey == nil {
			if xi != yi {
				if xi < yi {
					return -1
				}
				return 1
			}
			continue
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// ListSDKs returns installed SDKs under <dataRoot>/Sdks, newest first.
// A missing Sdks dir yields an empty list.
func ListSDKs(dataRoot string) ([]SDK, error) {
	dir := filepath.Join(dataRoot, "Sdks")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SDK
	for _, e := range ents {
		v := VersionFromDir(e.Name())
		if v == "" || !e.IsDir() {
			continue
		}
		out = append(out, SDK{Path: filepath.Join(dir, e.Name()), Version: v, Dir: e.Name()})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := CompareVersion(out[i].Version, out[j].Version); c != 0 {
			return c > 0
		}
		return out[i].Dir > out[j].Dir
	})
	return out, nil
}

// ParseCurrentCfg parses current-sdk.cfg content: a single path, trimmed;
// a relative path resolves against cfgDir.
func ParseCurrentCfg(b []byte, cfgDir string) (string, error) {
	p := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0])
	if p == "" {
		return "", errors.New("current-sdk.cfg is empty")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cfgDir, p)
	}
	return filepath.Clean(p), nil
}

// ValidateSDK checks an SDK root has a usable compiler (monkeybrains.jar or
// the monkeyc wrapper) and returns it with its version.
func ValidateSDK(path string) (SDK, error) {
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil {
		return SDK{}, fmt.Errorf("sdk %s: %w", path, err)
	}
	if !st.IsDir() {
		return SDK{}, fmt.Errorf("sdk %s is not a directory", path)
	}
	s := SDK{Path: path, Dir: filepath.Base(path), Version: VersionFromDir(filepath.Base(path))}
	if b, err := os.ReadFile(filepath.Join(path, "bin", "version.txt")); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			s.Version = v
		}
	}
	if !exists(filepath.Join(s.Bin(), "monkeybrains.jar")) && !exists(filepath.Join(s.Bin(), wrapperName("monkeyc"))) {
		return SDK{}, fmt.Errorf("sdk %s has neither bin/monkeybrains.jar nor bin/%s", path, wrapperName("monkeyc"))
	}
	return s, nil
}

// ResolveOpts configures [Resolve].
type ResolveOpts struct {
	// Arg is an explicit SDK path or installed version (e.g. "9.2.0").
	Arg      string
	DataRoot string
	Getenv   func(string) string
}

// Resolve picks the SDK: explicit arg → GARMIN_MCP_SDK/CIQ_SDK → current-sdk.cfg →
// newest installed. A source that is set but broken is an error, never a
// silent fall-through.
func Resolve(o ResolveOpts) (Resolved, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	installed, err := ListSDKs(o.DataRoot)
	if err != nil {
		return Resolved{}, err
	}
	if a := strings.TrimSpace(o.Arg); a != "" {
		s, err := pick(a, installed)
		if err != nil {
			return Resolved{}, fmt.Errorf("sdk argument: %w", err)
		}
		return Resolved{SDK: s, Source: SourceArg, Detail: a}, nil
	}
	for _, k := range EnvVars {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			s, err := pick(v, installed)
			if err != nil {
				return Resolved{}, fmt.Errorf("%s: %w", k, err)
			}
			return Resolved{SDK: s, Source: SourceEnv, Detail: k}, nil
		}
	}
	cfg := filepath.Join(o.DataRoot, "current-sdk.cfg")
	if b, err := os.ReadFile(cfg); err == nil {
		p, err := ParseCurrentCfg(b, o.DataRoot)
		if err != nil {
			return Resolved{}, fmt.Errorf("%s: %w", cfg, err)
		}
		s, err := ValidateSDK(p)
		if err != nil {
			return Resolved{}, fmt.Errorf("%s: %w", cfg, err)
		}
		return Resolved{SDK: s, Source: SourceConfig, Detail: cfg}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Resolved{}, fmt.Errorf("%s: %w", cfg, err)
	}
	for _, s := range installed {
		v, err := ValidateSDK(s.Path)
		if err != nil {
			return Resolved{}, fmt.Errorf("newest installed: %w", err)
		}
		return Resolved{SDK: v, Source: SourceNewest}, nil
	}
	return Resolved{}, fmt.Errorf("no Connect IQ SDK found under %s; install one with SdkManager or set GARMIN_MCP_SDK", filepath.Join(o.DataRoot, "Sdks"))
}

// pick resolves a path or an installed version string.
func pick(ref string, installed []SDK) (SDK, error) {
	if strings.ContainsAny(ref, `/\`) || filepath.IsAbs(ref) {
		return ValidateSDK(ref)
	}
	for _, s := range installed {
		if s.Version == ref || s.Dir == ref {
			return ValidateSDK(s.Path)
		}
	}
	// Prefix match ("9.2" → newest 9.2.x).
	for _, s := range installed {
		if strings.HasPrefix(s.Version, ref+".") {
			return ValidateSDK(s.Path)
		}
	}
	var vs []string
	for _, s := range installed {
		vs = append(vs, s.Version)
	}
	return SDK{}, fmt.Errorf("SDK %q is not installed (installed: %s)", ref, strings.Join(vs, ", "))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func wrapperName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".bat"
	}
	return n
}
