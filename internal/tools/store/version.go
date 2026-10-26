package store

import (
	"context"
	"fmt"

	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

type validateIn struct {
	tool.ConfirmFlag
	App     string `json:"app,omitempty" jsonschema:"existing app (store UUID, manifest id, or title); omit for a brand-new app"`
	Path    string `json:"path" jsonschema:".iq file inside the project root"`
	Version string `json:"version" jsonschema:"external version, e.g. 1.2.3 (max 20 chars)"`
}

type publishIn struct {
	tool.ConfirmFlag
	App      string            `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Path     string            `json:"path" jsonschema:".iq file inside the project root"`
	Version  string            `json:"version" jsonschema:"new external version, e.g. 1.2.3 (max 20 chars)"`
	WhatsNew map[string]string `json:"whats_new,omitempty" jsonschema:"what's new per existing listing locale, e.g. {\"en\":\"…\"}"`
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
	tool.Register(tool.New(tool.Meta{
		Name: "store_version_validate", Toolset: Toolset, Class: tool.Write, Confirm: tool.ConfirmByFlag,
		Title: "Validate .iq (dry run)",
		Description: "Upload a .iq to the portal validator only: manifest id, signature check, devices. Nothing is published. " +
			"Without confirm:true previews.",
	}, storeVersionValidate))
	tool.Register(tool.New(tool.Meta{
		Name: "store_version_publish", Toolset: Toolset, Class: tool.Destructive, Confirm: tool.ConfirmByFlag, RequireHuman: true,
		Title: "Publish new app version",
		Description: "Upload a .iq as a new public version of an existing app, optionally setting what's new per locale. " +
			"Refuses a failed signature check. Without confirm:true previews; a human approves.",
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

func storeVersionValidate(ctx context.Context, env *tool.Env, in validateIn) (*tool.Result, error) {
	if err := checkVersion(in.Version); err != nil {
		return nil, err
	}
	iq, err := readFile(env, in.Path, maxIQ, ".iq")
	if err != nil {
		return nil, err
	}
	c := client(env)
	var app portal.App
	var manifest string
	if in.App != "" {
		if app, err = resolve(ctx, c, in.App); err != nil {
			return nil, err
		}
		md, err := c.AppMetadata(ctx, app.ID)
		if err != nil {
			return nil, wrap(err)
		}
		manifest = portal.ManifestID(md)
	}
	target := "a brand-new app"
	if app.ID != "" {
		target = fmt.Sprintf("%s (%s)", app.Name, app.ID)
	}
	wctx, gate := tool.RequireConfirm(ctx, env, in.Confirm, tool.PlanPreview{
		Summary:  fmt.Sprintf("validate %s (%d bytes) as version %s of %s; temporary upload, nothing is published", iq.Name, len(iq.Data), in.Version, target),
		Affected: []string{"POST /api/ciq-developerservices/iqFiles/validate"},
	})
	if gate != nil {
		return gate, nil
	}
	v, err := c.ValidateIQ(wctx, app.ID, in.Version, iq)
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(validationReport(v, manifest)), nil
}

func storeVersionPublish(ctx context.Context, env *tool.Env, in publishIn) (*tool.Result, error) {
	if err := checkVersion(in.Version); err != nil {
		return nil, err
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
	have := map[string]bool{}
	for _, l := range locales(md) {
		have[localeOf(l)] = true
	}
	for loc := range in.WhatsNew {
		if !have[loc] {
			return nil, tool.InvalidArgs("locale %q is not in the listing; add it with store_listing_update (title and description required)", loc)
		}
	}
	info, _ := md["appIqFileInfoResult"].(map[string]any)
	manifest := portal.ManifestID(md)
	wctx, gate := tool.RequireConfirm(ctx, env, in.Confirm, tool.PlanPreview{
		Summary:  fmt.Sprintf("publish %s as version %s of %s (current %v); it goes to store review/live", iq.Name, in.Version, app.Name, info["newAppVersion"]),
		Current:  map[string]any{"version": info["newAppVersion"]},
		Intended: map[string]any{"version": in.Version, "file": iq.Name, "bytes": len(iq.Data), "whats_new": in.WhatsNew},
		Affected: []string{
			"POST /api/ciq-developerservices/iqFiles/validate",
			"POST /api/ciq-developerservices/developers/{developerId}/apps/" + app.ID + "/version",
		},
		Irreversible: []string{"a published version cannot be deleted; the version string can never be reused"},
	})
	if gate != nil {
		return gate, nil
	}
	v, err := c.ValidateIQ(wctx, app.ID, in.Version, iq)
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
	status := "published"
	if err := c.PostVersion(wctx, app.ID, v.TempFileID, in.Version); err != nil {
		if !portal.HasCode(err, "APP_VERSION_ALREADY_EXISTS") {
			return nil, wrap(err)
		}
		status = "already_exists"
	}
	res := &tool.Result{}
	out := map[string]any{"app": outApp(app), "version": in.Version, "status": status, "validation": report}
	if len(in.WhatsNew) > 0 {
		fresh, err := c.AppMetadata(wctx, app.ID)
		if err != nil {
			return nil, wrap(err)
		}
		for _, l := range locales(fresh) {
			if txt, ok := in.WhatsNew[localeOf(l)]; ok {
				l["appWhatsNew"] = txt
			}
		}
		fresh["externalAppVersion"] = in.Version
		if status == "published" {
			fresh["tempFileId"] = v.TempFileID
		}
		if err := c.UpdateAppMetadata(wctx, app.ID, fresh); err != nil {
			res.Warn("version posted but what's new failed: " + wrap(err).Error() + "; retry with store_listing_update")
		} else {
			out["whats_new_updated"] = len(in.WhatsNew)
		}
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
