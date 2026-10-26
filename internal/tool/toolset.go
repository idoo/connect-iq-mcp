package tool

import "slices"

// Toolset groups tools that are enabled or disabled together.
type Toolset string

// Toolset names. All but Connect are enabled by default.
const (
	Core      Toolset = "core"
	Store     Toolset = "store"
	SDK       Toolset = "sdk"
	Simulator Toolset = "simulator"
	Connect   Toolset = "connect"
)

// AllToolsets lists every toolset in display order.
var AllToolsets = []Toolset{Core, Store, SDK, Simulator, Connect}

// DefaultToolsets lists the toolsets enabled when no selection is given.
var DefaultToolsets = []Toolset{Core, Store, SDK, Simulator}

// Valid reports whether ts is a known toolset.
func (ts Toolset) Valid() bool { return slices.Contains(AllToolsets, ts) }

// IsDefault reports whether ts is enabled by default.
func (ts Toolset) IsDefault() bool { return slices.Contains(DefaultToolsets, ts) }
