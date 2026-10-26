package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/idoo/ciq-mcp/internal/ciqsdk"
	"github.com/idoo/ciq-mcp/internal/tool"
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
		Name: "sdk_manifest_set_products", Toolset: Toolset, Class: tool.Write, Local: true, Confirm: tool.ConfirmByFlag,
		Title:       "Edit manifest products",
		Description: "Add/remove <iq:product> entries in manifest.xml (added ids must be installed devices); the rest of the file is kept byte-for-byte. Without confirm:true previews.",
	}, sdkManifestSetProducts))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_build", Toolset: Toolset, Class: tool.Write, Local: true,
		Title:       "Build PRG (monkeyc)",
		Description: "Compile the project for one device (debug, release or unit-test) with monkeyc; returns the PRG path and structured diagnostics.",
	}, sdkBuild))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_export", Toolset: Toolset, Class: tool.Write, Local: true,
		Title:       "Export store package (.iq)",
		Description: "Build the store .iq for all manifest products (monkeyc -e -r). Slow (minutes); returns diagnostics grouped by device.",
	}, sdkExport))
	tool.Register(tool.New(tool.Meta{
		Name: "sdk_key_generate", Toolset: Toolset, Class: tool.Write, Local: true, Confirm: tool.ConfirmByFlag,
		Title:       "Generate developer key",
		Description: "Create an RSA-4096 PKCS#8 DER developer key (0600). Refuses to overwrite. Store updates need the same key forever: back it up. Without confirm:true previews.",
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
	SDK string `json:"sdk,omitempty" jsonschema:"SDK path or installed version, e.g. 9.2.0"`
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
	if b, err := os.ReadFile(filepath.Join(dr, "current-sdk.cfg")); err == nil {
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
	Query      string `json:"query,omitempty" jsonschema:"case-insensitive substring of id or display name"`
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

func sdkDevicesList(_ context.Context, env *tool.Env, in devicesIn) (*tool.Result, error) {
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
	q := strings.ToLower(in.Query)
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

// FilterDevices applies the sdk_devices_list filters (q lowercased).
func FilterDevices(devs []ciqsdk.Device, q, shape string, touch *bool, minAPI string, only map[string]bool) []ciqsdk.Device {
	var out []ciqsdk.Device
	for _, d := range devs {
		if q != "" && !strings.Contains(strings.ToLower(d.ID), q) && !strings.Contains(strings.ToLower(d.DisplayName), q) {
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
	if !ciqsdk.ValidDeviceID(in.ID) {
		return nil, tool.InvalidArgs("invalid device id %q", in.ID)
	}
	d, err := ciqsdk.LoadDevice(dr, in.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, tool.InvalidArgs("device %q is not installed under %s; download it with SdkManager", in.ID, ciqsdk.DevicesDir(dr))
	}
	if err != nil {
		return nil, err
	}
	return tool.OK(d), nil
}

// --- manifest ---

type manifestIn struct {
	Path string `json:"path,omitempty" jsonschema:"manifest.xml or project dir inside the root (default: root)"`
}

// manifestPath resolves a manifest.xml path (a dir means <dir>/manifest.xml).
func manifestPath(env *tool.Env, p string) (string, error) {
	if p == "" {
		p = "."
	}
	abs, err := env.Root.Resolve(p)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		abs = filepath.Join(abs, "manifest.xml")
	}
	return abs, nil
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
	tool.ConfirmFlag
	Path   string   `json:"path,omitempty" jsonschema:"manifest.xml or project dir inside the root (default: root)"`
	Add    []string `json:"add,omitempty" jsonschema:"device ids to add (must be installed)"`
	Remove []string `json:"remove,omitempty" jsonschema:"device ids to remove"`
}

func sdkManifestSetProducts(ctx context.Context, env *tool.Env, in setProductsIn) (*tool.Result, error) {
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
	_, gate := tool.RequireConfirm(ctx, env, in.Confirm, tool.PlanPreview{
		Summary:  fmt.Sprintf("edit %s: add %d product(s), remove %d", abs, len(added), len(removed)),
		Affected: []string{abs},
		Intended: map[string]any{"add": added, "remove": removed},
	})
	if gate != nil {
		return gate, nil
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
