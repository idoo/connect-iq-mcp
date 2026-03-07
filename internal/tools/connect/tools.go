package connect

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/connectapi"
	"github.com/idoo/connect-iq-mcp/internal/sim"
	"github.com/idoo/connect-iq-mcp/internal/tool"
)

// baseURL is the Connect API host; tests point it at a fake server.
var baseURL = connectapi.BaseURL

const (
	maxWeightDays = 92
	maxListLimit  = 100
	// bigArray is the length above which arrays are dropped unless asked.
	bigArray = 24
)

// userTextKeys are user-written strings in Connect payloads, including the
// names of other users (owners of public activities, profiles).
var userTextKeys = []string{
	"activityName", "description", "displayName", "deviceName", "nickname", "locationName", "notes", "comment", "name",
	"ownerFullName", "ownerDisplayName", "fullName", "userName", "courseName", "userProfileFullName", "userProfileDisplayName",
}

var activityTypeRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func client(env *tool.Env) *connectapi.Client {
	return connectapi.New(env.HTTP, env.Credentials, baseURL)
}

// upstreamHints are the [tool.FromUpstream] hints for Connect API errors;
// messages never carry tokens or bodies. env names the profile in sign-in
// hints.
func upstreamHints(env *tool.Env) tool.UpstreamHints {
	return tool.UpstreamHints{Statuses: map[int]string{
		404: "no data for that date or id, or the endpoint is not available for this account",
		403: "the Connect token lacks access; call auth_login with connect=true (or run `" + tool.LoginCommand(env.Profile.Name, "--connect") + "`)",
	}}
}

// day parses YYYY-MM-DD; empty means today in the local timezone.
func day(env *tool.Env, s, field string) (time.Time, error) {
	if s == "" {
		n := env.Clock.Now().In(time.Local)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, tool.InvalidArgs("%s %q must be YYYY-MM-DD", field, s)
	}
	return t, nil
}

func ymd(t time.Time) string { return t.Format(time.DateOnly) }

// compact drops arrays longer than bigArray (anywhere in v) and lists them
// under "_omitted". keep names arrays to retain.
func compact(v any, keep ...string) any {
	var omitted []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				if a, ok := e.([]any); ok && len(a) > bigArray && !contains(keep, k) {
					delete(x, k)
					omitted = append(omitted, fmt.Sprintf("%s (%d items)", p, len(a)))
					continue
				}
				walk(p, e)
			}
		case []any:
			for _, e := range x {
				walk(prefix, e)
			}
		}
	}
	walk("", v)
	if m, ok := v.(map[string]any); ok && len(omitted) > 0 {
		m["_omitted"] = omitted
	}
	return v
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func untrusted(v any) any { return tool.WrapUntrusted(v, userTextKeys, false) }

// pick copies keys present in m.
func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

type noIn struct{}

type dateIn struct {
	Date string `json:"date,omitempty" jsonschema:"YYYY-MM-DD; default today (local time)"`
}

type seriesIn struct {
	Date   string `json:"date,omitempty" jsonschema:"YYYY-MM-DD; default today (local time)"`
	Series bool   `json:"series,omitempty" jsonschema:"include the full time series"`
}

type rangeIn struct {
	Start string `json:"start,omitempty" jsonschema:"YYYY-MM-DD; default 30 days before end"`
	End   string `json:"end,omitempty" jsonschema:"YYYY-MM-DD; default today"`
}

type listIn struct {
	Offset       int    `json:"offset,omitempty" jsonschema:"number of activities to skip, default 0"`
	Limit        int    `json:"limit,omitempty" jsonschema:"1-100, default 20"`
	StartDate    string `json:"start_date,omitempty" jsonschema:"YYYY-MM-DD"`
	EndDate      string `json:"end_date,omitempty" jsonschema:"YYYY-MM-DD"`
	ActivityType string `json:"activity_type,omitempty" jsonschema:"typeKey, e.g. running, cycling"`
}

type activityIn struct {
	ID      int64 `json:"id" jsonschema:"activity id"`
	Details bool  `json:"details,omitempty" jsonschema:"include sampled metric details"`
	Splits  bool  `json:"splits,omitempty" jsonschema:"include laps/splits"`
}

type downloadIn struct {
	ID        int64  `json:"id" jsonschema:"activity id"`
	Format    string `json:"format,omitempty" jsonschema:"file format, default fit"`
	Path      string `json:"path,omitempty" jsonschema:"output file inside the project root, extension must match format; default garmin/activity-<id>.<format>"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"replace an existing file of the same format"`
}

func init() {
	read := func(name, title, desc string) tool.Meta {
		return tool.Meta{Name: name, Toolset: Toolset, Title: title, Description: desc, Class: tool.Read, Idempotent: true}
	}
	tool.Register(tool.New(read("connect_profile", "Connect profile",
		"Show your Garmin Connect profile: display name, full name and a settings summary (units, height, weight, VO2 max)."), connectProfile))
	tool.Register(tool.New(read("connect_daily_summary", "Daily summary",
		"Daily totals for one day: steps, calories, distance, floors, intensity minutes, resting HR, stress."), connectDailySummary))
	tool.Register(tool.New(read("connect_sleep", "Sleep",
		"Sleep for one night: duration, stages, score. Per-minute series only with series:true."), connectSleep))
	tool.Register(tool.New(read("connect_heart_rate", "Heart rate",
		"Heart rate for one day: resting, min, max, 7-day resting average. Full series only with series:true."), connectHeartRate))
	tool.Register(tool.New(read("connect_hrv", "HRV",
		"Heart rate variability for one night: weekly average, last night average, baseline, status."), connectHRV))
	tool.Register(tool.New(read("connect_stress_body_battery", "Stress and Body Battery",
		"Stress levels and Body Battery for one day. Full series only with series:true."), connectStress))
	tool.Register(tool.New(read("connect_training", "Training",
		"Training readiness, training status and race predictions for one day."), connectTraining))
	tool.Register(tool.New(read("connect_weight", "Weight",
		"Weigh-ins and body composition between two dates (at most 92 days)."), connectWeight))
	tool.Register(tool.New(read("connect_activities_list", "List activities",
		"List your activities newest first: id, name (untrusted), type, start, duration, distance, avg HR, calories."), connectActivitiesList))
	tool.Register(tool.New(read("connect_activity_get", "Get activity",
		"Read one activity's summary; optional laps/splits and sampled details."), connectActivityGet))
	tool.Register(tool.New(read("connect_personal_records", "Personal records",
		"List your personal records (fastest times, longest distances)."), connectPersonalRecords))
	tool.Register(tool.New(read("connect_devices", "Devices",
		"List your registered Garmin devices: model, part number, firmware, nickname (untrusted)."), connectDevices))
	tool.Register(tool.New(tool.Meta{
		Name: "connect_activity_download", Toolset: Toolset, Title: "Download activity",
		Description: "Save an activity file (fit, tcx, gpx, csv, kml) into the project root. Returns the path and size, not the file.",
		Class:       tool.Write,
		Enums:       map[string][]any{"format": {"fit", "tcx", "gpx", "csv", "kml"}},
	}, connectActivityDownload))
}

func connectProfile(ctx context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
	c := client(env)
	p, err := c.Profile(ctx)
	if err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	out := map[string]any{"display_name": tool.Untrusted(p.DisplayName), "full_name": tool.Untrusted(p.FullName)}
	var s map[string]any
	if err := c.GetJSON(ctx, "/userprofile-service/userprofile/user-settings", nil, &s); err != nil {
		out["settings_error"] = tool.FromUpstream(err, upstreamHints(env)).Error()
	} else if ud, ok := s["userData"].(map[string]any); ok {
		out["settings"] = pick(ud, "measurementSystem", "timeFormat", "gender", "birthDate", "height", "weight",
			"vo2MaxRunning", "vo2MaxCycling", "lactateThresholdHeartRate", "lactateThresholdSpeed", "intensityMinutesCalcMethod")
	}
	return tool.OK(out), nil
}

func connectDailySummary(ctx context.Context, env *tool.Env, in dateIn) (*tool.Result, error) {
	d, err := day(env, in.Date, "date")
	if err != nil {
		return nil, err
	}
	c := client(env)
	name, err := c.DisplayName(ctx)
	if err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	var v any
	if err := c.GetJSON(ctx, "/usersummary-service/usersummary/daily/"+name, url.Values{"calendarDate": {ymd(d)}}, &v); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	return tool.OK(map[string]any{"date": ymd(d), "summary": compact(v)}), nil
}

func connectSleep(ctx context.Context, env *tool.Env, in seriesIn) (*tool.Result, error) {
	d, err := day(env, in.Date, "date")
	if err != nil {
		return nil, err
	}
	c := client(env)
	name, err := c.DisplayName(ctx)
	if err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	var v any
	q := url.Values{"date": {ymd(d)}, "nonSleepBufferMinutes": {"60"}}
	if err := c.GetJSON(ctx, "/wellness-service/wellness/dailySleepData/"+name, q, &v); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	if !in.Series {
		v = compact(v)
	}
	return tool.OK(map[string]any{"date": ymd(d), "sleep": v}), nil
}

func connectHeartRate(ctx context.Context, env *tool.Env, in seriesIn) (*tool.Result, error) {
	d, err := day(env, in.Date, "date")
	if err != nil {
		return nil, err
	}
	c := client(env)
	name, err := c.DisplayName(ctx)
	if err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	var m map[string]any
	if err := c.GetJSON(ctx, "/wellness-service/wellness/dailyHeartRate/"+name, url.Values{"date": {ymd(d)}}, &m); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	out := map[string]any{"date": ymd(d)}
	if m != nil {
		out["resting"] = m["restingHeartRate"]
		out["min"] = m["minHeartRate"]
		out["max"] = m["maxHeartRate"]
		out["last_seven_days_avg_resting"] = m["lastSevenDaysAvgRestingHeartRate"]
		if in.Series {
			out["series"] = m["heartRateValues"]
			out["series_columns"] = m["heartRateValueDescriptors"]
		}
	}
	return tool.OK(out), nil
}

func connectHRV(ctx context.Context, env *tool.Env, in seriesIn) (*tool.Result, error) {
	return dated(ctx, env, in.Date, in.Series, "hrv", func(d string) string { return "/hrv-service/hrv/" + d })
}

func connectStress(ctx context.Context, env *tool.Env, in seriesIn) (*tool.Result, error) {
	return dated(ctx, env, in.Date, in.Series, "stress", func(d string) string { return "/wellness-service/wellness/dailyStress/" + d })
}

func dated(ctx context.Context, env *tool.Env, date string, series bool, key string, p func(string) string) (*tool.Result, error) {
	d, err := day(env, date, "date")
	if err != nil {
		return nil, err
	}
	var v any
	if err := client(env).GetJSON(ctx, p(ymd(d)), nil, &v); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	if !series {
		v = compact(v)
	}
	return tool.OK(map[string]any{"date": ymd(d), key: v}), nil
}

func connectTraining(ctx context.Context, env *tool.Env, in dateIn) (*tool.Result, error) {
	d, err := day(env, in.Date, "date")
	if err != nil {
		return nil, err
	}
	c := client(env)
	name, err := c.DisplayName(ctx)
	if err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	out := map[string]any{"date": ymd(d)}
	parts := []struct{ key, path string }{
		{"readiness", "/metrics-service/metrics/trainingreadiness/" + ymd(d)},
		{"status", "/metrics-service/metrics/trainingstatus/aggregated/" + ymd(d)},
		{"race_predictions", "/metrics-service/metrics/racepredictions/latest/" + name}, // DisplayName is already path-escaped
	}
	failed := 0
	var last error
	for _, p := range parts {
		var v any
		if err := c.GetJSON(ctx, p.path, nil, &v); err != nil {
			last = tool.FromUpstream(err, upstreamHints(env))
			if errors.Is(last, tool.ErrAuthRequired) || errors.Is(last, tool.ErrRateLimited) {
				return nil, last
			}
			failed++
			out[p.key+"_error"] = last.Error()
			continue
		}
		out[p.key] = compact(v)
	}
	if failed == len(parts) {
		return nil, last
	}
	return tool.OK(out), nil
}

func connectWeight(ctx context.Context, env *tool.Env, in rangeIn) (*tool.Result, error) {
	end, err := day(env, in.End, "end")
	if err != nil {
		return nil, err
	}
	start := end.AddDate(0, 0, -30)
	if in.Start != "" {
		if start, err = day(env, in.Start, "start"); err != nil {
			return nil, err
		}
	}
	if start.After(end) {
		return nil, tool.InvalidArgs("start %s is after end %s", ymd(start), ymd(end))
	}
	if end.Sub(start) > maxWeightDays*24*time.Hour {
		return nil, tool.InvalidArgs("range is longer than %d days; split it", maxWeightDays)
	}
	var v any
	path := "/weight-service/weight/range/" + ymd(start) + "/" + ymd(end)
	if err := client(env).GetJSON(ctx, path, url.Values{"includeAll": {"true"}}, &v); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	return tool.OK(map[string]any{"start": ymd(start), "end": ymd(end), "weight": v}), nil
}

func connectActivitiesList(ctx context.Context, env *tool.Env, in listIn) (*tool.Result, error) {
	if in.Limit == 0 {
		in.Limit = 20
	}
	if in.Limit < 1 || in.Limit > maxListLimit || in.Offset < 0 {
		return nil, tool.InvalidArgs("limit must be 1-%d and offset >= 0", maxListLimit)
	}
	q := url.Values{"start": {strconv.Itoa(in.Offset)}, "limit": {strconv.Itoa(in.Limit)}}
	for _, f := range []struct{ v, field, param string }{{in.StartDate, "start_date", "startDate"}, {in.EndDate, "end_date", "endDate"}} {
		if f.v == "" {
			continue
		}
		t, err := day(env, f.v, f.field)
		if err != nil {
			return nil, err
		}
		q.Set(f.param, ymd(t))
	}
	if in.ActivityType != "" {
		if !activityTypeRE.MatchString(in.ActivityType) {
			return nil, tool.InvalidArgs("activity_type %q must be a typeKey like running", in.ActivityType)
		}
		q.Set("activityType", in.ActivityType)
	}
	var raw []map[string]any
	if err := client(env).GetJSON(ctx, "/activitylist-service/activities/search/activities", q, &raw); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		r := pick(a, "activityId", "startTimeLocal", "duration", "distance", "averageHR", "calories")
		if n, ok := a["activityName"].(string); ok {
			r["name"] = tool.Untrusted(n)
		}
		if t, ok := a["activityType"].(map[string]any); ok {
			r["type"] = t["typeKey"]
		}
		rows = append(rows, r)
	}
	return tool.OK(map[string]any{"activities": rows, "count": len(rows), "offset": in.Offset}), nil
}

func activityPath(id int64) (string, error) {
	if id <= 0 {
		return "", tool.InvalidArgs("id must be a positive activity id")
	}
	return "/activity-service/activity/" + strconv.FormatInt(id, 10), nil
}

func connectActivityGet(ctx context.Context, env *tool.Env, in activityIn) (*tool.Result, error) {
	p, err := activityPath(in.ID)
	if err != nil {
		return nil, err
	}
	c := client(env)
	var a any
	if err := c.GetJSON(ctx, p, nil, &a); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	out := map[string]any{"activity": untrusted(compact(a))}
	if in.Splits {
		var s any
		if err := c.GetJSON(ctx, p+"/splits", nil, &s); err != nil {
			out["splits_error"] = tool.FromUpstream(err, upstreamHints(env)).Error()
		} else {
			out["splits"] = untrusted(compact(s, "lapDTOs"))
		}
	}
	if in.Details {
		var d any
		q := url.Values{"maxChartSize": {"200"}, "maxPolylineSize": {"0"}}
		if err := c.GetJSON(ctx, p+"/details", q, &d); err != nil {
			out["details_error"] = tool.FromUpstream(err, upstreamHints(env)).Error()
		} else {
			out["details"] = compact(d, "activityDetailMetrics", "metricDescriptors")
		}
	}
	return tool.OK(out), nil
}

func connectPersonalRecords(ctx context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
	c := client(env)
	name, err := c.DisplayName(ctx)
	if err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	var v any
	if err := c.GetJSON(ctx, "/personalrecord-service/personalrecord/prs/"+name, nil, &v); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	return tool.OK(map[string]any{"records": untrusted(v)}), nil
}

func connectDevices(ctx context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
	var raw []map[string]any
	if err := client(env).GetJSON(ctx, "/device-service/deviceregistration/devices", nil, &raw); err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	out := make([]any, 0, len(raw))
	for _, d := range raw {
		out = append(out, untrusted(pick(d, "deviceId", "unitId", "productDisplayName", "displayName", "partNumber",
			"currentFirmwareVersion", "deviceStatus", "primaryActivityTrackerIndicator", "lastSyncTime")))
	}
	return tool.OK(map[string]any{"devices": out, "count": len(out)}), nil
}

func connectActivityDownload(ctx context.Context, env *tool.Env, in downloadIn) (*tool.Result, error) {
	if _, err := activityPath(in.ID); err != nil {
		return nil, err
	}
	if in.Format == "" {
		in.Format = "fit"
	}
	id := strconv.FormatInt(in.ID, 10)
	var src string
	switch in.Format {
	case "fit":
		src = "/download-service/files/activity/" + id
	case "tcx", "gpx", "csv", "kml":
		src = "/download-service/export/" + in.Format + "/activity/" + id
	default:
		return nil, tool.InvalidArgs("format must be fit, tcx, gpx, csv or kml")
	}
	dst := in.Path
	if dst == "" {
		dst = path.Join("garmin", "activity-"+id+"."+in.Format)
	}
	// Resolve before downloading so an escaping path costs no request.
	abs, err := env.Root.Resolve(dst)
	if err != nil {
		return nil, tool.InvalidArgs("path %q: %v", dst, err)
	}
	if err := sim.CheckDest(abs, in.Overwrite, "."+in.Format); err != nil {
		return nil, tool.InvalidArgs("%v", err)
	}
	data, err := client(env).Download(ctx, src)
	if err != nil {
		return nil, tool.FromUpstream(err, upstreamHints(env))
	}
	if in.Format == "fit" {
		if data, err = unzipFIT(data); err != nil {
			return nil, err
		}
	}
	if err := env.Root.WriteFile(abs, data, 0o644); err != nil {
		return nil, err
	}
	return tool.OK(map[string]any{"path": abs, "size": len(data), "format": in.Format}), nil
}

// unzipFIT extracts the .fit file from Garmin's original-file zip; a bare
// FIT (no zip magic) is returned unchanged.
func unzipFIT(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return data, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("activity download is not a valid zip: %w", err)
	}
	for _, f := range zr.File {
		if !strings.EqualFold(path.Ext(f.Name), ".fit") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(io.LimitReader(rc, connectapi.MaxDownload+1))
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		if len(out) > connectapi.MaxDownload {
			return nil, fmt.Errorf("FIT file larger than %d bytes", connectapi.MaxDownload)
		}
		return out, nil
	}
	return nil, errors.New("activity download zip has no .fit file")
}
