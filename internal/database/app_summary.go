package database

import "time"

// AppSummary is what one app's Overview needs that nothing else on that page
// already carries.
//
// **Why it is on the app-detail response rather than four more endpoints.** The
// hub prints one fact per destination and links to the tab that owns it (ADR
// §53 D4), and the rule it inherits from ADR §50's admin hub is that a tile
// makes no request of its own. A page that fetched a figure per tile would be
// four extra round trips on the one page every link in the product lands on,
// on a browser whose network is the slow half. So the facts travel with the
// app.
//
// **Why these five and no others.** Each is an indexed read, which is the
// bar for something the page polls every fifteen seconds:
//
//   - grants are one prefix scan of `object_grants`
//
//   - schedules are one count on `job_definitions (app_id)`
//
//   - the usage window is a prefix scan of `app_usage_daily`, whose primary key
//     starts `(app_id, day)`
//
//   - the last failure is one row from `idx_app_events_app_time`
//
//   - the quiet verdict is the admin list's evidence query narrowed to this
//     app: three scalar subqueries over `app_usage_daily` and `deployments`,
//     each a prefix scan. The most expensive of the five, and shared with that
//     list rather than reinvented, because two definitions of "quiet" would
//     eventually tell an owner and an administrator different numbers.
//
// A destination whose fact would cost more than that gets no tile, which is
// the same rule stated from the other end.
type AppSummary struct {
	// People and groups with a grant on this app, the owner's own row included
	// — the question the Access tile answers is *who can reach this*, and the
	// owner is one of them. It is the same list the Access tab draws in full,
	// counted rather than fetched.
	GrantUsers  int `json:"grant_users"`
	GrantGroups int `json:"grant_groups"`

	// Scheduled tasks defined on this app, and how many are enabled. Both,
	// because "3 schedules, none enabled" and "3 schedules" are different
	// answers and only one of them means anything is running.
	Schedules        int `json:"schedules"`
	SchedulesEnabled int `json:"schedules_enabled"`

	// Requests and distinct signed-in viewers over the last 30 days — the same
	// window the Usage tab opens on, so the tile and the tab cannot disagree.
	Requests30d int `json:"requests_30d"`
	Viewers30d  int `json:"viewers_30d"`

	// When this app last recorded a failure of any kind. Nil means none has
	// ever been recorded, which is not the same as none having happened and is
	// why the page says "nothing recorded" rather than "no failures".
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`

	// How long since anybody wanted this app, and which of the three facts the
	// clock runs from — the same verdict, from the same evidence query, that
	// the administrator's quiet list renders.
	//
	// **Unwindowed, which is the point.** Requests30d above says "no traffic in
	// 30 days" for an app quiet since 2022 and for one deployed last week, and
	// the owner deciding whether to retire it needs those told apart. It is
	// never a verdict: a quarter-end report opened four times a year is quiet
	// by every measure here and is doing exactly its job.
	QuietDays  int       `json:"quiet_days"`
	QuietBasis string    `json:"quiet_basis"`
	QuietSince time.Time `json:"quiet_since"`
}

// GetAppSummary reads the five facts above. An error on any one of them fails
// the lot: a summary with a silently-zeroed field would print "0 schedules" on
// an app with six, and a fact that is only true by omission is not a fact.
func (db *DB) GetAppSummary(appID string) (*AppSummary, error) {
	var s AppSummary

	if err := db.Conn().QueryRow(`
		SELECT COUNT(*) FILTER (WHERE principal_type = 'user'),
		       COUNT(*) FILTER (WHERE principal_type = 'group')
		FROM object_grants WHERE object_id = ?`, appID,
	).Scan(&s.GrantUsers, &s.GrantGroups); err != nil {
		return nil, err
	}

	if err := db.Conn().QueryRow(`
		SELECT COUNT(*), COUNT(*) FILTER (WHERE enabled)
		FROM job_definitions
		WHERE app_id = ? AND schedule IS NOT NULL AND schedule <> ''`, appID,
	).Scan(&s.Schedules, &s.SchedulesEnabled); err != nil {
		return nil, err
	}

	// Distinct across the whole window rather than the sum of the daily
	// figures, and excluding the anonymous bucket and the key: prefixes, for
	// the reason GetAppUsageTotals gives.
	if err := db.Conn().QueryRow(`
		SELECT COALESCE(SUM(requests), 0),
		       COUNT(DISTINCT user_id) FILTER (WHERE user_id <> '' AND user_id NOT LIKE 'key:%')
		FROM app_usage_daily
		WHERE app_id = ? AND day > CURRENT_DATE - 30`, appID,
	).Scan(&s.Requests30d, &s.Viewers30d); err != nil {
		return nil, err
	}

	var last *time.Time
	if err := db.Conn().QueryRow(`
		SELECT MAX(occurred_at) FROM app_events WHERE app_id = ?`, appID,
	).Scan(&last); err != nil {
		return nil, err
	}
	s.LastFailureAt = last

	// The fifth fact, and the one that is not a single read: three scalar
	// subqueries, each a prefix scan of an index already leading with app_id.
	// It is the admin quiet list's own query narrowed to one app, deliberately,
	// because the alternative is a second definition of "quiet" that can
	// disagree with the first.
	activity, err := db.AppActivityFor(appID)
	if err != nil {
		return nil, err
	}
	if activity != nil {
		q := activity.Quiet(time.Now().UTC())
		s.QuietDays, s.QuietBasis, s.QuietSince = q.QuietDays, q.QuietBasis, q.QuietSince
	}

	return &s, nil
}
