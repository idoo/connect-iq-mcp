package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"

	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

type localeEdit struct {
	Locale      string  `json:"locale" jsonschema:"listing locale code from store_references languages, e.g. en"`
	Title       *string `json:"title,omitempty" jsonschema:"max 50 UTF-16 units"`
	Description *string `json:"description,omitempty" jsonschema:"max 4000 UTF-16 units"`
	WhatsNew    *string `json:"whats_new,omitempty" jsonschema:"max 4000 UTF-16 units"`
}

type listingIn struct {
	tool.ConfirmFlag
	App     string       `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Locales []localeEdit `json:"locales" jsonschema:"locales to change; omitted fields and other locales stay as they are"`
}

type change struct {
	Locale string `json:"locale"`
	Field  string `json:"field"`
	From   string `json:"from"`
	To     string `json:"to"`
}

type replyIn struct {
	tool.ConfirmFlag
	App      string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Action   string `json:"action" jsonschema:"create or delete"`
	ReviewID string `json:"review_id,omitempty" jsonschema:"review to answer (create); letters, digits, - or _"`
	ReplyID  string `json:"reply_id,omitempty" jsonschema:"your reply to remove (delete); letters, digits, - or _"`
	Text     string `json:"text,omitempty" jsonschema:"public reply text (create)"`
}

type screenshotsIn struct {
	tool.ConfirmFlag
	App          string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Action       string `json:"action" jsonschema:"add, reorder or delete"`
	Path         string `json:"path,omitempty" jsonschema:"image inside the project root (add): png/jpg/gif, max 150 KB"`
	ScreenshotID string `json:"screenshot_id,omitempty" jsonschema:"screenshot id (reorder, delete); letters, digits, - or _"`
	Order        int    `json:"order,omitempty" jsonschema:"position 0-4 sent as orderValue (add, reorder)"`
}

type createIn struct {
	tool.ConfirmFlag
	Path        string       `json:"path" jsonschema:".iq file inside the project root"`
	Version     string       `json:"version" jsonschema:"external version, e.g. 1.0.0"`
	Category    string       `json:"category" jsonschema:"category id from store_references categories"`
	DevEmail    string       `json:"dev_email" jsonschema:"support email shown on the store page"`
	Icon        string       `json:"icon" jsonschema:"500x500 png/jpg icon, max 300 KB"`
	Screenshots []string     `json:"screenshots" jsonschema:"1-5 images, max 150 KB each"`
	Locales     []localeEdit `json:"locales" jsonschema:"listing per locale; title and description required"`
	Countries   []string     `json:"countries,omitempty" jsonschema:"country ids from store_references countries; omit for the portal default"`
	Beta        bool         `json:"beta,omitempty" jsonschema:"private beta app (side-load only)"`
}

func init() {
	human := func(name, title, desc string) tool.Meta {
		return tool.Meta{Name: name, Toolset: Toolset, Class: tool.Destructive, Confirm: tool.ConfirmByFlag, RequireHuman: true, Title: title, Description: desc}
	}
	tool.Register(tool.New(human("store_listing_update", "Update store listing",
		"Change title, description or what's new per locale; other locales and fields are kept. Preview shows a diff; a human approves the public change."), storeListingUpdate))
	m := human("store_review_reply", "Reply to a review",
		"Post a public reply to a user review, or delete one of your replies. Without confirm:true previews; a human approves.")
	m.Enums = map[string][]any{"action": {"create", "delete"}}
	m.Patterns = map[string]string{"review_id": idPattern, "reply_id": idPattern}
	m.Toolset = ExperimentalToolset
	tool.Register(tool.New(m, storeReviewReply))
	m = human("store_screenshots", "Manage screenshots",
		"Add (png/jpg/gif ≤150 KB, at most 5), reorder or delete store screenshots. Without confirm:true previews; a human approves.")
	m.Enums = map[string][]any{"action": {"add", "reorder", "delete"}}
	m.Patterns = map[string]string{"screenshot_id": idPattern}
	m.Toolset = ExperimentalToolset
	tool.Register(tool.New(m, storeScreenshots))
	m = human("store_app_create", "Submit a new app",
		"Submit a brand-new app (.iq, icon, 1-5 screenshots, listing) for store review. Without confirm:true previews; a human approves.")
	m.Toolset = ExperimentalToolset
	tool.Register(tool.New(m, storeAppCreate))
	m = human("store_app_delete", "Delete an app",
		"Permanently delete an app from the Connect IQ store. Without confirm:true previews; a human approves.")
	m.Toolset = ExperimentalToolset
	tool.Register(tool.New(m, storeAppDelete))
}

// idPattern matches the portal ids the store tools splice into URL paths.
const idPattern = `^[A-Za-z0-9_-]{1,128}$`

func checkLocale(e localeEdit, create bool) error {
	if strings.TrimSpace(e.Locale) == "" {
		return tool.InvalidArgs("locale is empty")
	}
	if e.Title == nil && e.Description == nil && e.WhatsNew == nil {
		return tool.InvalidArgs("locale %s: nothing to change", e.Locale)
	}
	for _, f := range []struct {
		name string
		v    *string
		max  int
	}{{"title", e.Title, maxTitle}, {"description", e.Description, maxDescription}, {"whats_new", e.WhatsNew, maxWhatsNew}} {
		if f.v == nil {
			continue
		}
		if n := utf16Len(*f.v); n > f.max {
			return tool.InvalidArgs("locale %s: %s is %d UTF-16 units; limit %d", e.Locale, f.name, n, f.max)
		}
	}
	if create && (e.Title == nil || strings.TrimSpace(*e.Title) == "" || e.Description == nil || strings.TrimSpace(*e.Description) == "") {
		return tool.InvalidArgs("locale %s: a new locale needs title and description", e.Locale)
	}
	return nil
}

// applyEdits changes md's localizedAppModel in place and returns the diff.
func applyEdits(md map[string]any, edits []localeEdit) ([]change, error) {
	raw, _ := md["localizedAppModel"].([]any)
	byLoc := map[string]map[string]any{}
	for _, l := range locales(md) {
		byLoc[localeOf(l)] = l
	}
	var diff []change
	seen := map[string]bool{}
	for _, e := range edits {
		if seen[e.Locale] {
			return nil, tool.InvalidArgs("locale %s listed twice", e.Locale)
		}
		seen[e.Locale] = true
		l, ok := byLoc[e.Locale]
		if err := checkLocale(e, !ok); err != nil {
			return nil, err
		}
		if !ok {
			l = map[string]any{"appLocale": e.Locale, "appTitle": "", "appDescription": "", "appWhatsNew": "", "deleteHeroImage": false}
			raw = append(raw, l)
		}
		for _, f := range []struct {
			key, name string
			v         *string
		}{{"appTitle", "title", e.Title}, {"appDescription", "description", e.Description}, {"appWhatsNew", "whats_new", e.WhatsNew}} {
			if f.v == nil || textOf(l, f.key) == *f.v {
				continue
			}
			diff = append(diff, change{Locale: e.Locale, Field: f.name, From: textOf(l, f.key), To: *f.v})
			l[f.key] = *f.v
		}
	}
	md["localizedAppModel"] = raw
	return diff, nil
}

// fieldView is one changed listing text in an approval prompt: line hunks
// (Diff), or, when the prompt would exceed tool.MaxApprovalPrompt, digests
// of the old and new text. Every changed field always has a view.
type fieldView struct {
	Locale string      `json:"locale"`
	Field  string      `json:"field"`
	Diff   string      `json:"diff,omitempty"`
	Old    *textDigest `json:"old,omitempty"`
	New    *textDigest `json:"new,omitempty"`
	Note   string      `json:"note,omitempty"`
}

type textDigest struct {
	Chars  int    `json:"chars"`
	SHA256 string `json:"sha256_prefix"`
	Head   string `json:"first_200,omitempty"`
	Tail   string `json:"last_200,omitempty"`
}

const digestEdge = 200

func digestOf(s string, edges bool) *textDigest {
	sum := sha256.Sum256([]byte(s))
	r := []rune(s)
	d := &textDigest{Chars: len(r), SHA256: hex.EncodeToString(sum[:6])}
	if edges {
		if len(r) <= 2*digestEdge {
			d.Head = s
		} else {
			d.Head, d.Tail = string(r[:digestEdge]), string(r[len(r)-digestEdge:])
		}
	}
	return d
}

// fitFields returns p with Intended set to extra (if any) plus one view per
// changed field: line hunks first; while the approval prompt is too large,
// the largest remaining hunks are replaced by digests.
func fitFields(toolName string, p tool.PlanPreview, extra map[string]any, diff []change) tool.PlanPreview {
	views := make([]fieldView, len(diff))
	for i, d := range diff {
		views[i] = fieldView{Locale: d.Locale, Field: d.Field, Diff: lineDiff(d.From, d.To)}
	}
	set := func() {
		in := map[string]any{"fields": views}
		maps.Copy(in, extra)
		p.Intended = in
	}
	set()
	for {
		if _, err := tool.ApprovalPrompt(toolName, p); err == nil {
			return p
		}
		big := -1
		var d change
		for i, c := range diff {
			if v := views[i]; v.Diff != "" && (big < 0 || len(v.Diff) > len(views[big].Diff)) {
				big, d = i, c
			}
		}
		if big < 0 {
			return p // all digests; the gate reports the remaining size
		}
		views[big].Diff = ""
		if d.From != "" {
			views[big].Old = digestOf(d.From, false)
		}
		views[big].New = digestOf(d.To, true)
		views[big].Note = fmt.Sprintf("too long to show in full: the complete new %s for locale %s is in this call's arguments", d.Field, d.Locale)
		set()
	}
}

// lineDiff renders the changed lines of from→to ("- " old, "+ " new) with
// one line of context ("  "); "…" separates distant hunks.
func lineDiff(from, to string) string {
	a, b := strings.Split(from, "\n"), strings.Split(to, "\n")
	if from == "" {
		a = nil
	}
	// LCS table over lines.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i, j = i+1, j+1
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind != ' ' {
			for c := max(k-1, 0); c <= min(k+1, len(ops)-1); c++ {
				keep[c] = true
			}
		}
	}
	var sb strings.Builder
	gap := false
	for k, o := range ops {
		if !keep[k] {
			gap = true
			continue
		}
		if gap && sb.Len() > 0 {
			sb.WriteString("…\n")
		}
		gap = false
		sb.WriteByte(o.kind)
		sb.WriteByte(' ')
		sb.WriteString(o.text)
		sb.WriteByte('\n')
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func storeListingUpdate(ctx context.Context, env *tool.Env, in listingIn) (*tool.Result, error) {
	if len(in.Locales) == 0 {
		return nil, tool.InvalidArgs("locales is empty")
	}
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	md, err := c.AppMetadata(ctx, app.ID)
	if err != nil {
		return nil, wrap(err)
	}
	diff, err := applyEdits(md, in.Locales)
	if err != nil {
		return nil, err
	}
	if len(diff) == 0 {
		return tool.Already(map[string]any{"app": outApp(app), "changed": 0}), nil
	}
	summary := fmt.Sprintf("change %d listing field(s) of %s; public on the store page", len(diff), app.Name)
	wctx, gate := tool.RequireConfirm(ctx, env, in.Confirm, fitFields("store_listing_update", tool.PlanPreview{
		Summary:  summary,
		Affected: []string{"PUT /api/ciq-developerservices/developers/{developerId}/apps/" + app.ID},
	}, nil, diff))
	if gate != nil {
		return gate, nil
	}
	if err := c.UpdateAppMetadata(wctx, app.ID, md); err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"app": outApp(app), "changed": len(diff), "changes": diff}), nil
}

func storeReviewReply(ctx context.Context, env *tool.Env, in replyIn) (*tool.Result, error) {
	var p tool.PlanPreview
	switch in.Action {
	case "create":
		if strings.TrimSpace(in.ReviewID) == "" || strings.TrimSpace(in.Text) == "" {
			return nil, tool.InvalidArgs("create needs review_id and text")
		}
		if err := portal.CheckID("review_id", in.ReviewID); err != nil {
			return nil, wrap(err)
		}
	case "delete":
		if strings.TrimSpace(in.ReplyID) == "" {
			return nil, tool.InvalidArgs("delete needs reply_id")
		}
		if err := portal.CheckID("reply_id", in.ReplyID); err != nil {
			return nil, wrap(err)
		}
	default:
		return nil, tool.InvalidArgs("action must be create or delete")
	}
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	if in.Action == "create" {
		p = tool.PlanPreview{
			Summary:      fmt.Sprintf("publish a public reply to review %s of %s", in.ReviewID, app.Name),
			Intended:     map[string]string{"reply": in.Text},
			Affected:     []string{"POST /api/appsLibraryExternalServices/api/asw/apps/" + app.ID + "/reviews/" + in.ReviewID + "/reviewreplies"},
			Irreversible: []string{"the reply is public on the store immediately"},
		}
	} else {
		p = tool.PlanPreview{
			Summary:      fmt.Sprintf("delete your reply %s on %s", in.ReplyID, app.Name),
			Affected:     []string{"DELETE /api/appsLibraryExternalServices/api/asw/apps/" + app.ID + "/reviewreplies/" + in.ReplyID},
			Irreversible: []string{"the deleted reply text cannot be restored"},
		}
	}
	wctx, gate := tool.RequireConfirm(ctx, env, in.Confirm, p)
	if gate != nil {
		return gate, nil
	}
	if in.Action == "delete" {
		if err := c.DeleteReply(wctx, app.ID, in.ReplyID); err != nil {
			return nil, wrap(err)
		}
		return tool.OK(map[string]any{"app": outApp(app), "deleted": in.ReplyID}), nil
	}
	out, err := c.CreateReply(wctx, app.ID, in.ReviewID, in.Text)
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"app": outApp(app), "review_id": in.ReviewID, "reply": tool.WrapUntrusted(out, nil, true)}), nil
}

var imageExts = []string{".png", ".jpg", ".jpeg", ".gif"}

func storeScreenshots(ctx context.Context, env *tool.Env, in screenshotsIn) (*tool.Result, error) {
	if in.Order < 0 || in.Order >= maxScreenshots {
		return nil, tool.InvalidArgs("order must be 0-%d", maxScreenshots-1)
	}
	var f portal.File
	var err error
	switch in.Action {
	case "add":
		if f, err = readFile(env, in.Path, maxScreenshot, imageExts...); err != nil {
			return nil, err
		}
	case "reorder", "delete":
		if strings.TrimSpace(in.ScreenshotID) == "" {
			return nil, tool.InvalidArgs("%s needs screenshot_id", in.Action)
		}
		if err := portal.CheckID("screenshot_id", in.ScreenshotID); err != nil {
			return nil, wrap(err)
		}
	default:
		return nil, tool.InvalidArgs("action must be add, reorder or delete")
	}
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	base := "/api/ciq-developerservices/developers/{developerId}/apps/" + app.ID + "/screenshots"
	p := tool.PlanPreview{Summary: fmt.Sprintf("%s screenshot of %s (public on the store page)", in.Action, app.Name)}
	switch in.Action {
	case "add":
		p.Intended = map[string]any{"file": f.Name, "bytes": len(f.Data), "order": in.Order}
		p.Affected = []string{"POST " + base}
	case "reorder":
		p.Intended = map[string]any{"screenshot_id": in.ScreenshotID, "order": in.Order}
		p.Affected = []string{"PUT " + base + "/" + in.ScreenshotID}
	case "delete":
		p.Affected = []string{"DELETE " + base + "/" + in.ScreenshotID}
		p.Irreversible = []string{"the screenshot is removed from the store; upload it again to restore"}
	}
	wctx, gate := tool.RequireConfirm(ctx, env, in.Confirm, p)
	if gate != nil {
		return gate, nil
	}
	out := map[string]any{"app": outApp(app), "action": in.Action}
	switch in.Action {
	case "add":
		r, err := c.AddScreenshot(wctx, app.ID, in.Order, f)
		if err != nil {
			return nil, wrap(err)
		}
		out["result"] = r
	case "reorder":
		err = c.ReorderScreenshot(wctx, app.ID, in.ScreenshotID, in.Order)
	case "delete":
		err = c.DeleteScreenshot(wctx, app.ID, in.ScreenshotID)
	}
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(out), nil
}

func storeAppCreate(ctx context.Context, env *tool.Env, in createIn) (*tool.Result, error) {
	if err := checkVersion(in.Version); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Category) == "" || !strings.Contains(in.DevEmail, "@") {
		return nil, tool.InvalidArgs("category and a valid dev_email are required")
	}
	if len(in.Locales) == 0 {
		return nil, tool.InvalidArgs("at least one locale is required")
	}
	if len(in.Screenshots) < 1 || len(in.Screenshots) > maxScreenshots {
		return nil, tool.InvalidArgs("screenshots must hold 1-%d images", maxScreenshots)
	}
	model := make([]any, 0, len(in.Locales))
	for _, e := range in.Locales {
		if err := checkLocale(e, true); err != nil {
			return nil, err
		}
		l := map[string]any{"appLocale": e.Locale, "appTitle": *e.Title, "appDescription": *e.Description, "deleteHeroImage": false}
		if e.WhatsNew != nil {
			l["appWhatsNew"] = *e.WhatsNew
		}
		model = append(model, l)
	}
	iq, err := readFile(env, in.Path, maxIQ, ".iq")
	if err != nil {
		return nil, err
	}
	icon, err := readFile(env, in.Icon, maxIcon, ".png", ".jpg", ".jpeg")
	if err != nil {
		return nil, err
	}
	shots := make([]portal.File, len(in.Screenshots))
	names := make([]string, len(in.Screenshots))
	for i, s := range in.Screenshots {
		if shots[i], err = readFile(env, s, maxScreenshot, imageExts...); err != nil {
			return nil, err
		}
		names[i] = shots[i].Name
	}
	md := map[string]any{
		"betaApp": in.Beta, "shadowApp": false, "category": in.Category, "devEmail": in.DevEmail,
		"migrationAllowed": false, "reviewNotificationActive": true,
		"externalAppVersion": in.Version, "paymentModel": 0,
	}
	if len(in.Countries) > 0 {
		md["selectedCountries"] = in.Countries
	}
	// The listing texts go to the approval as per-field views (see fitFields).
	shown := map[string]any{"file": iq.Name, "icon": icon.Name, "screenshots": names, "metadata": maps.Clone(md)}
	md["localizedAppModel"] = model
	var fields []change
	for _, e := range in.Locales {
		fields = append(fields, change{Locale: e.Locale, Field: "title", To: *e.Title}, change{Locale: e.Locale, Field: "description", To: *e.Description})
		if e.WhatsNew != nil {
			fields = append(fields, change{Locale: e.Locale, Field: "whats_new", To: *e.WhatsNew})
		}
	}
	c := client(env)
	wctx, gate := tool.RequireConfirm(ctx, env, in.Confirm, fitFields("store_app_create", tool.PlanPreview{
		Summary:      fmt.Sprintf("submit new app %q version %s for store review", textOf(model[0].(map[string]any), "appTitle"), in.Version),
		Affected:     []string{"POST /api/ciq-developerservices/iqFiles/validate", "POST /api/ciq-developerservices/developers/{developerId}/apps"},
		Irreversible: []string{"the app is submitted for review and goes public when approved"},
	}, shown, fields))
	if gate != nil {
		return gate, nil
	}
	v, err := c.ValidateIQ(wctx, "", in.Version, iq)
	if err != nil {
		return nil, wrap(err)
	}
	md["tempFileId"] = v.TempFileID
	id, err := c.CreateApp(wctx, md, icon, shots)
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"app_id": id, "version": in.Version, "validation": validationReport(v, "")}), nil
}

type deleteIn struct {
	tool.ConfirmFlag
	App string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
}

func storeAppDelete(ctx context.Context, env *tool.Env, in deleteIn) (*tool.Result, error) {
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	wctx, gate := tool.RequireConfirm(ctx, env, in.Confirm, tool.PlanPreview{
		Summary:      fmt.Sprintf("permanently delete %s (%s, status %s) from the Connect IQ store", app.Name, app.ID, app.Status),
		Affected:     []string{"DELETE /api/ciq-developerservices/developers/{developerId}/apps/" + app.ID},
		Irreversible: []string{"the app, its reviews and install base on the store are removed; this cannot be undone"},
	})
	if gate != nil {
		return gate, nil
	}
	if err := c.DeleteApp(wctx, app.ID); err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"deleted": outApp(app)}), nil
}
