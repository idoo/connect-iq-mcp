package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

type publishIn struct {
	tool.ConfirmFlag
	App          string            `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Path         string            `json:"path" jsonschema:".iq file inside the project root"`
	Version      string            `json:"version" jsonschema:"new external version, greater than the live one, e.g. 1.2.3 (max 20 chars)"`
	WhatsNew     map[string]string `json:"whats_new,omitempty" jsonschema:"what's new per existing listing locale, e.g. {\"en\":\"…\"}"`
	OtherLocales string            `json:"other_locales,omitempty" jsonschema:"listing locales not in whats_new: keep (default) leaves last version's what's new, clear empties it"`
	IQSHA256     string            `json:"iq_sha256,omitempty" jsonschema:"required with confirm:true: the iq_sha256 from the preview; a file changed since the preview is refused"`
}

type validationOut struct {
	TempUpload      bool   `json:"temp_upload_only"`
	ManifestID      string `json:"manifest_id"`
	SignatureOK     bool   `json:"signature_check_ok"`
	ManifestMatches *bool  `json:"manifest_matches_app,omitempty"`
	DeviceCount     int    `json:"device_count"`
	Devices         []any  `json:"devices,omitempty"`
}

func init() {
	// Not Meta.RequireHuman: the temp validate upload (nothing public) runs
	// via tool.Unconfirmed so its result is in the preview; the publish
	// itself is forced through tool.RequireHumanConfirm.
	tool.Register(tool.New(tool.Meta{
		Name: "store_version_publish", Toolset: Toolset, Class: tool.Destructive, Confirm: tool.ConfirmByFlag,
		Title: "Publish new app version",
		Description: "Publish a .iq as a new public version, optionally with what's new per locale. The preview validates it " +
			"(temp upload): signature, manifest id, devices added/removed. Confirm with the preview's iq_sha256; a human approves.",
		Enums: map[string][]any{"other_locales": {"keep", "clear"}},
	}, storeVersionPublish))
}

func validationReport(v portal.Validation, appManifest string) validationOut {
	out := validationOut{TempUpload: true, ManifestID: v.ManifestAppID(), SignatureOK: v.SignatureOK(), Devices: v.Devices()}
	out.DeviceCount = len(out.Devices)
	if appManifest != "" {
		m := portal.NormalizeID(appManifest) == portal.NormalizeID(out.ManifestID)
		out.ManifestMatches = &m
	}
	return out
}

// deviceKey names a compatibleDevices entry (a string or an object).
func deviceKey(d any) string {
	if s, ok := d.(string); ok {
		return s
	}
	if m, ok := d.(map[string]any); ok {
		for _, k := range []string{"partNumber", "deviceId", "id", "name"} {
			if s := fmt.Sprint(m[k]); m[k] != nil && s != "" {
				return s
			}
		}
	}
	b, _ := json.Marshal(d)
	return string(b)
}

// deviceDiff returns the devices in next but not in live, and the reverse.
func deviceDiff(live, next []any) (added, removed []string) {
	l, n := map[string]bool{}, map[string]bool{}
	for _, d := range live {
		l[deviceKey(d)] = true
	}
	for _, d := range next {
		n[deviceKey(d)] = true
	}
	for k := range n {
		if !l[k] {
			added = append(added, k)
		}
	}
	for k := range l {
		if !n[k] {
			removed = append(removed, k)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

func storeVersionPublish(ctx context.Context, env *tool.Env, in publishIn) (*tool.Result, error) {
	if err := checkVersion(in.Version); err != nil {
		return nil, err
	}
	switch in.OtherLocales {
	case "":
		in.OtherLocales = "keep"
	case "keep", "clear":
	default:
		return nil, tool.InvalidArgs("other_locales must be keep or clear")
	}
	for loc, txt := range in.WhatsNew {
		if n := utf16Len(txt); n > maxWhatsNew {
			return nil, tool.InvalidArgs("whats_new[%s] is %d UTF-16 units; limit %d", loc, n, maxWhatsNew)
		}
	}
	iq, err := readFile(env, in.Path, maxIQ, ".iq")
	if err != nil {
		return nil, err
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
	info, _ := md["appIqFileInfoResult"].(map[string]any)
	live, _ := md["externalAppVersion"].(string)
	if live == "" {
		live, _ = info["newAppVersion"].(string)
	}
	if err := checkNewerVersion(in.Version, live); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	var stale, cleared []string
	for _, l := range locales(md) {
		loc := localeOf(l)
		have[loc] = true
		if _, ok := in.WhatsNew[loc]; !ok && textOf(l, "appWhatsNew") != "" {
			if in.OtherLocales == "clear" {
				cleared = append(cleared, loc)
			} else {
				stale = append(stale, loc)
			}
		}
	}
	for loc := range in.WhatsNew {
		if !have[loc] {
			return nil, tool.InvalidArgs("locale %q is not in the listing; add it with store_listing_update (title and description required)", loc)
		}
	}
	manifest := portal.ManifestID(md)
	sum := sha256.Sum256(iq.Data)
	digest := hex.EncodeToString(sum[:])
	if in.Confirm && in.IQSHA256 != digest {
		return nil, tool.InvalidArgs("iq_sha256 %q does not match %s (sha256 %s): pass the iq_sha256 from the preview; if the file changed, preview again", in.IQSHA256, iq.Name, digest)
	}

	// Validate first (temporary upload, nothing public) so the human
	// approves the actual check results.
	vctx, gate := tool.Unconfirmed(ctx, env)
	if gate != nil {
		return gate, nil
	}
	v, err := c.ValidateIQ(vctx, app.ID, in.Version, iq)
	if err != nil {
		return nil, wrap(err)
	}
	report := validationReport(v, manifest)
	if !report.SignatureOK {
		return nil, &tool.APIError{Status: 0, Code: "SIGNATURE_CHECK_FAILED", Detail: "the portal's signature check failed for this update; nothing was published", Hint: hintFor("SIGNATURE_CHECK_FAILED")}
	}
	if report.ManifestMatches != nil && !*report.ManifestMatches {
		return nil, tool.InvalidArgs("the .iq manifest id %s does not match the app's %s; nothing was published", report.ManifestID, manifest)
	}
	liveDevices, _ := info["compatibleDevices"].([]any)
	added, removed := deviceDiff(liveDevices, report.Devices)

	validation := map[string]any{
		"signature_check_ok":   report.SignatureOK,
		"manifest_matches_app": report.ManifestMatches == nil || *report.ManifestMatches,
		"device_count":         report.DeviceCount,
		"devices_added":        added,
		"devices_removed":      removed,
	}
	intended := map[string]any{
		"version": in.Version, "file": iq.Name, "bytes": len(iq.Data), "iq_sha256": digest,
		"validation": validation, "whats_new": in.WhatsNew,
	}
	var warnings []string
	for _, loc := range stale {
		warnings = append(warnings, fmt.Sprintf("locale %s keeps last version's what's new (not in whats_new; pass other_locales:\"clear\" to empty it)", loc))
	}
	if len(removed) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d device(s) lose this update: %v", len(removed), removed))
	}
	if len(warnings) > 0 {
		intended["warnings"] = warnings
	}
	if len(cleared) > 0 {
		intended["whats_new_cleared"] = cleared
	}
	wctx, gate := tool.RequireHumanConfirm(ctx, env, in.Confirm, tool.PlanPreview{
		Summary:  fmt.Sprintf("publish %s as version %s of %s (live %s); it goes to store review/live", iq.Name, in.Version, app.Name, live),
		Current:  map[string]any{"version": live, "device_count": len(liveDevices)},
		Intended: intended,
		Affected: []string{
			"POST /api/ciq-developerservices/iqFiles/validate (done for this preview; temporary, nothing public)",
			"POST /api/ciq-developerservices/developers/{developerId}/apps/" + app.ID + "/version",
		},
		Irreversible: []string{"a published version cannot be deleted; the version string can never be reused"},
	})
	if gate != nil {
		return gate, nil
	}
	status := "published"
	if err := c.PostVersion(wctx, app.ID, v.TempFileID, in.Version); err != nil {
		if !portal.HasCode(err, "APP_VERSION_ALREADY_EXISTS") {
			return nil, wrap(err)
		}
		status = "already_exists"
	}
	res := &tool.Result{}
	out := map[string]any{
		"app": outApp(app), "version": in.Version, "status": status, "validation": report,
		"devices_added": added, "devices_removed": removed,
	}
	if len(in.WhatsNew) > 0 || len(cleared) > 0 {
		fresh, err := c.AppMetadata(wctx, app.ID)
		if err != nil {
			return nil, wrap(err)
		}
		for _, l := range locales(fresh) {
			if txt, ok := in.WhatsNew[localeOf(l)]; ok {
				l["appWhatsNew"] = txt
			} else if slices.Contains(cleared, localeOf(l)) {
				l["appWhatsNew"] = ""
			}
		}
		fresh["externalAppVersion"] = in.Version
		if status == "published" {
			fresh["tempFileId"] = v.TempFileID
		}
		if err := c.UpdateAppMetadata(wctx, app.ID, fresh); err != nil {
			res.Warn("version posted but what's new failed: " + wrap(err).Error() + "; retry with store_listing_update")
		} else {
			out["whats_new_updated"] = len(in.WhatsNew) + len(cleared)
		}
	}
	for _, w := range warnings {
		res.Warn(w)
	}
	if after, err := c.AppMetadata(ctx, app.ID); err == nil {
		ai, _ := after["appIqFileInfoResult"].(map[string]any)
		out["portal_version"] = ai["newAppVersion"]
	}
	if status == "already_exists" {
		res.Warn("the portal already has this version; nothing new was posted")
	}
	res.Data = out
	return res, nil
}
