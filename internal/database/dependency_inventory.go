package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"toft/internal/advisory"
	"toft/internal/deps"
	"toft/internal/textutil"
)

// This install's document about itself: what each release actually installed,
// and the findings derived from it.
//
// **Findings are derived, never stored.** A finding is
// `inventory × advisories × waivers`, computed at read time. A stored
// `vulnerable` boolean would be wrong the moment the feed moved and would need
// its own invalidation logic that nobody would maintain — the same rule the
// gallery follows for "installed", for the same reason.
//
// The inventory is what makes that cheap. The sweep joins stored rows against
// refreshed advisories, so continuous watching costs no rebuild and touches no
// registry; "which apps have GHSA-xxxx" is a query rather than a fleet-wide
// grep; and a rollback needs no scan at all.

// SaveDeploymentDependencies records what a build resolved.
//
// Written once per successful resolution and never updated: the inventory of a
// given lockfile is a fact, and the thing that moves is the corpus it is
// judged against. Replaced wholesale rather than merged, so a re-resolution of
// the same deployment cannot leave a package behind that is no longer there.
func (db *DB) SaveDeploymentDependencies(deploymentID string, list []deps.Dependency) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM deployment_dependencies WHERE deployment_id=?`, deploymentID); err != nil {
		return err
	}
	if _, err := insertDependencies(tx, deploymentID, list, false); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceEcosystemDependencies swaps one ecosystem's rows for list and leaves
// every other ecosystem's alone.
//
// R's inventory is written twice by one deploy: from renv.lock before the
// build, and from the installed library after it. The second is the record,
// and it must not take a Python or npm inventory read at the same Step 2 with
// it.
func (db *DB) ReplaceEcosystemDependencies(deploymentID, ecosystem string, list []deps.Dependency) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM deployment_dependencies WHERE deployment_id=? AND ecosystem=?`,
		deploymentID, ecosystem); err != nil {
		return err
	}
	if _, err := insertDependencies(tx, deploymentID, list, false); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendDeploymentDependencies adds rows the inventory does not hold yet and
// reports how many it added.
//
// The sweep's write, for a package an R app installed while it ran: **append,
// never replace**. A package found at 02:00 and gone after a restart was still
// loaded by the app, and the inventory is the record of what was observed.
// A row already present — the same package at the same version, from any
// source — is left exactly as it was.
func (db *DB) AppendDeploymentDependencies(deploymentID string, list []deps.Dependency) (int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	n, err := insertDependencies(tx, deploymentID, list, true)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// insertDependencies writes list in batches. keepExisting leaves a conflicting
// row untouched; otherwise a conflict merges, because a lockfile can
// legitimately name one package at one version twice — the same version
// hoisted to two places in a tree — and a whole build must not fail over a
// duplicate row. The merge of duplicates *within* list happens here first
// (deps.MergeDuplicates): ON CONFLICT reconciles a new row with a stored one
// and refuses two new rows on the same key in one statement (G-2).
func insertDependencies(tx *Tx, deploymentID string, list []deps.Dependency, keepExisting bool) (int, error) {
	// The floor for every writer — the resolvers, the sweep, the R library:
	// a package or version Postgres would refuse fails the whole statement.
	// Clipped *before* the merge, so two rows the cut makes equal are folded
	// rather than hitting ON CONFLICT twice in one statement (LB-1).
	cleaned := make([]deps.Dependency, len(list))
	for i, d := range list {
		d.Package = textutil.Clip(d.Package, deps.MaxRecordedName)
		d.Version = textutil.Clip(d.Version, deps.MaxRecordedName)
		cleaned[i] = d
	}
	list = deps.MergeDuplicates(cleaned)
	var added int
	for start := 0; start < len(list); start += insertBatch {
		end := start + insertBatch
		if end > len(list) {
			end = len(list)
		}
		chunk := list[start:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO deployment_dependencies
			(deployment_id, ecosystem, package, version, direct, dev, path, source) VALUES `)
		args := make([]any, 0, len(chunk)*8)
		for i, d := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args, deploymentID, d.Ecosystem, d.Package, d.Version,
				d.Direct, d.Dev, clip(d.Path, 1000), deps.SourceOf(d))
		}
		if keepExisting {
			sb.WriteString(` ON CONFLICT (deployment_id, ecosystem, package, version) DO NOTHING`)
		} else {
			sb.WriteString(` ON CONFLICT (deployment_id, ecosystem, package, version) DO UPDATE SET
			direct = deployment_dependencies.direct OR excluded.direct,
			dev = deployment_dependencies.dev AND excluded.dev,
			path = CASE WHEN length(excluded.path) < length(deployment_dependencies.path)
			            THEN excluded.path ELSE deployment_dependencies.path END`)
		}
		res, err := tx.Exec(sb.String(), args...)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err == nil {
			added += int(n)
		}
	}
	return added, nil
}

// CopyDeploymentDependencies carries an inventory forward to a rollback.
//
// A rollback republishes a release already on disk: nothing is fetched and
// nothing is rebuilt, so there is nothing to resolve. Copying the rows rather
// than pointing at the older deployment keeps every deployment row
// self-contained, which is what lets the retention sweep prune one without
// silently emptying another's Dependencies tab.
func (db *DB) CopyDeploymentDependencies(fromDeployID, toDeployID string) error {
	_, err := db.conn.Exec(`
		INSERT INTO deployment_dependencies
			(deployment_id, ecosystem, package, version, direct, dev, path, source)
		SELECT ?, ecosystem, package, version, direct, dev, path, source
		FROM deployment_dependencies WHERE deployment_id=?
		ON CONFLICT (deployment_id, ecosystem, package, version) DO NOTHING`,
		toDeployID, fromDeployID)
	return err
}

// ListDeploymentDependencies is one release's inventory, in display order.
func (db *DB) ListDeploymentDependencies(deploymentID string) ([]deps.Dependency, error) {
	rows, err := db.conn.Query(`
		SELECT ecosystem, package, version, direct, dev, path, source
		FROM deployment_dependencies WHERE deployment_id=?
		ORDER BY ecosystem, package, version`, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []deps.Dependency{}
	for rows.Next() {
		var d deps.Dependency
		if err := rows.Scan(&d.Ecosystem, &d.Package, &d.Version, &d.Direct, &d.Dev, &d.Path, &d.Source); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountDeploymentDependencies is the inventory size without the rows, for a
// list that shows a number beside every release.
func (db *DB) CountDeploymentDependencies(deploymentID string) (int, error) {
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM deployment_dependencies WHERE deployment_id=?`, deploymentID).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// SetDeploymentVerdict stores what a release was allowed to deploy under.
//
// **Decided once, at deploy.** A restart is not a deploy: an isolated pod that
// is evicted or rescheduled bootstraps again with no new code, and evaluating
// today's corpus at that moment would refuse an app that has been serving
// since March, at 3am, because a third party published an advisory.
func (db *DB) SetDeploymentVerdict(deploymentID string, v deps.Verdict) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = db.conn.Exec(`
		UPDATE deployments SET dependency_verdict=?, dependency_checked_at=?, dependency_feed_as_of=?
		WHERE id=?`, string(encoded), v.CheckedAt, v.FeedAsOf, deploymentID)
	return err
}

// GetDeploymentVerdict reads it back. ok is false for a release that predates
// the feature or was never scanned — which is *unknown*, a state with a name,
// and never the same thing as "no findings".
func (db *DB) GetDeploymentVerdict(deploymentID string) (deps.Verdict, bool, error) {
	var raw string
	err := db.conn.QueryRow(
		`SELECT dependency_verdict FROM deployments WHERE id=?`, deploymentID).Scan(&raw)
	if err == sql.ErrNoRows {
		return deps.Verdict{}, false, nil
	}
	if err != nil {
		return deps.Verdict{}, false, err
	}
	if strings.TrimSpace(raw) == "" {
		return deps.Verdict{}, false, nil
	}
	var v deps.Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		// A verdict this binary cannot read is the same situation as no
		// verdict: unknown, and never a pass.
		return deps.Verdict{}, false, nil
	}
	return v, true, nil
}

// servingDeploymentSQL is the deployment the outer query's `apps` row is
// serving, as a scalar subquery — the one definition every reader of "the
// serving release" uses, ServingDeploymentID included.
//
// **What the app serves, not the newest success.** A rollback or promote does
// not leave a successful row of its own: the builder deletes the transient row
// and the source row is current again, so after rolling 47 back to 46 the newest
// success is still 47 — the release that was rolled *away* from — and every
// reader named it serving. The answer is read off the app row instead, in the
// order builder.LiveReleaseID reads it:
//
//  1. `apps.release_ref`, whose last element is the owning deployment
//     (`artifact:releases/<app>/<deploy>.tar.gz`, or a local release directory
//     named for it);
//  2. `apps.current_release`, the workload name `<slug>-<first 8 of the id>`
//     (ReleaseProcessName), for a row whose reference names no deployment;
//  3. the newest successful row, for an app with neither — and for a legacy
//     rollback row, which owns no release and was the one serving when written.
//
// Only successful rows qualify, so a failed or in-flight build is never
// "serving" whatever the reference says.
const servingDeploymentSQL = `(
	SELECT sd.id FROM deployments sd
	WHERE sd.app_id = apps.id AND sd.status = '` + DeployStatusSuccess + `'
	ORDER BY
		(sd.id = regexp_replace(regexp_replace(regexp_replace(apps.release_ref,
			'^(local|artifact):', ''), '^.*/', ''), '\.tar\.gz$', '')) DESC,
		(apps.current_release <> '' AND
			apps.current_release = apps.slug || '-' || left(replace(sd.id, '-', ''), 8)) DESC,
		COALESCE(sd.finished_at, sd.started_at) DESC NULLS LAST, sd.id DESC
	LIMIT 1)`

// ServingDeploymentID is the release an app is actually running: the
// deployment servingDeploymentSQL names, or "" when it has none. Every reader
// of "the serving release" — the Dependencies tab, the recheck route, the
// audit file's live flag and SBOM digest, the job-start verdict — goes through
// this or the fragment, so they cannot disagree with each other or with the
// deployments list after a rollback.
func (db *DB) ServingDeploymentID(appID string) (string, error) {
	var id sql.NullString
	err := db.conn.QueryRow(`SELECT `+servingDeploymentSQL+` FROM apps WHERE apps.id = ?`, appID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id.String, err
}

// LastRefusedDeployment is the most recent deploy this policy turned away,
// when it is newer than what is serving.
//
// The tab needs both: after a blocked deploy the app is still on release 46
// while 47 was refused, and those are two finding sets with two different
// meanings. Without this the developer reads today's refusal off yesterday's
// inventory and fixes the wrong package.
func (db *DB) LastRefusedDeployment(appID string) (*Deployment, error) {
	serving, err := db.ServingDeploymentID(appID)
	if err != nil {
		return nil, err
	}
	rows, err := db.conn.Query(`
		SELECT id, app_id, commit_sha, branch, status, started_at, finished_at,
		       dependency_verdict
		FROM deployments
		WHERE app_id=? AND status=? AND dependency_verdict <> ''
		ORDER BY COALESCE(finished_at, started_at) DESC NULLS LAST, id DESC
		LIMIT 5`, appID, DeployStatusFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var d Deployment
		var verdict string
		if err := rows.Scan(&d.ID, &d.AppID, &d.CommitSHA, &d.Branch, &d.Status,
			&d.StartedAt, &d.FinishedAt, &verdict); err != nil {
			return nil, err
		}
		if d.ID == serving {
			continue
		}
		var v deps.Verdict
		if err := json.Unmarshal([]byte(verdict), &v); err != nil || v.Allowed {
			// A failed deploy that this policy did not refuse is somebody
			// else's failure and does not belong on this tab.
			continue
		}
		// Newer than what is serving, as the doc comment always said: after
		// a refused 47 and a fixed, successful 48 the tab still read "your
		// last deploy was refused" (code review 2026-11-11, G-7).
		if serving != "" {
			if live, err := db.GetDeployment(serving); err == nil && live != nil &&
				!deploymentTime(&d).After(deploymentTime(live)) {
				return nil, nil
			}
		}
		return &d, nil
	}
	return nil, rows.Err()
}

// deploymentTime is when a deployment ended, or began when it has not ended.
func deploymentTime(d *Deployment) time.Time {
	if d.FinishedAt != nil {
		return *d.FinishedAt
	}
	if d.StartedAt != nil {
		return *d.StartedAt
	}
	return time.Time{}
}

// FindingsFor derives one release's findings.
//
// The join narrows to advisories that could possibly apply — same ecosystem,
// same package — and the **version comparison happens in Go**, over the
// handful of rows that returns. That is deliberate: npm semver, PEP 440 and Go
// semver each order versions differently, and a SQL approximation of any of
// them would be a second opinion about the one question this feature exists to
// answer correctly.
func (db *DB) FindingsFor(deploymentID, appID string, policy deps.Policy) ([]advisory.Finding, error) {
	findings, _, err := db.FindingsForCounted(deploymentID, appID, policy)
	return findings, err
}

// FindingsForCounted is FindingsFor and, beside it, how many advisory ranges
// could not be compared with the installed version at all — a bound or a
// version outside the ecosystem's grammar (advisory.Incomparable). Those are
// never findings, and they are never silently nothing either: the deploy gate
// logs the count and records it on the verdict (security review 2026-11-14,
// DG-10).
func (db *DB) FindingsForCounted(deploymentID, appID string, policy deps.Policy) ([]advisory.Finding, int, error) {
	rows, err := db.conn.Query(`
		SELECT d.ecosystem, d.package, d.version, d.direct, d.dev, d.path, d.source,
		       a.id, a.kind, a.severity, a.summary, a.url,
		       af.introduced, af.fixed, af.last_affected, af.versions
		FROM deployment_dependencies d
		JOIN advisory_affected af ON af.ecosystem = d.ecosystem AND af.package = d.package
		JOIN advisories a ON a.id = af.advisory_id
		WHERE d.deployment_id = ? AND a.withdrawn_at IS NULL`, deploymentID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	// One finding per (advisory, package, version): an advisory can declare
	// several ranges covering the same version, and reporting it twice would
	// double every count on every surface.
	type key struct{ advisory, pkg, version string }
	found := map[key]advisory.Finding{}
	incomparable := 0

	for rows.Next() {
		var f advisory.Finding
		var r advisory.Range
		var versions string
		if err := rows.Scan(&f.Ecosystem, &f.Package, &f.Version, &f.Direct, &f.Dev, &f.Path, &f.Source,
			&f.AdvisoryID, &f.Kind, &f.Severity, &f.Summary, &f.URL,
			&r.Introduced, &r.Fixed, &r.LastAffected, &versions); err != nil {
			return nil, 0, err
		}
		if versions != "" {
			r.Versions = strings.Split(versions, ",")
		}
		switch advisory.Matches(f.Ecosystem, f.Version, r) {
		case advisory.Matched:
		case advisory.Incomparable:
			incomparable++
			continue
		default:
			continue
		}
		k := key{f.AdvisoryID, f.Package, f.Version}
		f.FixedIn = r.Fixed
		if prev, ok := found[k]; ok {
			// Two ranges both matched. Keep the one that names a fix, because
			// "there is a version to move to" is the actionable half and an
			// unfixed range must not hide it.
			if prev.FixedIn != "" {
				continue
			}
		}
		found[k] = f
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	waivers, err := db.liveWaivers(appID)
	if err != nil {
		return nil, 0, err
	}

	out := make([]advisory.Finding, 0, len(found))
	for _, f := range found {
		if w, ok := waivers.match(f.AdvisoryID, f.Package); ok {
			// A waiver silences the refusal, never the record. The finding
			// stays, marked, by whom, and until when — a control whose
			// exceptions disappear from its own reporting is one that reports
			// what it wants to hear.
			f.Waived, f.WaivedBy, f.WaivedFor = true, w.CreatedByName, w.Reason
			end := w.ExpiresAt
			f.WaiverEnd = &end
		}
		f.WouldBlock = !f.Waived && policy.Blocks(f)
		out = append(out, f)
	}
	advisory.SortFindings(out)
	return out, incomparable, nil
}

// AppsAffectedBy answers "which of our apps has this", by advisory id or by
// package name.
//
// The package name is the one that actually gets used, because the news
// arrives as "there's something going round in event-stream" hours before
// anybody has an identifier. It reads stored inventory and reaches no registry
// and no feed, so it works with the watch off, with the corpus stale, and
// while the policy is `warn` and enforcing nothing.
func (db *DB) AppsAffectedBy(query string, policy deps.Policy) ([]AffectedApp, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return []AffectedApp{}, nil
	}
	return db.estateFindings(policy, q)
}

// EstateFindings is every app's findings in **one query**.
//
// It is what makes both axes of the estate view answerable from one read: by
// app is "how bad is this app", by advisory is "how bad is this news", and they
// are the same rows grouped differently. Deriving per app instead would be one
// query per app on a page that opens with forty of them — and, worse, would let
// the two axes disagree.
func (db *DB) EstateFindings(policy deps.Policy) ([]AffectedApp, error) {
	return db.estateFindings(policy, "")
}

// estateFindings is the shared core. An empty filter means the whole estate.
func (db *DB) estateFindings(policy deps.Policy, filter string) ([]AffectedApp, error) {
	where := ""
	args := []any{DeployStatusSuccess}
	if filter != "" {
		where = ` AND (LOWER(a.id) = LOWER(?) OR LOWER(d.package) LIKE ? ESCAPE '\')`
		args = append(args, filter, likeContains(strings.ToLower(filter)))
	}

	rows, err := db.conn.Query(`
		SELECT apps.id, apps.slug, apps.name, apps.status,
		       d.ecosystem, d.package, d.version, d.path, d.direct, d.dev,
		       a.id, a.kind, a.severity, a.summary, a.url,
		       af.introduced, af.fixed, af.last_affected, af.versions,
		       dep.id
		FROM apps
		JOIN deployments dep ON dep.app_id = apps.id AND dep.status = ?
		JOIN deployment_dependencies d ON d.deployment_id = dep.id
		JOIN advisory_affected af ON af.ecosystem = d.ecosystem AND af.package = d.package
		JOIN advisories a ON a.id = af.advisory_id
		WHERE a.withdrawn_at IS NULL`+where+`
		  AND dep.id = `+servingDeploymentSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct{ app, advisory, pkg, version string }
	seen := map[key]bool{}
	byApp := map[string]*AffectedApp{}

	for rows.Next() {
		var appID, slug, name, status, deployID string
		var f advisory.Finding
		var r advisory.Range
		var versions string
		if err := rows.Scan(&appID, &slug, &name, &status,
			&f.Ecosystem, &f.Package, &f.Version, &f.Path, &f.Direct, &f.Dev,
			&f.AdvisoryID, &f.Kind, &f.Severity, &f.Summary, &f.URL,
			&r.Introduced, &r.Fixed, &r.LastAffected, &versions, &deployID); err != nil {
			return nil, err
		}
		if versions != "" {
			r.Versions = strings.Split(versions, ",")
		}
		if advisory.Matches(f.Ecosystem, f.Version, r) != advisory.Matched {
			continue
		}
		k := key{appID, f.AdvisoryID, f.Package, f.Version}
		if seen[k] {
			continue
		}
		seen[k] = true
		f.FixedIn = r.Fixed

		entry, ok := byApp[appID]
		if !ok {
			entry = &AffectedApp{
				AppID: appID, Slug: slug, Name: name, Status: status,
				DeploymentID: deployID,
			}
			byApp[appID] = entry
		}
		entry.Findings = append(entry.Findings, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]AffectedApp, 0, len(byApp))
	for _, entry := range byApp {
		waivers, err := db.liveWaivers(entry.AppID)
		if err != nil {
			return nil, err
		}
		for i := range entry.Findings {
			f := &entry.Findings[i]
			if w, ok := waivers.match(f.AdvisoryID, f.Package); ok {
				f.Waived = true
				f.WaivedBy = w.CreatedByName
				f.WaivedFor = w.Reason
				end := w.ExpiresAt
				f.WaiverEnd = &end
			}
			// Judged with the same policy every other surface uses, so the
			// estate view and the app tab cannot disagree about what blocks.
			f.WouldBlock = !f.Waived && policy.Blocks(*f)
		}
		advisory.SortFindings(entry.Findings)
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// AffectedApp is one app the search matched.
type AffectedApp struct {
	AppID string `json:"app_id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	// Status is the app's own, and the estate view keeps stopped apps out of
	// its headline count: a stopped app is not exposure, and folding it in
	// inflates the number a security review reads. A *sleeping* app counts
	// normally — it wakes on the next request.
	Status       string             `json:"status"`
	DeploymentID string             `json:"deployment_id"`
	Findings     []advisory.Finding `json:"findings"`
}

// Serving reports whether this app is exposure right now.
func (a AffectedApp) Serving() bool { return a.Status != AppStatusStopped }

// AppInventoryState is one app's dependency position, for the estate view.
type AppInventoryState struct {
	AppID        string `json:"app_id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	OwnerID      string `json:"owner_id"`
	OwnerName    string `json:"owner_name,omitempty"`
	Status       string `json:"status"`
	DeploymentID string `json:"deployment_id,omitempty"`
	// Scanned is false when this app has nothing recorded in an ecosystem this
	// build derives findings for. **Unscanned is a state with a name and never
	// reads as zero findings** — most of an estate is in it on the first day,
	// so it is the state the copy has to be best at. It is deliberately *not*
	// "Packages > 0": an R release records every package it installs and is
	// checked against nothing, so the two counts below are two facts and a row
	// can honestly read "61 packages, not scanned".
	Scanned   bool       `json:"scanned"`
	ScannedAt *time.Time `json:"scanned_at,omitempty"`
	// Packages is everything recorded, covered or not, because that is the
	// number that says whether a redeploy would change anything.
	Packages   int        `json:"packages"`
	BaselineAt *time.Time `json:"baseline_at,omitempty"`
	// LastScanAt is when the sweep last took a reading of this app, from
	// dependency_scans. **Nil is never checked**, which is a different fact
	// from checked and found nothing, and the estate view says which.
	LastScanAt *time.Time `json:"last_scan_at,omitempty"`
	// LockedAt and LockedSource are the estate-wide view of the one automatic
	// consequence. An administrator who switched dependency_lock_on on sees
	// here what it has done, which is the only place that is visible in
	// aggregate.
	LockedAt     *time.Time `json:"locked_at,omitempty"`
	LockedSource string     `json:"locked_source,omitempty"`
}

// coveredPlaceholders and coveredEcosystemArgs spell advisory.Covered as SQL.
//
// The set is small and compiled in, so it is interpolated as placeholders
// rather than joined against a table: an ecosystem this build cannot match is
// not a row somebody can add.
func coveredPlaceholders() string {
	return strings.TrimPrefix(strings.Repeat(",?", len(advisory.Ecosystems)), ",")
}

func coveredEcosystemArgs() []any {
	args := make([]any, 0, len(advisory.Ecosystems))
	for _, e := range advisory.Ecosystems {
		args = append(args, e)
	}
	return args
}

// ListAppInventoryStates is every app and which release the findings would
// describe. One query, because the estate view is forty rows on day one and
// forty round trips would be the thing somebody notices about this feature.
func (db *DB) ListAppInventoryStates() ([]AppInventoryState, error) {
	rows, err := db.conn.Query(`
		SELECT apps.id, apps.slug, apps.name, COALESCE(apps.owner_user_id,''),
		       COALESCE(NULLIF(users.name, ''), NULLIF(users.login, ''), users.email, ''),
		       apps.status, apps.dependency_baseline_at,
		       COALESCE(dep.id, ''), dep.dependency_checked_at,
		       (SELECT COUNT(*) FROM deployment_dependencies dd WHERE dd.deployment_id = dep.id),
		       (SELECT COUNT(*) FROM deployment_dependencies dd
		         WHERE dd.deployment_id = dep.id AND dd.ecosystem IN (`+coveredPlaceholders()+`)
		           AND dd.ecosystem IN (SELECT ecosystem FROM advisory_feed_state WHERE snapshot_at IS NOT NULL)),
		       (SELECT MAX(occurred_at) FROM dependency_scans ds WHERE ds.app_id = apps.id),
		       apps.locked_at, apps.locked_source
		FROM apps
		LEFT JOIN users ON users.id = apps.owner_user_id
		LEFT JOIN deployments dep ON dep.id = `+servingDeploymentSQL+`
		ORDER BY apps.slug`, coveredEcosystemArgs()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AppInventoryState{}
	for rows.Next() {
		var s AppInventoryState
		var packages, covered sql.NullInt64
		if err := rows.Scan(&s.AppID, &s.Slug, &s.Name, &s.OwnerID, &s.OwnerName,
			&s.Status, &s.BaselineAt, &s.DeploymentID, &s.ScannedAt, &packages, &covered,
			&s.LastScanAt, &s.LockedAt, &s.LockedSource); err != nil {
			return nil, err
		}
		s.Packages = int(packages.Int64)
		// Scanned means something was recorded in a *covered* ecosystem whose
		// corpus has loaded, not merely that a release exists or that rows do
		// — deps.ScannedPackages, spelled in SQL (DG-5).
		s.Scanned = s.DeploymentID != "" && covered.Valid && covered.Int64 > 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetDependencyBaseline marks an app as having been evaluated once.
//
// **The first sweep of an app tells nobody.** Every existing advisory on every
// app is a first match, so without this the feature's opening act on an
// install with forty apps is a burst of mail about a hundred findings that are
// years old — which is how a security feature gets switched off on day one and
// described afterwards as noise. Per app rather than per install, so one
// onboarded in November is treated the same as one that was there on day one.
func (db *DB) SetDependencyBaseline(appID string, at time.Time) error {
	_, err := db.conn.Exec(
		`UPDATE apps SET dependency_baseline_at=? WHERE id=? AND dependency_baseline_at IS NULL`,
		at, appID)
	return err
}

// ClearDependencyBaselines re-baselines every app.
//
// Called when the watch is switched back on after a period off: the accumulated
// news of that period is not something to fire at forty owners at once, and
// notification should begin again from the next genuine change.
func (db *DB) ClearDependencyBaselines() error {
	_, err := db.conn.Exec(`UPDATE apps SET dependency_baseline_at=NULL`)
	return err
}

// DependencyBaselinedAt reports when an app was first evaluated, or nil.
func (db *DB) DependencyBaselinedAt(appID string) (*time.Time, error) {
	var at *time.Time
	err := db.conn.QueryRow(`SELECT dependency_baseline_at FROM apps WHERE id=?`, appID).Scan(&at)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no such app")
	}
	return at, err
}

// DependencyAlert is the app-list signal: whether the serving release carries
// anything this install counts, and how bad the worst of it is.
//
// **One set, the one the tab badge counts** — deps.Policy.Counts: malware,
// plus anything at or above the threshold. The glyph appears exactly when the
// badge is non-zero, and its colour is the worst severity in it: red for
// malware or critical, amber for anything else. What *happens* about it is the
// mode's business and is said on the app's own page, never by a second glyph.
type DependencyAlert struct {
	// Malware is a malicious package in the release that is serving. It earns
	// the signal in **every** mode, including one whose threshold would not
	// block it.
	Malware bool `json:"malware"`
	// Blocking is "the next deploy would be refused" — deps.Policy.Refuses,
	// so malware under `warn` as well.
	Blocking bool `json:"blocking"`
	// Severity is the worst counted, unwaived advisory serving; absent when
	// nothing but malware is counted.
	Severity string `json:"severity,omitempty"`
}

// DependencyAlerts answers the app list in **one query** for the whole page.
//
// Only apps that have an alert come back, which is normally none of them: the
// caller pays a per-app permission check for the handful that do rather than
// for the estate. That shape is the point — this runs on the dashboard, which
// is the most-hit read on the install, and a findings derivation per row would
// be the thing somebody notices about this feature.
func (db *DB) DependencyAlerts(policy deps.Policy, appIDs []string) (map[string]DependencyAlert, error) {
	out := map[string]DependencyAlert{}
	if policy.Off() || len(appIDs) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(appIDs))
	args := []any{DeployStatusSuccess}
	for i, id := range appIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	rows, err := db.conn.Query(`
		SELECT apps.id,
		       d.ecosystem, d.package, d.version,
		       a.id, a.kind, a.severity,
		       af.introduced, af.fixed, af.last_affected, af.versions
		FROM apps
		JOIN deployments dep ON dep.app_id = apps.id AND dep.status = ?
		JOIN deployment_dependencies d ON d.deployment_id = dep.id
		JOIN advisory_affected af ON af.ecosystem = d.ecosystem AND af.package = d.package
		JOIN advisories a ON a.id = af.advisory_id
		WHERE a.withdrawn_at IS NULL
		  AND apps.id IN (`+strings.Join(placeholders, ", ")+`)
		  AND dep.id = `+servingDeploymentSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Candidate rows first, then waivers for the apps that actually matched —
	// loading every app's waivers up front would be the per-row cost this
	// function exists to avoid.
	type candidate struct {
		appID string
		f     advisory.Finding
	}
	var matched []candidate
	for rows.Next() {
		var c candidate
		var r advisory.Range
		var versions string
		if err := rows.Scan(&c.appID, &c.f.Ecosystem, &c.f.Package, &c.f.Version,
			&c.f.AdvisoryID, &c.f.Kind, &c.f.Severity,
			&r.Introduced, &r.Fixed, &r.LastAffected, &versions); err != nil {
			return nil, err
		}
		if versions != "" {
			r.Versions = strings.Split(versions, ",")
		}
		if advisory.Matches(c.f.Ecosystem, c.f.Version, r) != advisory.Matched {
			continue
		}
		c.f.FixedIn = r.Fixed
		matched = append(matched, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	waivers := map[string]waiverSet{}
	for _, c := range matched {
		if _, ok := waivers[c.appID]; !ok {
			set, err := db.liveWaivers(c.appID)
			if err != nil {
				return nil, err
			}
			waivers[c.appID] = set
		}
		if _, waived := waivers[c.appID].match(c.f.AdvisoryID, c.f.Package); waived {
			continue
		}
		alert := out[c.appID]
		if c.f.Malware() {
			alert.Malware = true
		}
		if policy.Refuses(c.f) {
			alert.Blocking = true
		}
		if policy.Counts(c.f) && !c.f.Malware() && !advisory.AtOrAbove(alert.Severity, c.f.Severity) {
			alert.Severity = c.f.Severity
		}
		if alert.Malware || alert.Blocking || alert.Severity != "" {
			out[c.appID] = alert
		}
	}
	return out, nil
}

// The inventory's second reader: governance rather than exposure.
//
// Everything above this line answers "what is wrong". What follows answers
// "what do we have", which is a different question asked by different people
// for reasons that have nothing to do with an advisory — an upgrade being
// planned and somebody wanting the blast radius, a library deprecated
// internally, a license that changed, a vendor asking which of your tools embed
// their SDK and at what version.
//
// It is a second reader of the same rows and never a flag on the first. The
// findings path inner-joins the advisory corpus and then discards any row whose
// installed version falls outside the range, which is exactly right for
// exposure and is what makes it unable to answer any of the above: an app that
// installs the package and is not vulnerable is not in its result, and neither
// is a package no advisory has ever mentioned.

// PackageFilter is the predicate a governance query carries.
//
// The version half is an advisory.Range because that is the shape this
// codebase already compares versions with. **No second opinion about version
// ordering**: npm semver, PEP 440 and Go semver each keep their own, and a
// comparator written here to serve a search box would be a fourth one that
// could disagree with the matcher a refusal is issued from.
type PackageFilter struct {
	// Query is a substring of the package name, matched case-insensitively.
	// Substring rather than exact because "@types/lodash" is sometimes the
	// point; every row carries the name it matched so the reader can see which.
	Query string
	// Ecosystem narrows the name match, empty meaning all of them.
	Ecosystem string
	// Direct and Dev narrow the tree. Nil means "either", which is the default
	// on purpose: the governance question is nearly always "is it in there
	// anywhere", and a default that quietly excluded transitives would answer a
	// different question with the same confidence.
	Direct *bool
	Dev    *bool
	// Version is the interval, in the fields an advisory declares one with.
	Version advisory.Range
}

// constrainsVersion reports whether the filter says anything about versions at
// all. With nothing set the predicate is the whole name match, which is the
// common case: *do we have this at all*.
func (f PackageFilter) constrainsVersion() bool {
	return strings.TrimSpace(f.Version.Introduced) != "" ||
		strings.TrimSpace(f.Version.Fixed) != "" ||
		strings.TrimSpace(f.Version.LastAffected) != "" ||
		len(f.Version.Versions) > 0
}

// PackageOccurrence is one (app, package, version) the estate is carrying.
//
// One row per occurrence rather than per app, because an app can carry the same
// package at two versions through two paths and a governance answer has to show
// both — which is the other reason this is not a parameter on the findings
// route, where a row is an app.
type PackageOccurrence struct {
	AppID string `json:"app_id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	// Status is the app's own. Unlike the findings view nothing is counted
	// apart by it: a stopped app is not exposure, but it is still an app that
	// carries the package, and the upgrade still has to reach it.
	Status       string `json:"status"`
	DeploymentID string `json:"deployment_id"`

	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	Version   string `json:"version"`
	// Path is the transitive chain, "your-app > webpack > lodash". It rides on
	// every row because "we don't depend on that" is the first reply to a
	// governance report and the path is what settles it.
	Path   string `json:"path"`
	Direct bool   `json:"direct"`
	Dev    bool   `json:"dev"`
}

// PackageOccurrences is a governance answer: the rows that matched, and the
// ones nothing could be said about.
type PackageOccurrences struct {
	Occurrences []PackageOccurrence `json:"occurrences"`
	// Incomparable is its own bucket, never dropped and never counted as a
	// match: a version that is a git URL, a `file:` path or a local tag has no
	// place on any ordering, and an answer that silently omits the four apps
	// pinned to a fork is worse than no answer because it will be read as
	// complete. The same rule the resolver follows for versions it cannot read.
	Incomparable []PackageOccurrence `json:"incomparable"`
}

// SearchPackageOccurrences answers "which of our apps has this, vulnerable or
// not".
//
// Scoped to **the release each app is serving** — the same scope as the estate
// view, and the right answer after a rollback because the rollback's own row
// carries the restored inventory. What that costs is stated where a reader can
// see it: an app that shipped a bad version in March and rolled off it in April
// does not appear here. "Have we ever run this" is a different question with a
// retention interaction, since these rows go when the sweep prunes their
// deployment, and it would be a separate route with its own honest statement
// rather than a parameter on this one.
func (db *DB) SearchPackageOccurrences(f PackageFilter) (PackageOccurrences, error) {
	out := PackageOccurrences{Occurrences: []PackageOccurrence{}, Incomparable: []PackageOccurrence{}}

	where, args := f.sql()
	rows, err := db.conn.Query(`
		SELECT apps.id, apps.slug, apps.name, apps.status,
		       d.ecosystem, d.package, d.version, d.path, d.direct, d.dev, dep.id
		FROM apps
		JOIN deployments dep ON dep.app_id = apps.id AND dep.status = ?
		JOIN deployment_dependencies d ON d.deployment_id = dep.id
		WHERE dep.id = `+servingDeploymentSQL+where+`
		ORDER BY apps.slug, d.ecosystem, d.package, d.version`,
		append([]any{DeployStatusSuccess}, args...)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	for rows.Next() {
		var o PackageOccurrence
		if err := rows.Scan(&o.AppID, &o.Slug, &o.Name, &o.Status,
			&o.Ecosystem, &o.Package, &o.Version, &o.Path, &o.Direct, &o.Dev,
			&o.DeploymentID); err != nil {
			return out, err
		}
		if !f.constrainsVersion() {
			out.Occurrences = append(out.Occurrences, o)
			continue
		}
		switch advisory.Matches(o.Ecosystem, o.Version, f.Version) {
		case advisory.Matched:
			out.Occurrences = append(out.Occurrences, o)
		case advisory.Incomparable:
			out.Incomparable = append(out.Incomparable, o)
		}
	}
	return out, rows.Err()
}

// sql renders the half of the filter a database can answer. The version half is
// deliberately not here: see PackageFilter.
func (f PackageFilter) sql() (string, []any) {
	where := ""
	args := []any{}
	if q := strings.TrimSpace(f.Query); q != "" {
		where += ` AND LOWER(d.package) LIKE ? ESCAPE '\'`
		args = append(args, likeContains(strings.ToLower(q)))
	}
	if e := strings.TrimSpace(f.Ecosystem); e != "" {
		where += " AND d.ecosystem = ?"
		args = append(args, e)
	}
	if f.Direct != nil {
		where += " AND d.direct = ?"
		args = append(args, *f.Direct)
	}
	if f.Dev != nil {
		where += " AND d.dev = ?"
		args = append(args, *f.Dev)
	}
	return where, args
}

// PackageCatalogEntry is one (ecosystem, package) the estate carries, with the
// two counts that mean different things.
type PackageCatalogEntry struct {
	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	// Apps is how many apps carry it and Occurrences is how many (app, version)
	// rows there are. **Both, labelled**, because they answer different
	// questions and the difference is the finding: an app carrying lodash twice
	// at two versions is one app and two occurrences, and the fragmentation
	// number people want is the second while the exposure number is the first.
	Apps        int `json:"apps"`
	Occurrences int `json:"occurrences"`
	// Versions is every distinct version in use, ordered newest first where
	// this binary can order them.
	Versions []string `json:"versions"`
	Newest   string   `json:"newest,omitempty"`
	Oldest   string   `json:"oldest,omitempty"`
	// Incomparable counts versions that could not be ordered, so a package
	// whose spread is unknown does not read as a package with no spread.
	Incomparable int `json:"incomparable"`
}

// PackageCatalog is the other direction of the same query: the estate's package
// list, with no name to type.
//
// A search box cannot be used for *how fragmented are we*, *what is the long
// tail*, *which library is in everything* — you have to know the name to type
// it. "We are carrying five versions of express" is a governance finding on its
// own and no other surface can produce it.
//
// Bounded like the estate view rather than capped in SQL: this is an install
// with hundreds of apps, not millions of rows, and the list is paged where it
// is rendered.
func (db *DB) PackageCatalog(f PackageFilter) ([]PackageCatalogEntry, error) {
	where, args := f.sql()
	from := `
		FROM apps
		JOIN deployments dep ON dep.app_id = apps.id AND dep.status = ?
		JOIN deployment_dependencies d ON d.deployment_id = dep.id
		WHERE dep.id = ` + servingDeploymentSQL + where
	full := append([]any{DeployStatusSuccess}, args...)

	rows, err := db.conn.Query(`
		SELECT d.ecosystem, d.package,
		       COUNT(DISTINCT apps.id), COUNT(*)`+from+`
		GROUP BY d.ecosystem, d.package
		ORDER BY d.ecosystem, d.package`, full...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PackageCatalogEntry{}
	at := map[string]int{}
	for rows.Next() {
		var e PackageCatalogEntry
		if err := rows.Scan(&e.Ecosystem, &e.Package, &e.Apps, &e.Occurrences); err != nil {
			return nil, err
		}
		e.Versions = []string{}
		at[e.Ecosystem+"\x00"+e.Package] = len(out)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The versions come separately and are ordered in Go, because ordering them
	// is advisory.Compare's job and SQL has no opinion this codebase trusts.
	vrows, err := db.conn.Query(`
		SELECT DISTINCT d.ecosystem, d.package, d.version`+from, full...)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()
	for vrows.Next() {
		var eco, pkg, version string
		if err := vrows.Scan(&eco, &pkg, &version); err != nil {
			return nil, err
		}
		if i, ok := at[eco+"\x00"+pkg]; ok {
			out[i].Versions = append(out[i].Versions, version)
		}
	}
	if err := vrows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		orderVersions(&out[i])
	}
	return out, nil
}

// orderVersions sorts an entry's versions newest first and records the ones
// that could not be placed.
//
// A version this binary cannot parse is counted rather than dropped and never
// becomes the newest or the oldest: "5.0.0-git-fork" sorting to the top would
// make the spread a lie in the direction nobody checks.
func orderVersions(e *PackageCatalogEntry) {
	ordered := make([]string, 0, len(e.Versions))
	var odd []string
	for _, v := range e.Versions {
		if _, ok := advisory.Compare(e.Ecosystem, v, v); ok {
			ordered = append(ordered, v)
		} else {
			odd = append(odd, v)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		c, ok := advisory.Compare(e.Ecosystem, ordered[i], ordered[j])
		if !ok {
			return false
		}
		return c > 0
	})
	sort.Strings(odd)
	e.Incomparable = len(odd)
	if len(ordered) > 0 {
		e.Newest = ordered[0]
		e.Oldest = ordered[len(ordered)-1]
	}
	e.Versions = append(ordered, odd...)
}
