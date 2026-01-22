package ciqsdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// AppType is one app type a device supports with its memory limit (bytes).
type AppType struct {
	Type        string `json:"type"`
	MemoryLimit int64  `json:"memory_limit"`
}

// Resolution is a screen size in pixels.
type Resolution struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Device summarises Devices/<id>/compiler.json (+ simulator.json).
type Device struct {
	ID           string     `json:"id"`
	DisplayName  string     `json:"display_name"`
	Family       string     `json:"family,omitempty"`
	Group        string     `json:"group,omitempty"`
	Shape        string     `json:"shape,omitempty"`
	Resolution   Resolution `json:"resolution"`
	BitsPerPixel int        `json:"bits_per_pixel,omitempty"`
	DisplayType  string     `json:"display_type,omitempty"`
	Touch        *bool      `json:"touch,omitempty"`
	Buttons      []string   `json:"buttons,omitempty"`
	AppTypes     []AppType  `json:"app_types,omitempty"`
	CIQVersion   string     `json:"connect_iq_version,omitempty"`
	Languages    []string   `json:"languages,omitempty"`
	HasSimulator bool       `json:"has_simulator"`
	MaxPrgSpace  int64      `json:"max_prg_filespace,omitempty"`
}

type compilerJSON struct {
	DeviceID     string     `json:"deviceId"`
	DisplayName  string     `json:"displayName"`
	DeviceFamily string     `json:"deviceFamily"`
	DeviceGroup  string     `json:"deviceGroup"`
	DisplayType  string     `json:"displayType"`
	BitsPerPixel int        `json:"bitsPerPixel"`
	Resolution   Resolution `json:"resolution"`
	MaxPrg       int64      `json:"maxPrgFilespace"`
	AppTypes     []struct {
		Type        string `json:"type"`
		MemoryLimit int64  `json:"memoryLimit"`
	} `json:"appTypes"`
	PartNumbers []struct {
		ConnectIQVersion string `json:"connectIQVersion"`
		Languages        []struct {
			Code string `json:"code"`
		} `json:"languages"`
	} `json:"partNumbers"`
}

type simulatorJSON struct {
	Display struct {
		Shape   string `json:"shape"`
		IsTouch *bool  `json:"isTouch"`
	} `json:"display"`
	Keys []struct {
		ID string `json:"id"`
	} `json:"keys"`
}

var deviceIDRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidDeviceID reports whether id is a safe device directory name.
func ValidDeviceID(id string) bool { return deviceIDRE.MatchString(id) && id != "." && id != ".." }

// DevicesDir returns <dataRoot>/Devices.
func DevicesDir(dataRoot string) string { return filepath.Join(dataRoot, "Devices") }

// LoadDevice reads one device. A missing compiler.json is os.ErrNotExist.
func LoadDevice(dataRoot, id string) (Device, error) {
	if !ValidDeviceID(id) {
		return Device{}, fmt.Errorf("invalid device id %q", id)
	}
	dir := filepath.Join(DevicesDir(dataRoot), id)
	b, err := os.ReadFile(filepath.Join(dir, "compiler.json")) //nolint:gosec // G304: id passed ValidDeviceID; path stays under the Devices dir
	if err != nil {
		return Device{}, err
	}
	var c compilerJSON
	if err := json.Unmarshal(b, &c); err != nil {
		return Device{}, fmt.Errorf("device %s compiler.json: %w", id, err)
	}
	d := Device{
		ID: id, DisplayName: c.DisplayName, Family: c.DeviceFamily, Group: c.DeviceGroup,
		Resolution: c.Resolution, BitsPerPixel: c.BitsPerPixel, DisplayType: c.DisplayType, MaxPrgSpace: c.MaxPrg,
	}
	if c.DeviceID != "" {
		d.ID = c.DeviceID
	}
	for _, a := range c.AppTypes {
		d.AppTypes = append(d.AppTypes, AppType{Type: a.Type, MemoryLimit: a.MemoryLimit})
	}
	langs := map[string]bool{}
	for _, p := range c.PartNumbers {
		if p.ConnectIQVersion != "" && CompareVersion(p.ConnectIQVersion, d.CIQVersion) > 0 {
			d.CIQVersion = p.ConnectIQVersion
		}
		for _, l := range p.Languages {
			langs[l.Code] = true
		}
	}
	for l := range langs {
		d.Languages = append(d.Languages, l)
	}
	sort.Strings(d.Languages)
	if i := strings.IndexByte(c.DeviceFamily, '-'); i > 0 {
		d.Shape = c.DeviceFamily[:i]
	}
	if sb, err := os.ReadFile(filepath.Join(dir, "simulator.json")); err == nil { //nolint:gosec // G304: id passed ValidDeviceID; path stays under the Devices dir
		var s simulatorJSON
		if json.Unmarshal(sb, &s) == nil {
			d.HasSimulator = true
			if s.Display.Shape != "" {
				d.Shape = s.Display.Shape
			}
			d.Touch = s.Display.IsTouch
			for _, k := range s.Keys {
				if k.ID != "" {
					d.Buttons = append(d.Buttons, k.ID)
				}
			}
		}
	}
	return d, nil
}

// ListDevices loads every device dir with a compiler.json, sorted by id.
// Unreadable devices are skipped and returned as warnings.
func ListDevices(dataRoot string) ([]Device, []string, error) {
	ents, err := os.ReadDir(DevicesDir(dataRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var out []Device
	var warn []string
	for _, e := range ents {
		if !e.IsDir() || !ValidDeviceID(e.Name()) {
			continue
		}
		d, err := LoadDevice(dataRoot, e.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			warn = append(warn, err.Error())
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, warn, nil
}

// foldAccents maps accented Latin letters in device names to plain ones.
var foldAccents = strings.NewReplacer(
	"ā", "a", "á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a",
	"ē", "e", "é", "e", "è", "e", "ê", "e", "ë", "e",
	"ī", "i", "í", "i", "ì", "i", "î", "i", "ï", "i",
	"ō", "o", "ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o", "ø", "o",
	"ū", "u", "ú", "u", "ù", "u", "û", "u", "ü", "u", "ñ", "n", "ç", "c", "ý", "y",
)

// NormDeviceName folds a device id or display name for matching: lower
// case, no accents (fēnix → fenix), and only letters and digits, so ®, ™,
// spaces and punctuation never matter ("fēnix® 7" → "fenix7").
func NormDeviceName(s string) string {
	s = foldAccents.Replace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r > 0x7f && unicode.IsLetter(r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// MatchDevices returns the devices a free-text name q selects: those whose
// folded id or display name equals q's, else those containing it.
func MatchDevices(devs []Device, q string) []Device {
	n := NormDeviceName(q)
	if n == "" {
		return nil
	}
	var exact, part []Device
	for _, d := range devs {
		id, name := NormDeviceName(d.ID), NormDeviceName(d.DisplayName)
		switch {
		case id == n || name == n:
			exact = append(exact, d)
		case strings.Contains(id, n) || strings.Contains(name, n):
			part = append(part, d)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return part
}
