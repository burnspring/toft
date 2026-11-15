package database

import (
	"database/sql"
	"time"
)

// Finding the apps nobody uses any more.
//
// The durable record of app traffic is `app_usage_daily`, and two of its
// properties are what make this question answerable at all: it is written off
// the request path by internal/usage's flush, so counting a visit costs the
// visitor nothing; and it is kept on access_record_retention_days, which
// keeps everything by default, so the lookback is bounded only by how long
// the install has existed — or, once an administrator sets the dial, by the
// retained window, which QuietBasisRetention states rather than hides. The in-memory idle tracker cannot answer this — it forgets on every
// restart by design, and its timeout is measured in minutes.
//
// What makes this more than `ORDER BY last_visit` is that two apps with no
// traffic since March are not the same app. One was deployed in February and
// forgotten; the other was deployed last night and has not been announced yet.
// So quiet is measured from the most recent evidence that *anybody* wanted the
// app — a visit, a successful deploy, or failing both the day it was created —
// and QuietBasis names which of the three the clock is running from. An
// administrator about to take somebody's app away should be able to see the
// reasoning, not just the number.
//
// The list is deliberately not a verdict. Low traffic is not the same as no
// value: a quarter-end report opened four times a year is quiet by every
// measure here and doing exactly its job. See docs/invariants.md § Data path.

// How the quiet clock was started for one app. Four values, and they are a
// wire contract with the UI, which explains each one in its own words.
const (
	// QuietBasisVisit — somebody opened it, and that is the last thing anybody
	// did. The only basis that measures actual disuse.
	QuietBasisVisit = "visit"
	// QuietBasisDeploy — never visited since a successful deploy, so the clock
	// runs from the deploy. An app published and never announced looks like
	// this.
	QuietBasisDeploy = "deploy"
	// QuietBasisCreated — no traffic and no successful deploy, ever. The clock
	// runs from creation, and the app has never worked.
	QuietBasisCreated = "created"
	// QuietBasisRetention — no visit is retained and the clock would otherwise
	// start before the oldest day this install keeps, so it starts there: not
	// opened in at least the retained window. Without it an app last opened
	// eighteen months ago on a 365-day install would read as never visited
	// since a deploy two years ago, on the list that decides whose app is
	// taken away.
	QuietBasisRetention = "retention"
)

// AppActivity is the raw evidence for one app, straight out of the database
// with no policy applied.
//
// Kept apart from QuietApp so that the arithmetic turning evidence into a
// verdict is a pure function a test can drive without a database — the same
// reason ResolveRetention is one.
type AppActivity struct {
	AppID      string
	Name       string
	Slug       string
	Status     string
	OwnerID    string
	OwnerLogin string
	OwnerGroup string
	CreatedAt  time.Time
	// Locked is whether an administrator has already locked this app.
	Locked bool
	// LastDay is the most recent UTC day with any traffic at all, zero for an
	// app nobody has ever opened. Unwindowed, which is the whole difference
	// between this and AppUsageSummary.LastDay: that one is capped at the
	// window being charted, so every app quiet for longer than the window
	// reports the same empty answer.
	LastDay time.Time
	// Requests is lifetime traffic rather than a window's. The app this list
	// exists to surface is the one whose forty thousand requests all happened
	// last year, and a windowed count says zero for it and zero for an app that
	// has never been opened.
	Requests int64
	// LastDeploy is the most recent *successful* deploy, zero if none ever
	// succeeded. Successful specifically: a failed deploy is evidence somebody
	// tried, not evidence the app is wanted running.
	LastDeploy time.Time
	// RetainedFrom is the oldest day the access record still keeps, zero when
	// it keeps everything. Set by the reader from the setting.
	RetainedFrom time.Time
}

// QuietApp is one app with the verdict attached. The JSON is what the admin
// page renders.
type QuietApp struct {
	AppID  string `json:"app_id"`
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
	// OwnerID is a person owner's id, which the console's AppOwner links by.
	OwnerID    string `json:"owner_id,omitempty"`
	OwnerLogin string `json:"owner_login,omitempty"`
	OwnerGroup string `json:"owner_group,omitempty"`
	Requests   int64  `json:"requests"`
	// Locked closes the loop: this list is where an administrator decides what
	// to lock, so it has to say what is locked already. Without it the same
	// nine apps read as nine outstanding decisions every month, and the work
	// done last month is invisible.
	Locked bool `json:"locked"`
	// LastDay is a date, "" for an app never opened. A date rather than a
	// timestamp because usage is counted per day and rendering a day as an hour
	// would invent precision the counter never had — the same reason
	// AppUsageSummary.LastDay is one.
	LastDay string `json:"last_day,omitempty"`
	// QuietSince is where the clock starts, and QuietBasis says which fact it
	// came from. Both travel together: the date is meaningless without knowing
	// whether it is a visit or a creation.
	QuietSince time.Time `json:"quiet_since"`
	QuietBasis string    `json:"quiet_basis"`
	QuietDays  int       `json:"quiet_days"`
}

// Quiet reduces one app's evidence to a verdict as of now.
//
// Pure, and the whole of the policy. Days are truncated rather than rounded:
// an app last opened 47 hours ago has been quiet for one day, not two, and a
// list that overstates the number is a list an administrator stops trusting the
// first time they check one by hand.
func (a AppActivity) Quiet(now time.Time) QuietApp {
	q := QuietApp{
		AppID:      a.AppID,
		Name:       a.Name,
		Slug:       a.Slug,
		Status:     a.Status,
		OwnerID:    a.OwnerID,
		OwnerLogin: a.OwnerLogin,
		OwnerGroup: a.OwnerGroup,
		Requests:   a.Requests,
		Locked:     a.Locked,
		QuietSince: a.CreatedAt,
		QuietBasis: QuietBasisCreated,
	}
	if !a.LastDay.IsZero() {
		q.LastDay = a.LastDay.UTC().Format("2006-01-02")
	}

	// Most recent evidence wins, in ascending order of how strongly it says
	// somebody wanted this app. A visit beats a deploy even when the deploy is
	// newer is *not* the rule — recency is, because the question is "how long
	// since anything happened", and a deploy last night is something happening.
	if a.LastDeploy.After(q.QuietSince) {
		q.QuietSince, q.QuietBasis = a.LastDeploy, QuietBasisDeploy
	}
	if !a.LastDay.IsZero() {
		// A usage day is a date, so it lands at midnight UTC. Compared as-is it
		// would lose to a deploy made later the same day, which is correct: the
		// deploy is the more recent event either way, and both round to the
		// same number of quiet days.
		if a.LastDay.After(q.QuietSince) {
			q.QuietSince, q.QuietBasis = a.LastDay, QuietBasisVisit
		}
	}

	// Past the retained window, quiet and unrecorded cannot be told apart, so
	// the clock starts at the window's edge and says so. A deploy or creation
	// inside the window still wins: every visit after it is retained.
	if !a.RetainedFrom.IsZero() && a.LastDay.IsZero() && q.QuietSince.Before(a.RetainedFrom) {
		q.QuietSince, q.QuietBasis = a.RetainedFrom, QuietBasisRetention
	}

	if d := now.UTC().Sub(q.QuietSince.UTC()); d > 0 {
		q.QuietDays = int(d.Hours() / 24)
	}
	return q
}

// ListAppActivity returns the evidence for every app on the install, name
// order.
//
// Scalar subqueries rather than joins on purpose: joining `app_usage_daily`
// and `deployments` in one statement multiplies their rows against each other,
// and while MAX and SUM over the product would still come out right for MAX,
// SUM(requests) would be silently multiplied by the deployment count. Each
// subquery is a prefix scan of a primary key that already leads with app_id.
//
// Every app, unfiltered: the threshold is the caller's, and an install with a
// few hundred apps is a few hundred rows. ListAppUsageSummary is unbounded for
// the same reason.
func (db *DB) ListAppActivity() ([]AppActivity, error) {
	rows, err := db.Conn().Query(appActivitySelect+`
		ORDER BY a.name`, DeployStatusSuccess)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	retained, err := db.AccessRecordRetainedFrom()
	if err != nil {
		return nil, err
	}
	out := []AppActivity{}
	for rows.Next() {
		a, err := scanAppActivity(rows)
		if err != nil {
			return nil, err
		}
		a.RetainedFrom = retained
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccessRecordRetainedFrom is the oldest day the access record still keeps,
// or the zero time when it keeps everything.
func (db *DB) AccessRecordRetainedFrom() (time.Time, error) {
	s, err := db.GetPlatformSettings()
	if err != nil {
		return time.Time{}, err
	}
	if s.AccessRecordRetentionDays == 0 {
		return time.Time{}, nil
	}
	return AccessRecordCutoff(s.AccessRecordRetentionDays, time.Now()), nil
}

// appActivitySelect is the evidence, and both readers use it.
//
// One statement rather than two similar ones because the owner's line on their
// own app and the administrator's quiet list are answering the same question,
// and two queries that drift are two surfaces that disagree about whether an
// app has been quiet for 214 days — in front of the two people most likely to
// compare notes.
const appActivitySelect = `
	SELECT a.id, a.name, a.slug, a.status, a.created_at,
	       a.locked_at IS NOT NULL,
	       COALESCE(a.owner_user_id, ''), COALESCE(u.login, ''), COALESCE(g.name, ''),
	       (SELECT MAX(x.day) FROM app_usage_daily x WHERE x.app_id = a.id),
	       COALESCE((SELECT SUM(x.requests) FROM app_usage_daily x WHERE x.app_id = a.id), 0),
	       (SELECT MAX(COALESCE(d.finished_at, d.started_at)) FROM deployments d
	         WHERE d.app_id = a.id AND d.status = ?)
	FROM apps a
		LEFT JOIN users u ON u.id = a.owner_user_id
		LEFT JOIN groups g ON g.id = a.owner_group_id`

// scanAppActivity reads one row of it.
func scanAppActivity(rows interface{ Scan(...any) error }) (AppActivity, error) {
	var a AppActivity
	var lastDay, lastDeploy sql.NullTime
	if err := rows.Scan(&a.AppID, &a.Name, &a.Slug, &a.Status, &a.CreatedAt, &a.Locked,
		&a.OwnerID, &a.OwnerLogin, &a.OwnerGroup, &lastDay, &a.Requests, &lastDeploy); err != nil {
		return a, err
	}
	if lastDay.Valid {
		a.LastDay = lastDay.Time
	}
	if lastDeploy.Valid {
		a.LastDeploy = lastDeploy.Time
	}
	return a, nil
}

// AppActivityFor is one app's evidence, for that app's own page.
//
// **The fact is not administrator-only; the enumeration is.** `/system/quiet-apps`
// needs system_admin because it walks every app on the install including ones
// the caller holds no grant on — but "nobody has opened this app in 214 days"
// is a fact about one app, the owner can already read its usage, and the owner
// is the person who could actually retire it. Learning it from an
// administrator's email is one step too many.
//
// Returns nil for an app that does not exist, so the caller keeps its own 404.
func (db *DB) AppActivityFor(appID string) (*AppActivity, error) {
	row := db.Conn().QueryRow(appActivitySelect+`
		WHERE a.id = ?`, DeployStatusSuccess, appID)
	a, err := scanAppActivity(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if a.RetainedFrom, err = db.AccessRecordRetainedFrom(); err != nil {
		return nil, err
	}
	return &a, nil
}
