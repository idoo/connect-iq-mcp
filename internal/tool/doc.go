// Package tool is the contract every garmin-mcp tool is written against: tool
// declaration (Meta, Class, Toolset, ConfirmMode), the registry, the Env of
// dependencies a handler may use (all interfaces, implemented elsewhere),
// result and error helpers, the confirm/plan gates and the HTTP write guard.
//
// Domain packages (internal/tools/<toolset>) import only this package and
// internal/auth (plus tooltest in tests). They never import the MCP SDK or
// jsonschema-go; depguard enforces it.
//
// # Writing a tool
//
// Declare the input as a plain struct. The JSON schema is inferred: json
// names, `jsonschema:"..."` tags become descriptions, fields without
// omitempty are required. Enums go in Meta.Enums. Register in init:
//
//	type listIn struct {
//		AppID string `json:"app_id" jsonschema:"store app id (UUID)"`
//	}
//
//	func init() {
//		tool.Register(tool.New(tool.Meta{
//			Name: "store_versions_list", Toolset: tool.Store, Class: tool.Read, Idempotent: true,
//			Title: "List versions", Description: "Versions of a Connect IQ Store app.",
//		}, versionsList))
//	}
//
//	func versionsList(ctx context.Context, env *tool.Env, in listIn) (*tool.Result, error) {
//		if env.Credentials == nil {
//			return nil, tool.ErrAuthRequired
//		}
//		cookie, err := env.Credentials.PortalCookies(ctx)
//		if err != nil {
//			return nil, fmt.Errorf("%w: %v", tool.ErrAuthRequired, err)
//		}
//		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, portalURL+in.AppID, nil)
//		req.Header.Set("Cookie", cookie)
//		res, err := env.HTTP.Do(req)
//		...
//		return tool.OK(out), nil
//	}
//
// Inside a handler every Env member except Credentials is non-nil; missing
// capabilities fail with ErrUnavailable. Third-party text (store reviews, user
// comments) must be wrapped with [WrapUntrusted]. Idempotent no-ops return
// [Already]; waits that time out return [StillProcessing]; follow-up
// suggestions go in Result.Next or via [WithNext] on errors.
//
// # Safety
//
// Write and Destructive tools are refused when writes are disabled, unless
// Meta.AvailableReadOnly (the tool then restricts itself, see ReadOnlyEnums).
// Env.HTTP is wrapped by [GuardWrites], which refuses every request other
// than GET/HEAD/OPTIONS whose context lacks the write capability ([IsWrite]).
// That capability is minted only inside this package:
//
//   - ConfirmNone Write tools receive it before the handler runs.
//   - ConfirmByFlag tools embed [ConfirmFlag] and call [RequireConfirm].
//     Without confirm:true the call returns a preview and changes nothing;
//     with it the human is asked via elicitation; without a human answer it
//     is accepted only after a preview of the same args ([Planner.Claim]);
//     keys in Meta.ConfirmArgs (e.g. the preview's file digest) are left out
//     of that match and must be checked by the handler.
//   - Meta.RequireHuman makes the elicitation accept
//     mandatory; without elicitation the call is refused unless the profile
//     sets AllowNoElicit. [RequireHumanConfirm] forces the same for one
//     branch; Meta.HumanApproval documents that. Results report
//     _meta.garmin.approved_by.
//   - [Unconfirmed] lets a ConfirmByFlag tool write on a branch it has determined
//     is purely additive.
//
// A gated handler looks like:
//
//	type deleteIn struct {
//		tool.ConfirmFlag
//		ID string `json:"id"`
//	}
//
//	func deleteVersion(ctx context.Context, env *tool.Env, in deleteIn) (*tool.Result, error) {
//		wctx, res := tool.RequireConfirm(ctx, env, in.Confirm, tool.PlanPreview{
//			Summary: "delete version " + in.ID, Affected: []string{in.ID},
//			Irreversible: []string{"the uploaded binary is removed"},
//		})
//		if res != nil {
//			return res, nil
//		}
//		req, _ := http.NewRequestWithContext(wctx, http.MethodDelete, versionURL+in.ID, nil)
//		...
//	}
//
// The approval prompt is rendered by [ApprovalPrompt] from the preview's
// Summary, Intended, Affected and Irreversible fields, never from free text.
//
// # Testing
//
// Package tooltest provides fakes for every Env member and helpers. k.HTTP
// records requests (k.HTTP.Handler serves them; k.HTTP.Mutations lists
// non-GETs); k.UseServer points Env.HTTP at an httptest server instead:
//
//	k := tooltest.New(t)
//	k.HTTP.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ... })
//	res := tooltest.MustOK(t, tooltest.Call(t, myTool, k.Env, `{"app_id":"123"}`))
//	tooltest.AssertGolden(t, "testdata/golden/versions_list.json", res)
package tool
