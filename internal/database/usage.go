package database

import "time"

// UsageBump is one aggregated (app, viewer, day) counter increment, produced
// by internal/usage's flush.
type UsageBump struct {
	AppID  string
	UserID string // "" = anonymous
	// KeyOwnerID is the account a key: row's key belonged to; "" otherwise.
	KeyOwnerID string
	Day        time.Time
	Requests   int64
	// PageLoads is how many of Requests were top-level document navigations.
	PageLoads int64
}

// CapabilityBump is one aggregated (app, capability, day) counter increment.
// Same flush, same batching, and the same best-effort contract: a dropped batch
// costs a governance figure, never a request.
type CapabilityBump struct {
	AppID      string
	Capability string
	Day        time.Time
	Requests   int64
}

// BumpCapabilityUsage upserts a batch of capability counters.
//
// Deliberately counts calls per app and not per viewer. "Which apps are reading
// viewer identity" is the question an administrator has; "which people did app X
// look up" is a surveillance log, and this is the design that answers the first
// without building the second.
func (db *DB) BumpCapabilityUsage(rows []CapabilityBump) error {
	if len(rows) == 0 {
		return nil
	}

	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, r := range rows {
		_, err := tx.Exec(`
			INSERT INTO app_capability_usage_daily (app_id, capability, day, requests)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (app_id, capability, day)
			DO UPDATE SET requests = app_capability_usage_daily.requests + EXCLUDED.requests`,
			r.AppID, r.Capability, r.Day, r.Requests)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CapabilityUsage totals one capability's calls for one app over the last n
// days. The read side of the counter above, for an administrator asking what an
// app is actually doing with what it was granted.
func (db *DB) CapabilityUsage(appID, capability string, days int) (int64, error) {
	var total int64
	err := db.conn.QueryRow(`
		SELECT COALESCE(SUM(requests), 0) FROM app_capability_usage_daily
		WHERE app_id = ? AND capability = ? AND day >= (now() AT TIME ZONE 'UTC')::date - ?::int`,
		appID, capability, days).Scan(&total)
	return total, err
}

type AppUsageDay struct {
	Day      string `json:"day"` // YYYY-MM-DD
	Requests int64  `json:"requests"`
	// Viewers is distinct signed-in humans that day: anonymous ('') and API
	// keys ('key:…') both count toward Requests but never toward Viewers.
	Viewers int `json:"viewers"`
	// PageLoads is top-level document navigations, signed-in and anonymous
	// alike: the unit a reader produces once per page.
	PageLoads int64 `json:"page_loads"`
	// AnonymousBrowsers is the day's distinct anonymous browsers, estimated
	// from its sketch; nil when audience measurement was off (no sketch), so a
	// chart draws a gap rather than a zero. Omitted from the response when the
	// switch is off.
	AnonymousBrowsers *int64 `json:"anonymous_browsers,omitempty"`
}

type AppUsageTotals struct {
	Requests  int64 `json:"requests"`
	Viewers   int   `json:"viewers"` // distinct signed-in principals across the window
	PageLoads int64 `json:"page_loads"`
	// AnonymousBrowsers is the union of the window's sketches — one browser
	// on seven days is one, not seven. Same nil/omitted rule as the day's.
	AnonymousBrowsers *int64 `json:"anonymous_browsers,omitempty"`
}

// BumpAppUsage upserts a batch of usage counters. Batches are small (one row
// per app+viewer per flush interval), so a transaction per batch is enough.
func (db *DB) BumpAppUsage(rows []UsageBump) error {
	if len(rows) == 0 {
		return nil
	}

	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, r := range rows {
		_, err := tx.Exec(`
			INSERT INTO app_usage_daily (app_id, day, user_id, requests, key_owner_user_id, page_loads)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (app_id, day, user_id)
			DO UPDATE SET requests = app_usage_daily.requests + EXCLUDED.requests,
			    page_loads = app_usage_daily.page_loads + EXCLUDED.page_loads,
			    key_owner_user_id = CASE WHEN EXCLUDED.key_owner_user_id <> ''
			        THEN EXCLUDED.key_owner_user_id ELSE app_usage_daily.key_owner_user_id END`,
			r.AppID, r.Day, r.UserID, r.Requests, r.KeyOwnerID, r.PageLoads)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetAppUsageDaily returns one row per day with traffic, oldest first, for the
// last `days` days (today inclusive). Days without traffic are absent.
func (db *DB) GetAppUsageDaily(appID string, days int) ([]AppUsageDay, error) {
	rows, err := db.Conn().Query(`
		SELECT to_char(day, 'YYYY-MM-DD'),
		       COALESCE(SUM(requests), 0),
		       COUNT(DISTINCT user_id) FILTER (WHERE user_id <> '' AND user_id NOT LIKE 'key:%'),
		       COALESCE(SUM(page_loads), 0)
		FROM app_usage_daily
		WHERE app_id = ? AND day > (now() AT TIME ZONE 'UTC')::date - ?::int
		GROUP BY day
		ORDER BY day`,
		appID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AppUsageDay
	for rows.Next() {
		var d AppUsageDay
		if err := rows.Scan(&d.Day, &d.Requests, &d.Viewers, &d.PageLoads); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AppUsageViewer is one principal's traffic against an app over the window.
// UserID carries the raw stored value: "" for anonymous, "key:{id}" for an API
// key, a user id otherwise — the caller turns it into a display identity.
type AppUsageViewer struct {
	UserID   string
	Requests int64
	Days     int    // distinct days with traffic
	FirstDay string // YYYY-MM-DD
	LastDay  string
	// Display identity, resolved here rather than in a per-row lookup. All
	// three are empty when the principal is gone: usage rows have no FK to
	// users precisely so stats survive deletion.
	Login     string
	AvatarURL string
	KeyName   string
	// KeyOwner is the login of the person a personal key belonged to, resolved
	// from the traffic's own key_owner_user_id and from user.delete when the
	// account is gone. Empty only for a removed app key's old rows.
	KeyOwner string
}

// GetAppUsageViewers breaks an app's traffic down by who made it, busiest
// first, over the UTC days since..until inclusive. Anonymous and API-key rows
// come back alongside the users: they are traffic that happened, and omitting
// them would make the numbers disagree with the totals on the same page.
func (db *DB) GetAppUsageViewers(appID string, since, until time.Time) ([]AppUsageViewer, error) {
	return db.appUsageViewers(appID, since, until, false)
}

// GetAppUsageViewerDays is the same principals at the stored grain, one row per
// principal per day, oldest first: the file an auditor asks for when the
// question is who opened an app over a year, which a per-principal total
// cannot answer. FirstDay and LastDay are both the row's day.
func (db *DB) GetAppUsageViewerDays(appID string, since, until time.Time) ([]AppUsageViewer, error) {
	return db.appUsageViewers(appID, since, until, true)
}

// appUsageViewers resolves each principal's name from what still exists, and
// from the record that outlived it when it does not: a key's owner from the
// key_owner_user_id the traffic was written with, a deleted person's login
// from their user.delete row, a deleted key's name from its
// user.api_key_create row — the move ListActivity makes for a deleted app.
func (db *DB) appUsageViewers(appID string, since, until time.Time, byDay bool) ([]AppUsageViewer, error) {
	group, order := "user_id", "requests DESC, v.user_id"
	if byDay {
		group, order = "user_id, day", "v.first_day, requests DESC, v.user_id"
	}
	rows, err := db.Conn().Query(`
		WITH v AS (
			SELECT user_id,
			       COALESCE(SUM(requests), 0) AS requests,
			       COUNT(*) AS active_days,
			       to_char(MIN(day), 'YYYY-MM-DD') AS first_day,
			       to_char(MAX(day), 'YYYY-MM-DD') AS last_day,
			       MAX(key_owner_user_id) AS owner
			FROM app_usage_daily
			WHERE app_id = ? AND day >= ?::date AND day <= ?::date
			GROUP BY `+group+`
		)
		SELECT v.user_id, v.requests, v.active_days, v.first_day, v.last_day,
		       COALESCE(usr.login, ''), COALESCE(usr.avatar_url, ''),
		       COALESCE(pk.name, kc.name, ''),
		       COALESCE(pku.login, ou.login, od.login, '')
		FROM v
		LEFT JOIN users usr ON usr.id = v.user_id
		LEFT JOIN user_api_keys pk ON 'key:' || pk.id = v.user_id
		LEFT JOIN users pku ON pku.id = pk.user_id
		LEFT JOIN users ou ON ou.id = v.owner AND v.owner <> ''
		LEFT JOIN LATERAL (
			SELECT c.after->>'name' AS name FROM audit_log c
			WHERE pk.id IS NULL AND v.user_id LIKE 'key:%' AND c.action = 'user.api_key_create'
			  AND c.after->>'key_id' = substr(v.user_id, 5)
			LIMIT 1) kc ON true
		LEFT JOIN LATERAL (
			SELECT d.before->>'login' AS login FROM audit_log d
			WHERE v.owner <> '' AND pku.id IS NULL AND ou.id IS NULL
			  AND d.action = 'user.delete' AND d.target_type = 'user' AND d.target_id = v.owner
			LIMIT 1) od ON true
		ORDER BY `+order,
		appID, since.Format("2006-01-02"), until.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AppUsageViewer{}
	for rows.Next() {
		var v AppUsageViewer
		if err := rows.Scan(&v.UserID, &v.Requests, &v.Days, &v.FirstDay, &v.LastDay,
			&v.Login, &v.AvatarURL, &v.KeyName, &v.KeyOwner); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ActiveUserCounts is how many distinct signed-in people opened any app within
// each window. Anonymous traffic and API keys are excluded — they are not
// people, and counting them would inflate a number an operator reads as
// headcount. Traffic is recorded per UTC day, so "today" is a day, not 24h.
type ActiveUserCounts struct {
	Today int `json:"today"`
	Week  int `json:"week"`
	Month int `json:"month"`
	Year  int `json:"year"`
}

// CountActiveUsers answers all four windows in one pass over the last year.
func (db *DB) CountActiveUsers() (*ActiveUserCounts, error) {
	var c ActiveUserCounts
	err := db.Conn().QueryRow(`
		SELECT
			COUNT(DISTINCT user_id) FILTER (WHERE day = (now() AT TIME ZONE 'UTC')::date),
			COUNT(DISTINCT user_id) FILTER (WHERE day > (now() AT TIME ZONE 'UTC')::date - 7),
			COUNT(DISTINCT user_id) FILTER (WHERE day > (now() AT TIME ZONE 'UTC')::date - 30),
			COUNT(DISTINCT user_id)
		FROM app_usage_daily
		WHERE day > (now() AT TIME ZONE 'UTC')::date - 365
		  AND user_id <> '' AND user_id NOT LIKE 'key:%'
	`).Scan(&c.Today, &c.Week, &c.Month, &c.Year)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// AppUsageSummary is one app's traffic over the window, for the estate-wide
// usage report. Every app appears, including one nobody has opened: "no visits
// in thirty days" is the answer that page most often exists to give, and an
// app missing from the table reads as an app that does not exist.
type AppUsageSummary struct {
	AppID      string `json:"app_id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Framework  string `json:"framework,omitempty"`
	Status     string `json:"status"`
	OwnerID    string `json:"owner_id,omitempty"`
	OwnerLogin string `json:"owner_login,omitempty"`
	// OwnerGroupID and OwnerGroup are the owning group, empty when a person
	// owns the app — the id so the page can draw `AppOwner` like every other
	// owner column. OwnerGroup is the owning group's name, empty when a person owns the app.
	// Carried beside the login rather than folded into it, because a group is
	// not a person with an unusual name and the Owner column has to be able to
	// say so.
	OwnerGroupID string `json:"owner_group_id,omitempty"`
	OwnerGroup   string `json:"owner_group,omitempty"`
	Requests     int64  `json:"requests"`
	PageLoads    int64  `json:"page_loads"`
	// Viewers is distinct signed-in people across the whole window, so it is
	// not the sum of the daily counts.
	Viewers int `json:"viewers"`
	// LastDay is the most recent day with any traffic, "" for an app with
	// none. A date rather than a timestamp: usage is counted per day, and
	// rendering a day as an hour would invent precision the counter never had.
	LastDay string        `json:"last_day,omitempty"`
	Series  []AppUsageDay `json:"series"`
}

// ListAppUsageSummary returns every app with its traffic over the last `days`
// days, busiest first, each with its daily series for a sparkline.
//
// Two queries rather than one: the totals need a DISTINCT across the whole
// window and the series needs a row per day, and computing both in one grouped
// statement would multiply the request counts by the number of days.
func (db *DB) ListAppUsageSummary(days int) ([]AppUsageSummary, error) {
	rows, err := db.Conn().Query(`
		SELECT a.id, a.name, a.slug, a.framework, a.status,
		       COALESCE(a.owner_user_id, ''), COALESCE(u.login, ''), COALESCE(a.owner_group_id, ''), COALESCE(g.name, ''),
		       COALESCE(SUM(x.requests), 0) AS requests,
		       COALESCE(SUM(x.page_loads), 0),
		       COUNT(DISTINCT x.user_id) FILTER (WHERE x.user_id <> '' AND x.user_id NOT LIKE 'key:%'),
		       COALESCE(to_char(MAX(x.day), 'YYYY-MM-DD'), '')
		FROM apps a
			LEFT JOIN users u ON u.id = a.owner_user_id
			LEFT JOIN groups g ON g.id = a.owner_group_id
			LEFT JOIN app_usage_daily x ON x.app_id = a.id AND x.day > (now() AT TIME ZONE 'UTC')::date - ?::int
		GROUP BY a.id, a.name, a.slug, a.framework, a.status, a.owner_user_id, u.login, a.owner_group_id, g.name
		ORDER BY requests DESC, a.name`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AppUsageSummary{}
	index := map[string]int{}
	for rows.Next() {
		var s AppUsageSummary
		if err := rows.Scan(&s.AppID, &s.Name, &s.Slug, &s.Framework, &s.Status,
			&s.OwnerID, &s.OwnerLogin, &s.OwnerGroupID, &s.OwnerGroup, &s.Requests, &s.PageLoads, &s.Viewers, &s.LastDay); err != nil {
			return nil, err
		}
		s.Series = []AppUsageDay{}
		index[s.AppID] = len(out)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	series, err := db.Conn().Query(`
		SELECT app_id, to_char(day, 'YYYY-MM-DD'),
		       COALESCE(SUM(requests), 0),
		       COUNT(DISTINCT user_id) FILTER (WHERE user_id <> '' AND user_id NOT LIKE 'key:%'),
		       COALESCE(SUM(page_loads), 0)
		FROM app_usage_daily
		WHERE day > (now() AT TIME ZONE 'UTC')::date - ?::int
		GROUP BY app_id, day
		ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer series.Close()

	for series.Next() {
		var appID string
		var d AppUsageDay
		if err := series.Scan(&appID, &d.Day, &d.Requests, &d.Viewers, &d.PageLoads); err != nil {
			return nil, err
		}
		// Usage rows have no FK to apps, so traffic can outlive the app it was
		// against. Such rows belong to no summary and are dropped here.
		if i, ok := index[appID]; ok {
			out[i].Series = append(out[i].Series, d)
		}
	}
	return out, series.Err()
}

// GetAppUsageTotals returns window totals. Viewers is distinct across the
// whole window, so it is not the sum of the daily viewer counts.
func (db *DB) GetAppUsageTotals(appID string, days int) (*AppUsageTotals, error) {
	var t AppUsageTotals
	err := db.Conn().QueryRow(`
		SELECT COALESCE(SUM(requests), 0),
		       COUNT(DISTINCT user_id) FILTER (WHERE user_id <> '' AND user_id NOT LIKE 'key:%'),
		       COALESCE(SUM(page_loads), 0)
		FROM app_usage_daily
		WHERE app_id = ? AND day > (now() AT TIME ZONE 'UTC')::date - ?::int`,
		appID, days).Scan(&t.Requests, &t.Viewers, &t.PageLoads)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UserAppUsage is one app one person opened, over a window: the transpose of
// AppUsageViewer, read from the same counters.
type UserAppUsage struct {
	AppID    string
	Slug     string
	Name     string
	Requests int64
	Days     int    // distinct days with traffic
	FirstDay string // YYYY-MM-DD
	LastDay  string
}

// GetUserAppUsage is what one person has been opening: every app with traffic
// under their own id between since and until (UTC days, inclusive), the most
// recently opened first. **Only apps that still exist** — the join to apps is
// inner, because the question is "what is this person interested in", and a
// deleted app is not an answer anybody can act on; its cost stays in the
// counters for the reads that ask about money. API-key traffic is not counted:
// a script is not a person's interest, and its rows carry the key, not them.
func (db *DB) GetUserAppUsage(userID string, since, until time.Time) ([]UserAppUsage, error) {
	rows, err := db.Conn().Query(`
		SELECT u.app_id, a.slug, a.name,
		       COALESCE(SUM(u.requests), 0),
		       COUNT(*),
		       to_char(MIN(u.day), 'YYYY-MM-DD'),
		       to_char(MAX(u.day), 'YYYY-MM-DD')
		FROM app_usage_daily u
		JOIN apps a ON a.id = u.app_id
		WHERE u.user_id = ? AND u.day >= ?::date AND u.day <= ?::date
		GROUP BY u.app_id, a.slug, a.name
		ORDER BY MAX(u.day) DESC, SUM(u.requests) DESC, a.slug`,
		userID, since.Format("2006-01-02"), until.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserAppUsage{}
	for rows.Next() {
		var u UserAppUsage
		if err := rows.Scan(&u.AppID, &u.Slug, &u.Name, &u.Requests, &u.Days, &u.FirstDay, &u.LastDay); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
