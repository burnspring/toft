package api

import (
	"encoding/csv"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

const (
	usageDefaultDays = 30
	usageMaxDays     = 90
)

const (
	// quietDefaultDays is what "a while" means when nobody says: a month with
	// nothing at all is long enough to be worth a look and short enough that
	// the first page an administrator opens is not empty.
	quietDefaultDays = 30
	// quietMaxDays is ten years, not the ninety the usage chart allows. That
	// cap exists because a sparkline cannot usefully draw more; this list has
	// no such limit, `app_usage_daily` keeps everything unless the access
	// record's dial says otherwise, and "quiet for three years" is precisely
	// the answer somebody cleaning up an old install wants. Past a retention
	// that is set, the threshold is refused instead (handleQuietApps).
	quietMaxDays = 3650
)

// handleAppUsage returns daily request/viewer counts for an app. Readable by
// anyone signed in who can open the app — it exposes aggregate counts, not
// identities, matching the process metrics already returned by handleGetApp —
// and never by an anonymous caller, even on a public app (BB-28).
func (s *Server) handleAppUsage(w http.ResponseWriter, r *http.Request) {
	if refuseAnonymousAppRead(w, auth.GetPrincipal(r.Context())) {
		return
	}
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	if d := auth.Authorize(p, auth.ActionRead, objectFromApp(app), s.db); !d.Allow {
		writeAuthzError(w, r, d, false)
		return
	}

	days := usageDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > usageMaxDays {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be 1-90", "hint": "pass days as a whole number from 1 to 90, like ?days=30"})
			return
		}
		days = n
	}

	series, err := s.db.GetAppUsageDaily(appID, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	totals, err := s.db.GetAppUsageTotals(appID, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if series == nil {
		series = []database.AppUsageDay{} // JSON [] rather than null
	}

	// Anonymous browsers wherever a sketch was recorded, whatever the switch
	// says now (BB-43): turning counting off stops the counting and the cookie,
	// not the reading of what was counted while it was on — that stays until
	// the access record's retention, and an administrator who switched it off
	// still has to answer for those weeks. A day without a sketch stays nil
	// (the switch was off that day, or nobody came), and the window's figure is
	// one union, never a sum. An install that never counted carries no field.
	perDay, err := s.db.AppAudienceDaily(appID, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	for i := range series {
		if n, ok := perDay[series[i].Day]; ok {
			n := n
			series[i].AnonymousBrowsers = &n
		}
	}
	if totals.AnonymousBrowsers, err = s.db.AppAudienceTotal(appID, days); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"days": days, "series": series, "totals": totals,
		"audience_measurement": s.db.AudienceMeasurement(),
	})
}

// handleActiveUsers — GET /system/active-users. system_admin only: a
// platform-wide headcount is an operator's number, not a viewer's.
//
// "Active" means opened a deployed app, which is the only activity the
// platform counts; someone who signed in and only browsed Toft itself
// does not appear, and nothing here is derived from sessions.
func (s *Server) handleActiveUsers(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "sign in as an administrator to read install-wide usage"})
		return
	}

	counts, err := s.db.CountActiveUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

// handleUsageOverview — GET /system/usage. The estate's traffic, one row per
// app. system_admin only for the same reason as /system/processes: it lists
// every app on the install, including ones the caller holds no grant on.
//
// Counts and no identities, unlike the per-app viewer breakdown — who visited
// stays behind manage_grants on the app itself.
func (s *Server) handleUsageOverview(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}

	days := usageDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > usageMaxDays {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be 1-90", "hint": "pass days as a whole number from 1 to 90, like ?days=30"})
			return
		}
		days = n
	}

	apps, err := s.db.ListAppUsageSummary(days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "apps": apps})
}

// audienceMaxDays bounds /system/audience when the install keeps visits
// forever: thirteen months and change, so a year-on-year comparison fits.
const audienceMaxDays = 400

// handleSystemAudience — GET /system/audience. The install's audience over a
// window, bucketed by day, ISO week or month: distinct signed-in people,
// distinct anonymous browsers (null for a bucket with no sketch — the switch
// was off, or nobody came), and page loads, with the same three
// across the whole window.
//
// system_admin only, for /system/usage's reason: it is computed over every app
// on the install. A route of its own rather than a parameter on that one,
// because a CTO's question is quarterly or yearly and /system/usage's 90-day
// cap is what keeps every per-app read cheap. Bounded instead by the access
// record's retention when one is set — a window past it would report a
// deleted past as an empty one — else by audienceMaxDays.
func (s *Server) handleSystemAudience(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	q := r.URL.Query()
	grain := q.Get("grain")
	if grain == "" {
		grain = database.AudienceGrainWeek
	}
	if grain != database.AudienceGrainDay && grain != database.AudienceGrainWeek && grain != database.AudienceGrainMonth {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grain must be day, week or month",
			"code": "invalid_parameter", "hint": "pass grain=day, grain=week or grain=month"})
		return
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	until, since := today, today.AddDate(0, 0, -(12*7-1)) // twelve weeks by default
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(dayLayout, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "`until` must be a date, such as 2026-01-31",
				"code": "invalid_parameter", "hint": "pass until as a date like 2026-01-31"})
			return
		}
		until = t
	}
	// Clamped, not refused: only `since` had a bound, and one bucket per day
	// up to until=9999-12-31 was millions of them before any SQL (PB-9). A
	// client clock just past UTC midnight sends tomorrow, which is no error.
	if until.After(today) {
		until = today
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(dayLayout, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "`since` must be a date, such as 2026-01-01",
				"code": "invalid_parameter", "hint": "pass since as a date like 2026-01-01"})
			return
		}
		since = t
	}
	if since.After(until) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "`since` may not follow `until`",
			"code": "invalid_parameter", "hint": "pass a since on or before until"})
		return
	}
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	limit, what := audienceMaxDays, " days, the longest window this report covers"
	if settings.AccessRecordRetentionDays != 0 {
		limit, what = settings.AccessRecordRetentionDays, " days this install keeps visits for"
	}
	if since.Before(today.AddDate(0, 0, -(limit - 1))) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "since may not reach further back than the " + strconv.Itoa(limit) + what,
			"code":  "invalid_parameter", "hint": "pass a later since, within the number of days the sentence names",
		})
		return
	}

	buckets, totals, err := s.db.EstateAudience(since, until, grain)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Recorded figures are read whatever the switch says now (BB-43); a
	// bucket with no sketch is null, so the weeks counting was off read as gaps.
	on := settings.AudienceMeasurementEnabled
	writeJSON(w, http.StatusOK, map[string]any{
		"since": since.Format(dayLayout), "until": until.Format(dayLayout), "grain": grain,
		"audience_measurement": on, "buckets": buckets, "totals": totals,
	})
}

// handleQuietApps — GET /system/quiet-apps. The apps nobody has opened in at
// least `days` days, longest-quiet first.
//
// system_admin only, for the same reason as /system/usage: it enumerates every
// app on the install, including ones the caller holds no grant on. Counts and
// owners, never who visited — that stays behind manage_grants on the app.
//
// The threshold is applied here rather than in SQL because the verdict is a
// pure function over three facts (see database.AppActivity.Quiet) and an
// install has hundreds of apps, not millions. Filtering in Go keeps the policy
// in one readable, testable place instead of splitting it across a WHERE
// clause; /system/usage already returns every app unbounded for the same
// reason.
//
// `total` rides along so the page can say "9 of 84 apps" rather than making a
// list of nine look like the whole estate.
func (s *Server) handleQuietApps(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}

	days := quietDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > quietMaxDays {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be 1-3650", "hint": "pass days as a whole number from 1 to 3650, like ?days=90"})
			return
		}
		days = n
	}
	// Past the retained window, quiet and unrecorded cannot be told apart, so
	// a threshold beyond it would list apps on evidence the install deleted.
	if st, err := s.db.GetPlatformSettings(); err == nil &&
		st.AccessRecordRetentionDays != 0 && days > st.AccessRecordRetentionDays {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "days may not exceed the " + strconv.Itoa(st.AccessRecordRetentionDays) +
				" days this install keeps visits for",
			"code": "invalid_parameter", "hint": "pass a smaller days, no larger than the number the sentence names",
		})
		return
	}

	activity, err := s.db.ListAppActivity()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	now := time.Now().UTC()
	quiet := []database.QuietApp{}
	for _, a := range activity {
		if q := a.Quiet(now); q.QuietDays >= days {
			quiet = append(quiet, q)
		}
	}
	sort.Slice(quiet, func(i, j int) bool {
		if quiet[i].QuietDays == quiet[j].QuietDays {
			return quiet[i].Name < quiet[j].Name
		}
		return quiet[i].QuietDays > quiet[j].QuietDays
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"days": days, "total": len(activity), "apps": quiet,
	})
}

// usageViewer is one row of the who-used-this-app breakdown. Kind decides how
// it reads: a person, the anonymous pool, an API key, or a principal that no
// longer exists — the traffic is a fact regardless of who is still around.
type usageViewer struct {
	Kind        string `json:"kind"` // user | anonymous | api_key | removed
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	Requests    int64  `json:"requests"`
	Days        int    `json:"days"`
	FirstDay    string `json:"first_day"`
	LastDay     string `json:"last_day"`
}

// handleAppUsageViewers — GET /apps/{appID}/usage/viewers. Unlike the totals
// above, this names people, so it is gated on manage_grants: whoever decides
// who may reach the app is who may see who did. A reader of the app gets 403,
// a non-reader 404.
func (s *Server) handleAppUsageViewers(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	read := auth.Authorize(p, auth.ActionRead, obj, s.db)
	if d := auth.Authorize(p, auth.ActionManageGrants, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, read.Allow)
		return
	}

	since, until, days, msg := viewerWindow(r.URL.Query())
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": "invalid_parameter", "hint": "pass days from 1 to 90, or since and until as dates like 2026-01-31 — not both"})
		return
	}

	// The file is the stored grain, one line per principal per day: "who
	// opened this app over the year" is a question a per-person total cannot
	// answer. On the Accept header, as the dependency inventory's CSV is.
	if strings.Contains(r.Header.Get("Accept"), "text/csv") {
		rows, err := s.db.GetAppUsageViewerDays(appID, since, until)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeViewerCSV(w, app.Slug, since, until, rows)
		return
	}

	rows, err := s.db.GetAppUsageViewers(appID, since, until)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]usageViewer, 0, len(rows))
	for _, v := range rows {
		out = append(out, viewerOf(v))
	}
	// The oldest day the install still keeps, "" when it keeps everything. A
	// range reaching past it is answered rather than refused — a saved year-long
	// query must not break on the day the dial is set — and the page says the
	// window was clipped.
	retained := ""
	if from, err := s.db.AccessRecordRetainedFrom(); err == nil && !from.IsZero() {
		retained = from.Format(dayLayout)
	}
	resp := map[string]interface{}{
		"since": since.Format(dayLayout), "until": until.Format(dayLayout), "viewers": out,
		"retained_from": retained,
	}
	if days > 0 {
		resp["days"] = days
	}
	writeJSON(w, http.StatusOK, resp)
}

const dayLayout = "2006-01-02"

// viewerWindow is the viewers read's window, in UTC days, inclusive. Either
// `days` (1–90, the page's original form, counting today) or `since` and
// `until` as dates — never both, since one window cannot have two lower
// bounds. `until` defaults to today and `since` may not follow it. The answer
// is the window, the days it was given as (0 for a range), and a refusal.
func viewerWindow(q url.Values) (since, until time.Time, days int, refusal string) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	rawSince, rawUntil, rawDays := q.Get("since"), q.Get("until"), q.Get("days")
	if rawDays != "" && (rawSince != "" || rawUntil != "") {
		return since, until, 0, "`days` cannot be combined with `since` or `until`"
	}
	if rawSince == "" && rawUntil == "" {
		days = usageDefaultDays
		if rawDays != "" {
			n, err := strconv.Atoi(rawDays)
			if err != nil || n < 1 || n > usageMaxDays {
				return since, until, 0, "days must be 1-90"
			}
			days = n
		}
		return today.AddDate(0, 0, -(days - 1)), today, days, ""
	}
	until = today
	if rawUntil != "" {
		t, err := time.Parse(dayLayout, rawUntil)
		if err != nil {
			return since, until, 0, "`until` must be a date, such as 2026-01-31"
		}
		until = t
	}
	if rawSince == "" {
		return since, until, 0, "`since` is required with `until`"
	}
	t, err := time.Parse(dayLayout, rawSince)
	if err != nil {
		return since, until, 0, "`since` must be a date, such as 2026-01-01"
	}
	since = t
	if since.After(until) {
		return since, until, 0, "`since` may not follow `until`"
	}
	return since, until, 0, ""
}

// viewerOf turns one stored principal into the identity the page shows.
func viewerOf(v database.AppUsageViewer) usageViewer {
	e := usageViewer{
		Requests: v.Requests, Days: v.Days,
		FirstDay: v.FirstDay, LastDay: v.LastDay,
	}
	switch {
	case v.UserID == "":
		e.Kind = "anonymous"
		e.DisplayName = "Anonymous visitors"
	case strings.HasPrefix(v.UserID, database.UsageKeyPrefix):
		e.Kind = "api_key"
		e.ID = strings.TrimPrefix(v.UserID, database.UsageKeyPrefix)
		e.DisplayName = v.KeyName
		// A personal key names its person: the proxy records the key, not
		// the owner, so that a nightly cron is traffic and not a daily
		// active human, and this is where the row resolves back.
		if v.KeyOwner != "" {
			e.DisplayName = v.KeyOwner + " (key " + strconv.Quote(v.KeyName) + ")"
		}
		if e.DisplayName == "" {
			e.DisplayName = "Deleted API key"
		}
	case v.Login != "":
		e.Kind = "user"
		e.ID = v.UserID
		e.DisplayName = v.Login
		e.AvatarURL = v.AvatarURL
	default:
		// No FK ties usage to users, so a deleted account leaves its
		// traffic behind. Reporting it as a person who is gone beats
		// dropping requests the totals still count.
		e.Kind = "removed"
		e.DisplayName = "Removed account"
	}
	return e
}

// writeViewerCSV writes the day-grain file. A cell an identity provider
// supplied can begin with a character a spreadsheet reads as a formula, and
// this is the file an auditor opens in one, so such a cell is prefixed with
// an apostrophe.
func writeViewerCSV(w http.ResponseWriter, slug string, since, until time.Time, rows []database.AppUsageViewer) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+slug+"-viewers-"+
		since.Format(dayLayout)+"-to-"+until.Format(dayLayout)+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"day", "kind", "user_id", "display_name", "key_name", "key_owner", "requests"})
	for _, v := range rows {
		e := viewerOf(v)
		_ = cw.Write([]string{
			v.FirstDay, e.Kind, csvCell(e.ID), csvCell(e.DisplayName), csvCell(v.KeyName),
			csvCell(v.KeyOwner), strconv.FormatInt(v.Requests, 10),
		})
	}
	cw.Flush()
}

// csvCell neutralises a value a spreadsheet would evaluate — including one
// led by a tab or carriage return, which Excel strips before reading the rest.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
