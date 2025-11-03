package tool

import "slices"

// Toolset groups tools that are enabled or disabled together.
type Toolset string

// Toolset names. All but Connect and StoreExperimental are enabled by default.
const (
	Core      Toolset = "core"
	Store     Toolset = "store"
	SDK       Toolset = "sdk"
	Simulator Toolset = "simulator"
	Connect   Toolset = "connect"
	// StoreExperimental holds store tools whose portal endpoints were read
	// from the portal's JavaScript but never called live.
	StoreExperimental Toolset = "store_experimental"
)

// AllToolsets lists every toolset in display order.
var AllToolsets = []Toolset{Core, Store, StoreExperimental, SDK, Simulator, Connect}

// DefaultToolsets lists the toolsets enabled when no selection is given.
var DefaultToolsets = []Toolset{Core, Store, SDK, Simulator}

var toolsetDescriptions = map[Toolset]string{
	Core:              "server status, sign-in and toolset switching",
	Store:             "Connect IQ store: apps, listing, versions, reviews",
	StoreExperimental: "store tools that call Connect IQ portal endpoints never verified live (app create/delete, screenshots, review replies); off by default",
	SDK:               "Connect IQ SDK: build, devices, manifests",
	Simulator:         "Connect IQ simulator",
	Connect:           "Garmin Connect account data",
}

// Valid reports whether ts is a known toolset.
func (ts Toolset) Valid() bool { return slices.Contains(AllToolsets, ts) }

// IsDefault reports whether ts is enabled by default.
func (ts Toolset) IsDefault() bool { return slices.Contains(DefaultToolsets, ts) }

// Description is a one-line summary of ts.
func (ts Toolset) Description() string { return toolsetDescriptions[ts] }
