package store

import "github.com/idoo/garmin-mcp/internal/tool"

// Toolset is the toolset every verified tool in this package registers under.
const Toolset = tool.Store

// ExperimentalToolset holds the tools whose portal endpoints were never
// called live (seen only in the portal's JavaScript). Off by default.
const ExperimentalToolset = tool.StoreExperimental
