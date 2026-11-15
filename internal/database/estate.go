package database

import (
	"strings"
	"time"
)

// lastDeployedAtExpr is *when this app last shipped*, as a scalar the select
// list can carry with no change to a query's FROM clause or its argument list.
//
// The estate view already computed this fact with a LATERAL join, and it was
// the only place in the product that had it. Everywhere else, "Last deployed"
// was `apps.updated_at` — a column 26 of the 34 `UPDATE apps SET` statements
// in this package touch, including the idle reaper, the health watcher, the
// reconciler, app lock and the git poller's SetLastSeen. On an install with
// idle sleep on, every app's "last deployed" was rewritten twice per doze, so
// sorting the apps list by it during an incident returned what had *broken*
// most recently.
//
// **The join is to the serving release** — servingDeploymentSQL, the one
// definition ServingDeploymentID and EstateByLanguage read too: an app whose
// last build failed is still running the one before it, and an app rolled back
// is running the release it was rolled back to, not the newest success (drills
// D4, 2026-10-28). The date is that release's own build, since a rollback
// leaves no row of its own to date the switch by.
//
// A correlated subquery, because this has to drop into `appColumns` — which is
// concatenated into a dozen queries with fixed argument orders and no FROM of
// its own to extend. The fragment interpolates its status from the constant
// rather than binding it, for the same reason.
//
// Nothing is stored on `apps`: a column the deploy path has to write can drift
// from the deployments table, and the derived answer cannot be wrong.
const lastDeployedAtExpr = `(SELECT COALESCE(d.finished_at, d.started_at)
	 FROM deployments d
	 WHERE d.id = ` + servingDeploymentSQL + `)`

// The estate census: what this install's apps are *written in*, counted.
//
// The framework count on /tags/framework is a different question with a
// different reader — "show me the Flask apps I can see" — and it stays where it
// is. This is the governance reader's number: every app the scope admits,
// grouped by what its *serving* release actually resolved, so an end-of-life
// decision has something to be made from.

// EstateGroup is one bucket of the census, at the grain the database can count
// at: exactly what was recorded, with nothing derived.
//
// Language and Version are the serving deployment's columns verbatim, empty
// included — three different causes produce an empty version and merging them
// into a fourth meaning is the one thing this must not do. Turning that into
// something a person reads (a language for a static app, a series rather than a
// patch) is `internal/api`'s fold, because it needs `builder.LanguageOf` and
// `toolchain.Series`, and this package cannot import either of them without a
// cycle.
type EstateGroup struct {
	Language    string
	Version     string
	Framework   string
	RunningMode string
	Count       int
	// WithJobs is how many of Count declare at least one job.
	WithJobs int
}

// EstateCensus is the whole answer, rows and coverage together.
//
// The coverage is not a second query. It is summed from the same rows the table
// renders, so "84 apps, 61 with a recorded runtime version" cannot disagree
// with the table underneath it — which a separately-counted total eventually
// would, and would do silently.
type EstateCensus struct {
	Groups []EstateGroup
	// AppsTotal is every app in scope, static ones included: they are part of
	// the estate whether or not they run an interpreter.
	AppsTotal int
	// AppsVersioned is how many of those have a recorded runtime version. A
	// static app is never one of them.
	AppsVersioned int
	// AppsLocked is how many are locked — retired apps among them. They are
	// counted rather than excluded, because a locked app still has a release
	// with a version on it and is still something an upgrade has to deal with;
	// the number rides here so the census can say so once, rather than as a
	// parenthesis on every row.
	AppsLocked int
	// AppsWithJobs is how many declare at least one job, of any kind of app.
	AppsWithJobs int
}

// hasJobsSQL is whether the app in scope declares a job — a row in
// job_definitions, which ReconcileManifestJobs keeps equal to toft.toml.
const hasJobsSQL = `EXISTS (SELECT 1 FROM job_definitions j WHERE j.app_id = apps.id)`

// EstateByLanguage counts the apps a scope admits by what their serving release
// resolved.
//
// **The join is to the serving release, never the newest build.** An app whose
// last build failed is running the version its last *successful* build
// resolved, and reporting the failed build's language would report a version no
// process on this box is executing. The join is servingDeploymentSQL, the
// fragment ServingDeploymentID reads, so the census and the Dependencies tab
// cannot name different releases. It is not "the newest successful deployment":
// a rollback leaves no row of its own, so that is the release rolled away from
// (drills D4, 2026-10-28).
//
// One query for the whole census. A per-language query would be four round
// trips and a fifth for the coverage, and they could disagree.
func (db *DB) EstateByLanguage(scope AppScope) (EstateCensus, error) {
	pred, predArgs := scope.readingPredicate()

	rows, err := db.conn.Query(`
		SELECT COALESCE(serving.runtime_language, ''),
		       COALESCE(serving.runtime_version, ''),
		       apps.framework,
		       apps.running_mode,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE apps.locked_at IS NOT NULL),
		       COUNT(*) FILTER (WHERE `+hasJobsSQL+`)
		FROM apps
		LEFT JOIN deployments serving ON serving.id = `+servingDeploymentSQL+`
		WHERE `+pred+`
		GROUP BY 1, 2, 3, 4`, predArgs...)
	if err != nil {
		return EstateCensus{}, err
	}
	defer rows.Close()

	census := EstateCensus{Groups: []EstateGroup{}}
	for rows.Next() {
		var g EstateGroup
		var locked int
		if err := rows.Scan(&g.Language, &g.Version, &g.Framework, &g.RunningMode, &g.Count, &locked, &g.WithJobs); err != nil {
			return EstateCensus{}, err
		}
		census.Groups = append(census.Groups, g)
		census.AppsTotal += g.Count
		if g.Version != "" {
			census.AppsVersioned += g.Count
		}
		census.AppsLocked += locked
		census.AppsWithJobs += g.WithJobs
	}
	return census, rows.Err()
}

// AppRuntime is what one app's serving release resolved, for the list that
// wants to say it per row rather than in aggregate.
//
// Both fields are the recorded columns verbatim, empty included, for the reason
// EstateGroup's are: the words belong to the layer that can derive them.
type AppRuntime struct {
	Language string
	Version  string
}

// ServingRuntimeForApps is EstateByLanguage's join, per app, for a page that
// already holds the apps.
//
// One query for the whole listing rather than one per app — the same reason
// TagsForApps exists. Apps with no successful deployment are simply absent from
// the map, which is the honest answer and not a zero.
func (db *DB) ServingRuntimeForApps(appIDs []string) (map[string]AppRuntime, error) {
	out := map[string]AppRuntime{}
	if len(appIDs) == 0 {
		return out, nil // an empty IN () is a syntax error
	}

	placeholders := make([]string, len(appIDs))
	args := make([]interface{}, 0, len(appIDs))
	for i, id := range appIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	// servingDeploymentSQL is what makes it the serving one — what the app row
	// serves, which after a rollback is not the newest success.
	rows, err := db.conn.Query(`
		SELECT apps.id, d.runtime_language, d.runtime_version
		FROM apps
		JOIN deployments d ON d.id = `+servingDeploymentSQL+`
		WHERE apps.id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var appID string
		var r AppRuntime
		if err := rows.Scan(&appID, &r.Language, &r.Version); err != nil {
			return nil, err
		}
		out[appID] = r
	}
	return out, rows.Err()
}

// AppsWithJobs is which of these apps declare at least one job, for the apps
// list's `?kind=jobs`. Only apps that do are in the map.
func (db *DB) AppsWithJobs(appIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(appIDs) == 0 {
		return out, nil // an empty IN () is a syntax error
	}
	placeholders := make([]string, len(appIDs))
	args := make([]interface{}, 0, len(appIDs))
	for i, id := range appIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := db.conn.Query(`
		SELECT DISTINCT app_id FROM job_definitions
		WHERE app_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// The estate's scan targets: every app as *something to visit*.
//
// A different reader from the census above, and a different row. There a row
// is a language and a count; here it is one app, with the address a scanner
// would point at and the handful of facts that decide whether pointing at it
// means anything — is it published, is it running, is it locked, when was it
// last deployed. The argument for a second query rather than a column on the
// first is the one /admin/packages already had against /admin/dependencies: a
// row that answers two questions answers neither well.

// EstateTarget is one app, as a scanner needs it.
//
// The URL is deliberately absent: this layer has no idea what origin deployed
// apps are served from, and composing one here would be the second definition
// of an app's address. `internal/api` fills it in through builder.PublicURL.
type EstateTarget struct {
	AppID string `json:"app_id"`
	Slug  string `json:"slug"`
	// Hostname and ReleaseBasePath are what the app's address is built from
	// when it lives on a hostname of its own (builder.PublicURL).
	Hostname        string  `json:"-"`
	ReleaseBasePath *string `json:"-"`
	Name            string  `json:"name"`
	// Visibility is the app's stored column, and AnonymityWithheld is whether
	// this install's own switch has taken anonymous access away from it. The pair is deliberately not
	// collapsed here: the substitution is `AuthObjectOf`'s and there is one
	// copy of it, so the layer that serves this resolves the two into the word
	// a scanner acts on. A row that said `public` about an app the install has
	// withheld would send a scanner to test anonymous access that cannot work.
	Visibility        string `json:"visibility"`
	AnonymityWithheld bool   `json:"-"`
	// Owner is a display name — a person's or a group's — and OwnerKind says
	// which. Never both and never neither: apps_one_owner makes that a
	// database fact.
	//
	// The ids ride beside the name because the two have different readers and
	// neither is derivable from the other here: a CMDB pulling this route wants
	// something a person can read, and the console renders it through the one
	// component that knows how to draw either kind of owner, which takes ids.
	Owner        string `json:"owner,omitempty"`
	OwnerKind    string `json:"owner_kind,omitempty"`
	OwnerID      string `json:"owner_id,omitempty"`
	OwnerGroupID string `json:"owner_group_id,omitempty"`
	// OwnerLogin is a person owner's login — what every other admin page names
	// an owner by, so the console says "admin" here as Usage and Processes do
	// rather than the display name a CMDB wants in Owner.
	OwnerLogin string `json:"owner_login,omitempty"`
	Framework  string `json:"framework"`
	// ExecutionMode is the mode the serving release was actually started
	// under, empty for an app that has never been deployed. The *effective*
	// mode, which is the one that answers "what is this workload", never the
	// desired one.
	ExecutionMode  string `json:"execution_mode,omitempty"`
	Status         string `json:"status"`
	CurrentRelease string `json:"current_release,omitempty"`
	// LastDeployedAt is when the serving release finished building, and nil is
	// what makes an app unaddressable: nothing has ever been published under
	// this slug, so there is nothing at that URL to scan.
	LastDeployedAt *time.Time `json:"last_deployed_at,omitempty"`
	// Locked is the platform lock. Listed rather than omitted — a scanner's
	// job is to skip what it is told to skip, and a reader's job is to be
	// complete.
	Locked bool `json:"locked"`
}

// EstateTargetSet is the whole answer, rows and coverage together, for the
// reason EstateCensus carries its own: a list feels exhaustive, and a
// governance reader that answers less than it knows without saying so is the
// defect § Optional capabilities exists to prevent.
//
// **Nothing is ever skipped**, which is why there is no skipped count. An app
// with no successful deployment is a row with no address and a status, because
// omitting it would hide an app from the one read that is supposed to
// enumerate the estate. What the coverage reports instead is how many of the
// rows have an address at all.
type EstateTargetSet struct {
	Targets []EstateTarget `json:"targets"`
	// AppsTotal is every app the scope admits.
	AppsTotal int `json:"apps_total"`
	// AppsLocked is how many are locked. Counted in AppsTotal, listed in
	// Targets, and said once here.
	AppsLocked int `json:"apps_locked"`
}

// EstateTargets lists every app a scope admits, ordered by slug.
//
// **The deploy date is the serving release's**, exactly as EstateByLanguage's
// language is and for the same reason: an app whose last build failed is still
// running the one before it. It comes from `lastDeployedAtExpr`, which the
// apps list now shares — this was the only place in the product that had the
// honest figure, and the extraction is what let the other three readers stop
// rendering `updated_at` under the label "Last deployed".
//
// One query, with the coverage summed from the rows it returns rather than
// counted separately — two counts that can disagree is the failure the census
// above avoids the same way.
func (db *DB) EstateTargets(scope AppScope) (EstateTargetSet, error) {
	pred, args := scope.readingPredicate()

	rows, err := db.conn.Query(`
		SELECT apps.id, apps.slug, apps.name, apps.visibility, apps.framework,
		       apps.status, COALESCE(apps.current_release,''),
		       COALESCE(apps.running_mode,''),
		       apps.locked_at IS NOT NULL,
		       (`+publicAppsOffExpr+`),
		       COALESCE(apps.owner_user_id,''), COALESCE(apps.owner_group_id,''),
		       COALESCE(NULLIF(users.name,''), NULLIF(users.login,''), users.email, ''),
		       COALESCE(groups.name,''), COALESCE(users.login,''),
		       `+lastDeployedAtExpr+`,
		       COALESCE(apps.hostname,''), apps.release_base_path
		FROM apps
		LEFT JOIN users ON users.id = apps.owner_user_id
		LEFT JOIN groups ON groups.id = apps.owner_group_id
		WHERE `+pred+`
		ORDER BY apps.slug`, args...)
	if err != nil {
		return EstateTargetSet{}, err
	}
	defer rows.Close()

	out := EstateTargetSet{Targets: []EstateTarget{}}
	for rows.Next() {
		var t EstateTarget
		var userName, groupName string
		if err := rows.Scan(&t.AppID, &t.Slug, &t.Name, &t.Visibility, &t.Framework,
			&t.Status, &t.CurrentRelease, &t.ExecutionMode, &t.Locked, &t.AnonymityWithheld,
			&t.OwnerID, &t.OwnerGroupID, &userName, &groupName, &t.OwnerLogin,
			&t.LastDeployedAt, &t.Hostname, &t.ReleaseBasePath); err != nil {
			return EstateTargetSet{}, err
		}
		// Exactly one of the two is set. A name that has gone missing leaves
		// the kind behind rather than both: "a group owns this and the group's
		// name did not come back" is a truer answer than an empty cell.
		switch {
		case t.OwnerGroupID != "":
			t.OwnerKind, t.Owner = "group", groupName
		case t.OwnerID != "":
			t.OwnerKind, t.Owner = "user", userName
		}
		out.Targets = append(out.Targets, t)
		out.AppsTotal++
		if t.Locked {
			out.AppsLocked++
		}
	}
	return out, rows.Err()
}
