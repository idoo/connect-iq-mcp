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
