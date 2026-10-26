package store

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// maxReviewPage is the portal's largest accepted review pageSize (31+ is
// refused with "Invalid pageSize!", checked live). The inbox reads at most
// inboxPages pages of it per app.
const (
	maxReviewPage = 30
	inboxPages    = 3
	inboxPageSize = maxReviewPage
)

type inboxIn struct {
	Since          string   `json:"since,omitempty" jsonschema:"only reviews on or after this date (YYYY-MM-DD or RFC 3339); undated reviews are kept"`
	UnansweredOnly *bool    `json:"unanswered_only,omitempty" jsonschema:"skip reviews that already have your reply (default true)"`
	Apps           []string `json:"apps,omitempty" jsonschema:"store UUIDs, manifest ids or exact titles; default all apps"`
}

type inboxReview struct {
	App      appOut          `json:"app"`
	ReviewID string          `json:"review_id"`
	Rating   any             `json:"rating,omitempty"`
	Locale   string          `json:"locale,omitempty"`
	Date     string          `json:"date,omitempty"`
	Title    *tool.Untrusted `json:"title,omitempty"`
	Text     tool.Untrusted  `json:"text"`
	Reply    any             `json:"reply"`

	at time.Time
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
	unanswered := in.UnansweredOnly == nil || *in.UnansweredOnly
	c := client(env)
	var apps []portal.App
	if len(in.Apps) == 0 {
		all, err := listApps(ctx, c)
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
	out := []inboxReview{}
	var truncated []string
	for _, a := range apps {
		for page := range inboxPages {
			rv, err := c.Reviews(ctx, a.ID, portal.ReviewsQuery{Page: page, PageSize: inboxPageSize})
			if err != nil {
				return nil, wrap(err)
			}
			list := reviewList(rv)
			for _, e := range list {
				r, ok := normalizeReview(a, e)
				if !ok || (unanswered && r.Reply != nil) || (!since.IsZero() && !r.at.IsZero() && r.at.Before(since)) {
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
	res := map[string]any{"reviews": out, "count": len(out), "apps_checked": len(apps)}
	if len(truncated) > 0 {
		res["truncated_apps"] = truncated
		res["note"] = "only the first reviews of these apps were read; use store_reviews_list with page for the rest"
	}
	return tool.OK(res), nil
}

// reviewList finds the review array in a reviews payload: the payload itself
// or the first array inside an object (the shape was never verified live).
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
// is untrusted; the reply object is wrapped whole.
func normalizeReview(a portal.App, e any) (inboxReview, bool) {
	m, ok := e.(map[string]any)
	if !ok {
		return inboxReview{}, false
	}
	r := inboxReview{
		App:      outApp(a),
		ReviewID: firstStr(m, "id", "reviewId"),
		Rating:   first(m, "rating", "ratingValue", "stars", "score"),
		Locale:   firstStr(m, "locale", "reviewLocale", "language", "languageCode"),
		Text:     tool.Untrusted(firstStr(m, "reviewText", "text", "comment", "body")),
	}
	if t := firstStr(m, "reviewTitle", "title"); t != "" {
		u := tool.Untrusted(t)
		r.Title = &u
	}
	if d := first(m, "reviewDate", "date", "createdDate", "creationDate", "postedDate", "createDate"); d != nil {
		r.at, _ = parseDate(d)
		if r.at.IsZero() {
			r.Date = firstStr(m, "reviewDate", "date", "createdDate", "creationDate", "postedDate", "createDate")
		} else {
			r.Date = r.at.UTC().Format(time.RFC3339)
		}
	}
	if reply := first(m, "reviewReply", "reply", "reviewReplies", "developerReply", "replies"); reply != nil {
		if l, isList := reply.([]any); !isList || len(l) > 0 {
			r.Reply = tool.WrapUntrusted(reply, nil, true)
		}
	}
	return r, r.ReviewID != ""
}

// parseDate reads a date as YYYY-MM-DD, RFC 3339 or epoch (ms when large).
func parseDate(v any) (time.Time, bool) {
	switch x := v.(type) {
	case float64:
		if x > 1e11 {
			return time.UnixMilli(int64(x)), true
		}
		return time.Unix(int64(x), 0), true
	case string:
		x = strings.TrimSpace(x)
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, x); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}
