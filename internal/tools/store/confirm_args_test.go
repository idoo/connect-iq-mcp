package store

import (
	"testing"

	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

// G1/HA-4: without elicitation (allow_no_elicit) the agent path works:
// preview, then confirm with the preview's iq_sha256. The plan is bound to
// the args without iq_sha256, which publish checks against the file itself.
func TestVersionPublishAgentConfirmWithDigest(t *testing.T) {
	const preview = `{"app":"Bike Guide","path":"app.iq","version":"1.1.0","other_locales":"keep"}`
	t.Run("same file", func(t *testing.T) {
		k, f := setup(t)
		k.Env.Elicit = nil
		k.Env.Profile.AllowNoElicit = true
		writeIQ(t, k, "IQ")
		args, _ := publishPlan(t, k, preview)
		res := tooltest.MustOK(t, tooltest.Call(t, lookup(t, "store_version_publish"), k.Env, args))
		if !f.called("/version") || res.Meta.ApprovedBy != "agent" {
			t.Fatalf("not published by agent confirm: approved_by=%q calls=%v", res.Meta.ApprovedBy, f.calls)
		}
	})
	t.Run("changed file", func(t *testing.T) {
		k, f := setup(t)
		k.Env.Elicit = nil
		k.Env.Profile.AllowNoElicit = true
		writeIQ(t, k, "IQ")
		args, _ := publishPlan(t, k, preview)
		writeIQ(t, k, "IQ2")
		tooltest.MustFail(t, tooltest.Call(t, lookup(t, "store_version_publish"), k.Env, args), "invalid_arguments")
		if f.called("/version") {
			t.Fatalf("changed file published: %v", f.calls)
		}
	})
}
