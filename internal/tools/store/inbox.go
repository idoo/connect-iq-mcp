package store

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// maxReviewPage is the portal's largest accepted review pageSize (31+ is
// refused with "Invalid pageSize!", checked live). The inbox reads at most
// inboxPages pages of it per app, for at most maxApps apps.
const (
	maxReviewPage   = 30
	inboxPages      = 3
	inboxPageSize   = maxReviewPage
	inboxDefaultApp = 30
	inboxMaxApps    = 100
)

type inboxIn struct {
	Since             string   `json:"since,omitempty" jsonschema:"only reviews on or after this date (YYYY-MM-DD in your local time zone, or RFC 3339); undated reviews are kept"`
	UnansweredOnly    *bool    `json:"unanswered_only,omitempty" jsonschema:"skip reviews that already have your reply (default true)"`
	IncludeRatingOnly bool     `json:"include_rating_only,omitempty" jsonschema:"also list star ratings that have no text (default false)"`
	Apps              []string `json:"apps,omitempty" jsonschema:"store UUIDs, manifest ids or exact titles; default all apps"`
	MaxApps           int      `json:"max_apps,omitempty" jsonschema:"read at most this many apps, 1-100 (default 30)"`
}

// inboxReview is one review. The keys come from the live reviews payload
// (testdata/reviews_live.json): id, rating, text, date (epoch ms),
// reviewerFullName, appExternalVersion, replies. The portal sends no locale.
type inboxReview struct {
	App        appOut          `json:"app"`
	ReviewID   string          `json:"review_id"`
	Rating     any             `json:"rating,omitempty"`
	Date       string          `json:"date,omitempty"`
	AppVersion string          `json:"app_version,omitempty"`
	Author     *tool.Untrusted `json:"author,omitempty"`
	Text       tool.Untrusted  `json:"text"`
	Reply      any             `json:"reply"`

	at time.Time
}

// replyNext points the agent at the opt-in toolset that holds store_review_reply.
var replyNext = map[string]any{
	"tool": "toolsets", "arguments": map[string]any{"action": "enable", "toolsets": []string{"store_experimental"}},
	"why": "replying to a review needs store_review_reply, which is in the opt-in store_experimental toolset",
}

func storeReviewsInbox(ctx context.Context, env *tool.Env, in inboxIn) (*tool.Result, error) {
	var since time.Time
	if in.Since != "" {
		t, ok := parseDate(in.Since)
		if !ok {
			return nil, tool.InvalidArgs("since must be YYYY-MM-DD or RFC 3339")
		}
		since = t
	}
	if in.MaxApps == 0 {
		in.MaxApps = inboxDefaultApp
	}
	if in.MaxApps < 1 || in.MaxApps > inboxMaxApps {
		return nil, tool.InvalidArgs("max_apps must be 1-%d", inboxMaxApps)
	}
	if len(in.Apps) > in.MaxApps {
		return nil, tool.InvalidArgs("apps lists %d apps; at most max_apps (%d, up to %d) per call", len(in.Apps), in.MaxApps, inboxMaxApps)
	}
	unanswered := in.UnansweredOnly == nil || *in.UnansweredOnly
	c := client(env)
	var apps []portal.App
	if len(in.Apps) == 0 {
		all, _, err := listApps(ctx, c)
		if err != nil {
			return nil, wrap(err)
		}
		apps = all
	} else {
		for _, ref := range in.Apps {
			a, err := resolve(ctx, c, ref)
			if err != nil {
				return nil, err
			}
			apps = append(apps, a)
		}
	}
	res := map[string]any{}
	if len(apps) > in.MaxApps {
		var skipped []string
		for _, a := range apps[in.MaxApps:] {
			skipped = append(skipped, a.ID)
		}
		res["apps_not_checked"] = skipped
		apps = apps[:in.MaxApps]
	}
	out := []inboxReview{}
	var truncated []string
	var errs []map[string]any
	ratingOnly, checked := 0, 0
apps:
	for i, a := range apps {
		for page := range inboxPages {
			rv, err := c.Reviews(ctx, a.ID, portal.ReviewsQuery{Page: page, PageSize: inboxPageSize})
			if err != nil {
				werr := wrap(err)
				stop := errors.Is(werr, tool.ErrRateLimited) || errors.Is(werr, tool.ErrAuthRequired)
				if stop && i == 0 && page == 0 {
					return nil, werr // nothing collected yet
				}
				errs = append(errs, map[string]any{"app": outApp(a), "error": werr.Error()})
				if stop {
					var rest []string
					for _, s := range apps[i+1:] {
						rest = append(rest, s.ID)
					}
					if len(rest) > 0 {
						res["apps_not_checked"] = append(rest, anyStrings(res["apps_not_checked"])...)
					}
					break apps
				}
				break
			}
			if page == 0 {
				checked++
			}
			list := reviewList(rv)
			for _, e := range list {
				r, ok := normalizeReview(a, e)
				if !ok || (unanswered && r.Reply != nil) || (!since.IsZero() && !r.at.IsZero() && r.at.Before(since)) {
					continue
				}
				if strings.TrimSpace(string(r.Text)) == "" && !in.IncludeRatingOnly {
					ratingOnly++
					continue
				}
				out = append(out, r)
			}
			if len(list) < inboxPageSize {
				break
			}
			if page == inboxPages-1 {
				truncated = append(truncated, a.ID)
			}
		}
	}
	slices.SortStableFunc(out, func(x, y inboxReview) int { return cmp.Compare(y.at.UnixMilli(), x.at.UnixMilli()) })
	res["reviews"], res["count"], res["apps_checked"] = out, len(out), checked
	if ratingOnly > 0 {
		res["rating_only_hidden"] = ratingOnly
	}
	if len(truncated) > 0 {
		res["truncated_apps"] = truncated
		res["note"] = "only the first reviews of these apps were read; use store_reviews_list with page for the rest"
	}
	if len(errs) > 0 || res["apps_not_checked"] != nil {
		res["partial"] = true
	}
	if len(errs) > 0 {
		res["errors"] = errs
	}
	if len(out) > 0 {
		res["next"] = replyNext
	}
	return tool.OK(res), nil
}

func anyStrings(v any) []string { s, _ := v.([]string); return s }

// reviewList finds the review array in a reviews payload: live it is a bare
// array; an object holding the first array is accepted too.
func reviewList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		for _, k := range []string{"reviews", "content", "items", "results", "data"} {
			if l, ok := x[k].([]any); ok {
				return l
			}
		}
		for _, e := range x {
			if l, ok := e.([]any); ok {
				return l
			}
		}
	}
	return nil
}

// normalizeReview maps one raw review. Every third-party string it returns
// is untrusted; the reply list is wrapped whole.
func normalizeReview(a portal.App, e any) (inboxReview, bool) {
	m, ok := e.(map[string]any)
	if !ok {
		return inboxReview{}, false
	}
	r := inboxReview{
		App:        outApp(a),
		ReviewID:   firstStr(m, "id"),
		Rating:     first(m, "rating"),
		AppVersion: firstStr(m, "appExternalVersion"),
		Text:       tool.Untrusted(firstStr(m, "text")),
	}
	if n := firstStr(m, "reviewerFullName"); n != "" {
		u := tool.Untrusted(n)
		r.Author = &u
	}
	if d := first(m, "date"); d != nil {
		r.at, _ = parseDate(d)
		if r.at.IsZero() {
			r.Date = firstStr(m, "date")
		} else {
			r.Date = r.at.In(time.Local).Format(time.RFC3339)
		}
	}
	if reply := first(m, "replies"); reply != nil {
		if l, isList := reply.([]any); !isList || len(l) > 0 {
			r.Reply = tool.WrapUntrusted(reply, nil, true)
		}
	}
	return r, r.ReviewID != ""
}

// parseDate reads a date as YYYY-MM-DD (local midnight), RFC 3339 or epoch
// (ms when large).
func parseDate(v any) (time.Time, bool) {
	switch x := v.(type) {
	case float64:
		if x > 1e11 {
			return time.UnixMilli(int64(x)), true
		}
		return time.Unix(int64(x), 0), true
	case string:
		x = strings.TrimSpace(x)
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t, true
		}
		for _, layout := range []string{"2006-01-02T15:04:05", time.DateOnly} {
			if t, err := time.ParseInLocation(layout, x, time.Local); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}
