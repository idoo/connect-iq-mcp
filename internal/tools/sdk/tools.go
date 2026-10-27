package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/sim"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// maxManifest caps manifest.xml reads.
const maxManifest = 1 << 20

func init() {
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_info", Toolset: Toolset, Class: tool.Read, Idempotent: true, Local: true,
		Title:       "Connect IQ SDK info",
		Description: "Installed Connect IQ SDKs, the active one and which source picked it (arg/env/current-sdk.cfg/newest), Java, data root and device count.",
	}, sdkInfo))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_devices_list", Toolset: Toolset, Class: tool.Read, Idempotent: true, Local: true,
		Title:       "List devices",
		Description: "Installed Connect IQ devices: id, name, shape, resolution, bpp, touch, max CIQ version. Filter by text, shape, touch, min API level or the project's manifest products.",
		Enums:       map[string][]any{"shape": {"round", "semiround", "rectangle", "semioctagon"}},
	}, sdkDevicesList))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_device_get", Toolset: Toolset, Class: tool.Read, Idempotent: true, Local: true,
		Title:       "Get device",
		Description: "Full summary of one installed device: display, buttons, touch, app-type memory limits, CIQ version, languages.",
	}, sdkDeviceGet))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_manifest_get", Toolset: Toolset, Class: tool.Read, Idempotent: true, Local: true,
		Title:       "Read manifest.xml",
		Description: "Parse a project's manifest.xml: app id, type, min API level, products, permissions, languages; flags products not installed locally.",
	}, sdkManifestGet))
	tool.Register(tool.New(tool.Meta{
		// Local project edits: usable in the default read-only mode.
		Name: "sdk_manifest_set_products", Toolset: Toolset, Class: tool.Write, Local: true, AvailableReadOnly: true,
		Title:       "Edit manifest products",
		Description: "Add/remove <iq:product> entries in manifest.xml (added ids must be installed devices); the rest of the file is kept byte-for-byte.",
	}, sdkManifestSetProducts))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_build", Toolset: Toolset, Class: tool.Write, Local: true, AvailableReadOnly: true,
		Title:       "Build with monkeyc",
		Description: "monkeyc build: one device PRG, or device \"all\" for the store .iq (never replaces an .iq without overwrite). Key defaults to GARMIN_MCP_KEY. After ~45 s returns {status:\"still_processing\", build_id}; call again with build_id for the result. Structured diagnostics.",
	}, sdkBuild))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_key_generate", Toolset: Toolset, Class: tool.Write, Local: true, AvailableReadOnly: true,
		Title:       "Generate developer key",
		Description: "Create an RSA-4096 DER developer key (0600) only for a brand-new app; updates must be signed with the key of the first upload (use GARMIN_MCP_KEY). In a git repo the path must be gitignored. Never overwrites. Back it up.",
	}, sdkKeyGenerate))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_key_check", Toolset: Toolset, Class: tool.Read, Idempotent: true, Local: true,
		Title:       "Check developer key",
		Description: "Validate a developer key: PKCS#8 DER, RSA size, file permissions, SHA-256 fingerprint of the public key. Never returns key bytes.",
	}, sdkKeyCheck))
}

// sdkEnv is the resolved SDK context shared by tools.
type sdkEnv struct {
	DataRoot string
	Res      ciqsdk.Resolved
}

func dataRoot() (string, error) { return ciqsdk.DataRoot(os.Getenv) }

func resolveSDK(arg string) (sdkEnv, error) {
	dr, err := dataRoot()
	if err != nil {
		return sdkEnv{}, err
	}
	r, err := ciqsdk.Resolve(ciqsdk.ResolveOpts{Arg: arg, DataRoot: dr, Getenv: os.Getenv})
	if err != nil {
		return sdkEnv{}, tool.InvalidArgs("%v", err)
	}
	return sdkEnv{DataRoot: dr, Res: r}, nil
}

// --- sdk_info ---

type infoIn struct {
	SDK string `json:"sdk,omitempty" jsonschema:"installed SDK version or dir name, e.g. 9.2.0; a custom SDK only via GARMIN_MCP_SDK"`
}

type infoOut struct {
	DataRoot     string           `json:"data_root"`
	Active       *ciqsdk.Resolved `json:"active,omitempty"`
	ActiveError  string           `json:"active_error,omitempty"`
	Installed    []ciqsdk.SDK     `json:"installed"`
	Newest       string           `json:"newest,omitempty"`
	CurrentCfg   string           `json:"current_sdk_cfg,omitempty"`
	Java         *ciqsdk.Java     `json:"java,omitempty"`
	JavaError    string           `json:"java_error,omitempty"`
	DeviceCount  int              `json:"device_count"`
	DeviceErrors []string         `json:"device_warnings,omitempty"`
}

func sdkInfo(ctx context.Context, _ *tool.Env, in infoIn) (*tool.Result, error) {
	dr, err := dataRoot()
	if err != nil {
		return nil, err
	}
	out := infoOut{DataRoot: dr}
	if out.Installed, err = ciqsdk.ListSDKs(dr); err != nil {
		return nil, err
	}
	if out.Installed == nil {
		out.Installed = []ciqsdk.SDK{}
	} else {
		out.Newest = out.Installed[0].Version
	}
	if b, err := os.ReadFile(filepath.Join(dr, "current-sdk.cfg")); err == nil { //nolint:gosec // G304: fixed file name under the Connect IQ data root
		out.CurrentCfg, _ = ciqsdk.ParseCurrentCfg(b, dr)
	}
	if r, err := ciqsdk.Resolve(ciqsdk.ResolveOpts{Arg: in.SDK, DataRoot: dr, Getenv: os.Getenv}); err != nil {
		out.ActiveError = err.Error()
	} else {
		out.Active = &r
	}
	if j, err := ciqsdk.FindJava(ctx, os.Getenv); err != nil {
		out.JavaError = err.Error()
	} else {
		out.Java = &j
	}
	devs, warn, err := ciqsdk.ListDevices(dr)
	if err != nil {
		return nil, err
	}
	out.DeviceCount, out.DeviceErrors = len(devs), warn
	return tool.OK(out), nil
}

// --- sdk_devices_list ---

type devicesIn struct {
	Query      string `json:"query,omitempty" jsonschema:"part of the id or display name; case, accents, ® and spaces are ignored (\"Fenix 7\" finds fenix7)"`
	Shape      string `json:"shape,omitempty" jsonschema:"screen shape"`
	Touch      *bool  `json:"touch,omitempty" jsonschema:"only touch (true) or non-touch (false) devices"`
	MinAPI     string `json:"min_api,omitempty" jsonschema:"minimum Connect IQ version, e.g. 4.2.0"`
	InManifest string `json:"in_manifest,omitempty" jsonschema:"manifest.xml (or project dir) inside the root; keep only its products"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max devices (default 200)"`
}

type deviceRow struct {
	ID           string            `json:"id"`
	DisplayName  string            `json:"display_name"`
	Shape        string            `json:"shape,omitempty"`
	Resolution   ciqsdk.Resolution `json:"resolution"`
	BitsPerPixel int               `json:"bits_per_pixel,omitempty"`
	DisplayType  string            `json:"display_type,omitempty"`
	Touch        *bool             `json:"touch,omitempty"`
	CIQVersion   string            `json:"connect_iq_version,omitempty"`
}

// minAPIRE is the accepted min_api shape: 1 to 3 dot-separated numbers.
var minAPIRE = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){0,2}$`)

func sdkDevicesList(_ context.Context, env *tool.Env, in devicesIn) (*tool.Result, error) {
	if in.MinAPI != "" && !minAPIRE.MatchString(in.MinAPI) {
		return nil, tool.InvalidArgs("min_api %q must look like 4.2.0", in.MinAPI)
	}
	dr, err := dataRoot()
	if err != nil {
		return nil, err
	}
	devs, warn, err := ciqsdk.ListDevices(dr)
	if err != nil {
		return nil, err
	}
	var only map[string]bool
	var missing []string
	if in.InManifest != "" {
		m, _, err := readManifest(env, in.InManifest)
		if err != nil {
			return nil, err
		}
		only = map[string]bool{}
		for _, p := range m.Products {
			only[p] = true
		}
		have := map[string]bool{}
		for _, d := range devs {
			have[d.ID] = true
		}
		for _, p := range m.Products {
			if !have[p] {
				missing = append(missing, p)
			}
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 200
	}
	q := ciqsdk.NormDeviceName(in.Query)
	rows := []deviceRow{}
	total := 0
	for _, d := range FilterDevices(devs, q, in.Shape, in.Touch, in.MinAPI, only) {
		total++
		if len(rows) < limit {
			rows = append(rows, deviceRow{d.ID, d.DisplayName, d.Shape, d.Resolution, d.BitsPerPixel, d.DisplayType, d.Touch, d.CIQVersion})
		}
	}
	out := map[string]any{"devices": rows, "count": len(rows), "total": total}
	if len(missing) > 0 {
		out["manifest_products_not_installed"] = missing
	}
	if len(warn) > 0 {
		out["warnings"] = warn
	}
	return tool.OK(out), nil
}

// FilterDevices applies the sdk_devices_list filters (q folded by
// ciqsdk.NormDeviceName).
func FilterDevices(devs []ciqsdk.Device, q, shape string, touch *bool, minAPI string, only map[string]bool) []ciqsdk.Device {
	var out []ciqsdk.Device
	for _, d := range devs {
		if q != "" && !strings.Contains(ciqsdk.NormDeviceName(d.ID), q) && !strings.Contains(ciqsdk.NormDeviceName(d.DisplayName), q) {
			continue
		}
		if shape != "" && !strings.EqualFold(d.Shape, shape) {
			continue
		}
		if touch != nil && (d.Touch == nil || *d.Touch != *touch) {
			continue
		}
		if minAPI != "" && ciqsdk.CompareVersion(d.CIQVersion, minAPI) < 0 {
			continue
		}
		if only != nil && !only[d.ID] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// --- sdk_device_get ---

type deviceIn struct {
	ID string `json:"id" jsonschema:"device id as in manifest products, e.g. fenix7"`
}

func sdkDeviceGet(_ context.Context, _ *tool.Env, in deviceIn) (*tool.Result, error) {
	dr, err := dataRoot()
	if err != nil {
		return nil, err
	}
	id, note, err := ResolveDevice(dr, in.ID)
	if err != nil {
		return nil, err
	}
	d, err := ciqsdk.LoadDevice(dr, id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, missingDevice(dr, id)
	}
	if err != nil {
		return nil, err
	}
	return withNote(tool.OK(d), note), nil
}

// maxCandidates caps the device ids an ambiguous name lists.
const maxCandidates = 8

// ResolveDevice maps a device argument to an installed device id. An
// installed id is taken as is (note ""). Otherwise a free-text name ("Fenix
// 7") selecting exactly one installed device is accepted, with a note naming
// the mapping; several matches or none are invalid arguments. A well-formed id
// that matches nothing goes through missingDevice (unknown, or not downloaded).
func ResolveDevice(dr, arg string) (id, note string, err error) {
	if ciqsdk.ValidDeviceID(arg) {
		if _, err := ciqsdk.LoadDevice(dr, arg); err == nil {
			return arg, "", nil
		}
	}
	devs, _, err := ciqsdk.ListDevices(dr)
	if err != nil {
		return "", "", err
	}
	m := ciqsdk.MatchDevices(devs, arg)
	switch {
	case len(m) == 1:
		return m[0].ID, fmt.Sprintf("device %q → %s", arg, m[0].ID), nil
	case len(m) > 1:
		ids := make([]string, 0, maxCandidates)
		for _, d := range m[:min(len(m), maxCandidates)] {
			ids = append(ids, d.ID)
		}
		more := ""
		if len(m) > maxCandidates {
			more = fmt.Sprintf(" (and %d more)", len(m)-maxCandidates)
		}
		return "", "", tool.WithNext(tool.InvalidArgs("device %q matches %d devices: %s%s; pass one id", arg, len(m), strings.Join(ids, ", "), more), tool.NextStep{Tool: "sdk_devices_list", Args: map[string]any{"query": arg}})
	case ciqsdk.ValidDeviceID(arg):
		return "", "", missingDevice(dr, arg)
	}
	return "", "", tool.WithNext(tool.InvalidArgs("unknown device %q; list ids with sdk_devices_list", arg), tool.NextStep{Tool: "sdk_devices_list"})
}

// withNote adds note (if any) as a result warning, so the agent sees which
// device a free-text name became.
func withNote(r *tool.Result, note string) *tool.Result {
	if note != "" {
		r.Warn(note)
	}
	return r
}

// missingDevice explains a device with no compiler.json: an id the SDK
// Manager knows (its Devices/<id> folder exists) needs a download; any other
// id is unknown, e.g. a guessed "fenix8".
func missingDevice(dr, id string) error {
	if st, err := os.Stat(filepath.Join(ciqsdk.DevicesDir(dr), id)); err == nil && st.IsDir() {
		return tool.InvalidArgs("device %q is not installed under %s; download it with SdkManager", id, ciqsdk.DevicesDir(dr))
	}
	return tool.WithNext(tool.InvalidArgs("unknown device %s; list valid ids with sdk_devices_list", id), tool.NextStep{Tool: "sdk_devices_list"})
}

// --- manifest ---

type manifestIn struct {
	Path string `json:"path,omitempty" jsonschema:"manifest.xml or project dir inside the root (default: root)"`
}

// manifestPath resolves a manifest.xml path (a dir means <dir>/manifest.xml,
// and must be a Connect IQ project).
func manifestPath(env *tool.Env, p string) (string, error) {
	if p == "" {
		p = "."
	}
	abs, err := env.Root.Resolve(p)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		if err := CheckProject(env, "path", p, abs); err != nil {
			return "", err
		}
		abs = filepath.Join(abs, "manifest.xml")
	}
	return abs, nil
}

// maxProjectHints caps the project dirs a not-a-project error lists.
const maxProjectHints = 5

// CheckProject refuses a project dir (argument arg, value val, resolved
// pdir) that is missing or has no manifest.xml, naming the dirs under the
// root that are projects. A value repeating the root's own name (the agent
// passing the project's name from inside it) is told to leave arg out.
func CheckProject(env *tool.Env, arg, val, pdir string) error {
	root, err := env.Root.Resolve(".")
	if err != nil {
		return err
	}
	st, err := os.Stat(pdir)
	if err == nil && st.IsDir() {
		if _, err := os.Stat(filepath.Join(pdir, "manifest.xml")); err == nil {
			return nil
		}
	}
	if val != "" && val != "." && filepath.Clean(val) == filepath.Base(root) {
		if _, err := os.Stat(filepath.Join(root, "manifest.xml")); err == nil {
			return tool.InvalidArgs("%s %q names the root folder itself; paths are relative to the root %s, so leave %s out", arg, val, root, arg)
		}
	}
	what := fmt.Sprintf("%s is not a Connect IQ project (no manifest.xml)", pdir)
	if err != nil || !st.IsDir() {
		what = fmt.Sprintf("%s %s is not a directory (paths are relative to the root %s)", arg, pdir, root)
	}
	found := projectDirs(root)
	if len(found) == 0 {
		return tool.InvalidArgs("%s; no folder under %s has a manifest.xml: open the project folder (the one with manifest.xml) as the workspace root", what, root)
	}
	return tool.InvalidArgs("%s; pass %s:\"<subdir>\", projects under the root: %s", what, arg, strings.Join(found, ", "))
}

// projectDirs lists up to maxProjectHints dirs (relative to root, at most
// two levels down) that contain manifest.xml, skipping hidden and build dirs.
func projectDirs(root string) []string {
	var out []string
	var walk func(rel string, depth int)
	walk = func(rel string, depth int) {
		ents, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return
		}
		for _, e := range ents {
			n := e.Name()
			if len(out) >= maxProjectHints || !e.IsDir() || strings.HasPrefix(n, ".") || n == "bin" || n == "node_modules" {
				continue
			}
			sub := filepath.Join(rel, n)
			if _, err := os.Stat(filepath.Join(root, sub, "manifest.xml")); err == nil {
				out = append(out, filepath.ToSlash(sub))
			} else if depth < 2 {
				walk(sub, depth+1)
			}
		}
	}
	walk("", 1)
	return out
}

func readManifest(env *tool.Env, p string) (ciqsdk.Manifest, []byte, error) {
	abs, err := manifestPath(env, p)
	if err != nil {
		return ciqsdk.Manifest{}, nil, err
	}
	st, err := env.Root.Stat(abs)
	if err != nil {
		return ciqsdk.Manifest{}, nil, err
	}
	if st.Size() > maxManifest {
		return ciqsdk.Manifest{}, nil, tool.InvalidArgs("%s is larger than %d bytes", abs, maxManifest)
	}
	b, err := env.Root.ReadFile(abs)
	if err != nil {
		return ciqsdk.Manifest{}, nil, err
	}
	m, err := ciqsdk.ParseManifest(b)
	if err != nil {
		return ciqsdk.Manifest{}, nil, tool.InvalidArgs("%v", err)
	}
	return m, b, nil
}

func installedSet(dr string) (map[string]ciqsdk.Device, error) {
	devs, _, err := ciqsdk.ListDevices(dr)
	if err != nil {
		return nil, err
	}
	m := make(map[string]ciqsdk.Device, len(devs))
	for _, d := range devs {
		m[d.ID] = d
	}
	return m, nil
}

func sdkManifestGet(_ context.Context, env *tool.Env, in manifestIn) (*tool.Result, error) {
	m, _, err := readManifest(env, in.Path)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"manifest": m, "product_count": len(m.Products)}
	if dr, err := dataRoot(); err == nil {
		if inst, err := installedSet(dr); err == nil && len(inst) > 0 {
			var missing []string
			for _, p := range m.Products {
				if _, ok := inst[p]; !ok {
					missing = append(missing, p)
				}
			}
			if len(missing) > 0 {
				out["products_not_installed"] = missing
			}
		}
	}
	return tool.OK(out), nil
}

type setProductsIn struct {
	Path   string   `json:"path,omitempty" jsonschema:"manifest.xml or project dir inside the root (default: root)"`
	Add    []string `json:"add,omitempty" jsonschema:"device ids to add (must be installed)"`
	Remove []string `json:"remove,omitempty" jsonschema:"device ids to remove"`
}

func sdkManifestSetProducts(_ context.Context, env *tool.Env, in setProductsIn) (*tool.Result, error) {
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return nil, tool.InvalidArgs("give add and/or remove")
	}
	abs, err := manifestPath(env, in.Path)
	if err != nil {
		return nil, err
	}
	_, b, err := readManifest(env, abs)
	if err != nil {
		return nil, err
	}
	if len(in.Add) > 0 {
		dr, err := dataRoot()
		if err != nil {
			return nil, err
		}
		inst, err := installedSet(dr)
		if err != nil {
			return nil, err
		}
		var unknown []string
		for _, p := range in.Add {
			if _, ok := inst[strings.TrimSpace(p)]; !ok {
				unknown = append(unknown, p)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return nil, tool.InvalidArgs("not installed devices: %s (see sdk_devices_list)", strings.Join(unknown, ", "))
		}
	}
	nb, added, removed, err := ciqsdk.SetProducts(b, in.Add, in.Remove)
	if err != nil {
		return nil, tool.InvalidArgs("%v", err)
	}
	if len(added) == 0 && len(removed) == 0 {
		return tool.Already(map[string]any{"path": abs, "added": []string{}, "removed": []string{}}), nil
	}
	if filepath.Base(abs) != "manifest.xml" {
		return nil, tool.InvalidArgs("%s is not a manifest.xml", abs)
	}
	if err := sim.CheckDest(abs, true, ".xml"); err != nil {
		return nil, tool.InvalidArgs("%v", err)
	}
	st, err := env.Root.Stat(abs)
	if err != nil {
		return nil, err
	}
	if err := env.Root.WriteFile(abs, nb, st.Mode().Perm()); err != nil {
		return nil, err
	}
	m, _ := ciqsdk.ParseManifest(nb)
	return tool.OK(map[string]any{"path": abs, "added": nonNil(added), "removed": nonNil(removed), "product_count": len(m.Products)}), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
