package tooltest_test

import (
	"testing"
	"time"

	"github.com/idoo/ciq-mcp/internal/mcpserver"
	"github.com/idoo/ciq-mcp/internal/tool"
	"github.com/idoo/ciq-mcp/internal/tool/tooltest"
)

func TestPlannerConformanceFake(t *testing.T) {
	tooltest.PlannerConformance(t, tool.PlanTTL, func(now func() time.Time) tool.Planner {
		return tooltest.NewFakePlanner(now)
	})
}

func TestPlannerConformanceServer(t *testing.T) {
	tooltest.PlannerConformance(t, mcpserver.PlanTTL, func(now func() time.Time) tool.Planner {
		return mcpserver.NewPlanner(now)
	})
}
