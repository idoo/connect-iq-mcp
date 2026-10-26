// Package all links every domain tool package so their init functions
// register tools with internal/tool.
package all

import (
	_ "github.com/idoo/garmin-mcp/internal/tools/connect"
	_ "github.com/idoo/garmin-mcp/internal/tools/core"
	_ "github.com/idoo/garmin-mcp/internal/tools/sdk"
	_ "github.com/idoo/garmin-mcp/internal/tools/simulator"
	_ "github.com/idoo/garmin-mcp/internal/tools/store"
)
