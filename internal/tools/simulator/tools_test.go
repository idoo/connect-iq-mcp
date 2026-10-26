package simulator

import (
	"context"
	"errors"
	"testing"

	"github.com/idoo/garmin-mcp/internal/tool"
)

func TestRegistered(t *testing.T) {
	for _, n := range []string{"sim_status", "sim_start", "sim_stop", "sim_run", "sim_logs", "sim_crash_log",
		"sim_test", "sim_screenshot", "sim_press", "sim_set_position"} {
		tl, ok := tool.Lookup(n)
		if !ok {
			t.Fatalf("%s not registered", n)
		}
		if m := tl.Meta(); !m.Local || m.Confirm != tool.ConfirmNone {
			t.Fatalf("%s must be Local without confirm gate", n)
		}
	}
}

func TestFocusToolsRefuseWithoutAllowFocus(t *testing.T) {
	var te *tool.Error
	if _, err := simPress(context.Background(), nil, pressIn{Button: "up"}); !errors.As(err, &te) || te.Code != "focus_required" {
		t.Fatalf("press: %v", err)
	}
	if _, err := simSetPosition(context.Background(), nil, positionIn{Lat: 1}); !errors.As(err, &te) || te.Code != "focus_required" {
		t.Fatalf("position: %v", err)
	}
}
