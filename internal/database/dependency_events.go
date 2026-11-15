package database

import (
	"strings"
	"time"

	"toft/internal/deps"
)

// The sweep's half of app_events: which advisories an app is already known to
// be matching, and closing the ones it no longer is.
//
// **`app_events` is where this stays, and no new table is added for findings.**
// `events.Recorder` already has the fingerprint and the resolution machinery,
// and the fingerprint here is the advisory id — so one open row per (app,
// advisory), written when a serving release first matches and resolved when it
// stops. Never one per sweep: the sweep runs every six hours forever, and 1,460
// identical rows a year would bury the one that explains the problem.
//
// `audit_log` gains no new writers from any of this. Waivers and settings
// changes write there; discovering a CVE is not an administrative action.

// OpenEventFingerprints is the set of advisories an app already has an
// unresolved event for, so the sweep can tell a new match from a continuing
// one.
func (db *DB) OpenEventFingerprints(appID, kind string) (map[string]bool, error) {
	rows, err := db.conn.Query(`
		SELECT fingerprint FROM app_events
		WHERE app_id = ? AND kind = ? AND resolved_at IS NULL`, appID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		out[fp] = true
	}
	return out, rows.Err()
}

// ResolveEventsExcept closes every open event of a kind whose fingerprint is
// not in keep.
//
// Per fingerprint rather than per kind, unlike Recorder.Clear, because an app
// matching four advisories and fixing one has three problems left — and
// closing them all because a sweep found *something* would be the version of
// this that reports what it wants to hear.
//
// Nothing is notified on resolution. An advisory that stops matching is
// somebody having upgraded a package, and mailing them about the thing they
// just did is how a channel earns a filter rule.
func (db *DB) ResolveEventsExcept(appID, kind string, keep []string) (int64, error) {
	now := time.Now().UTC()
	if len(keep) == 0 {
		res, err := db.conn.Exec(`
			UPDATE app_events SET resolved_at = ?
			WHERE app_id = ? AND kind = ? AND resolved_at IS NULL`, now, appID, kind)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}

	placeholders := make([]string, len(keep))
	args := []any{now, appID, kind}
	for i, fp := range keep {
		placeholders[i] = "?"
		args = append(args, fp)
	}
	res, err := db.conn.Exec(`
		UPDATE app_events SET resolved_at = ?
		WHERE app_id = ? AND kind = ? AND resolved_at IS NULL
		  AND fingerprint NOT IN (`+strings.Join(placeholders, ", ")+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListServingReleases is every app with a release the sweep should judge.
//
// The release is servingDeploymentSQL's — what the app row serves, which after
// a rollback is not the newest success, so the sweep judges the code running.
//
// It returns the deps package's own type rather than a database one, so that
// the sweep can declare its Store interface in terms this platform's policy
// layer owns — the same reason advisory.Store exists.
//
// Stopped apps are included and flagged rather than filtered. A stopped app is
// not exposure and must stay out of the estate view's headline count, but its
// findings are still a fact about it, and deciding that here would make the
// number the sweep produces disagree with the number the tab shows.
func (db *DB) ListServingReleases() ([]deps.ServingRelease, error) {
	rows, err := db.conn.Query(`
		SELECT apps.id, apps.slug, apps.status, apps.repo_url,
		       apps.dependency_baseline_at, dep.id,
		       apps.framework, apps.running_mode, apps.release_ref
		FROM apps
		JOIN deployments dep ON dep.id = ` + servingDeploymentSQL + `
		ORDER BY apps.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []deps.ServingRelease{}
	for rows.Next() {
		var r deps.ServingRelease
		var repoURL string
		if err := rows.Scan(&r.AppID, &r.Slug, &r.Status, &repoURL,
			&r.BaselinedAt, &r.DeploymentID,
			&r.Framework, &r.RunningMode, &r.ReleaseRef); err != nil {
			return nil, err
		}
		r.Stopped = r.Status == AppStatusStopped
		out = append(out, r)
	}
	return out, rows.Err()
}
