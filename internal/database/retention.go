package database

import (
	"fmt"
	"time"
)

// Release retention: how many versions of an app survive, who decides, and
// what happens when the two answers disagree.
//
// Two settings meet here. An administrator may cap versions per app
// (SettingMaxVersions); an app owner may turn automatic cleanup off and choose
// how many versions to keep. The cap wins on the number, but it never rewrites
// what the owner asked for — see ResolveRetention.

// DefaultReleasesKept is how many versions an app keeps when nobody has said
// otherwise. Three is what every deploy has pruned to since before either
// setting existed, and changing that number is not what this feature is for.
const DefaultReleasesKept = 3

// Retention is the resolved answer for one app: what happens on its next
// deploy, and what to tell its owner about why.
type Retention struct {
	// AutoDelete is whether a deploy prunes older releases at all.
	AutoDelete bool `json:"auto_delete"`
	// Keep is the number actually applied — the owner's figure, lowered to the
	// platform maximum when there is one. Never below 1: a keep of zero would
	// mean deleting the release being deployed.
	Keep int `json:"keep"`
	// Requested is what the owner stored, which is *not* clamped. An admin who
	// raises the cap later restores the owner's intent without anyone having to
	// re-enter it, and that only works if the original number survives.
	Requested int `json:"requested"`
	// Max is the platform cap, 0 when there is none.
	Max int `json:"max"`
	// Conflict is true when the owner asked for more than the platform allows.
	// The API does not refuse such a request — it stores it and reports this,
	// so the settings page can explain the gap rather than silently swallowing
	// a number the owner typed.
	Conflict bool `json:"conflict"`
}

// ResolveRetention combines an app's retention settings with the platform's.
//
// A pure function on purpose: it is the whole policy, it is read from the API,
// the builder and the UI, and it is the piece most likely to be got subtly
// wrong, so it is worth being able to test on its own.
func ResolveRetention(app *App, s PlatformSettings) Retention {
	r := Retention{
		AutoDelete: app.AutoDeleteReleases,
		Requested:  app.ReleasesKept,
		Max:        s.MaxVersions,
	}
	if r.Requested <= 0 {
		// No opinion stored. The default is not a conflict however low the cap
		// is — the owner never asked for anything.
		r.Keep = DefaultReleasesKept
	} else {
		r.Keep = r.Requested
	}
	if r.Max > 0 && r.Keep > r.Max {
		r.Keep = r.Max
		// Only a stored figure can conflict. Being handed a lower number than
		// the built-in default is the platform working, not a disagreement.
		r.Conflict = r.Requested > r.Max
	}
	if r.Keep < 1 {
		r.Keep = 1
	}
	return r
}

// BlocksDeploy reports whether a new version may not be built, given how many
// versions the app currently has.
//
// Only an app with cleanup turned off can ever be blocked: with it on, the
// deploy that would exceed the cap prunes down to it in the same pass, so the
// count cannot climb. That is the shape of the whole feature — the block is
// what makes "keep everything" bounded, not a limit the platform imposes on
// apps that are already tidying up after themselves.
func (r Retention) BlocksDeploy(versions int) bool {
	return r.NeedsCount() && versions >= r.Max
}

// NeedsCount reports whether counting this app's versions can change anything.
//
// It cannot with no cap, and it cannot with cleanup on — the deploy that would
// exceed the cap prunes to it in the same pass. Counting means walking release
// directories and, for anything already pruned, asking the artifact store, so
// the cheap cases are worth naming: this is what keeps an app detail page from
// issuing a round trip per deployment row to render a number that would have
// been decorative.
func (r Retention) NeedsCount() bool {
	return r.Max > 0 && !r.AutoDelete
}

// BlockReason is the refusal to show, or "" when the deploy may proceed. A
// negative count means nobody counted, which is never a block.
//
// One sentence, defined once, because three paths refuse a deploy for this
// reason — the deploy endpoint, the upload endpoint, and the auto-deploy
// poller writing a failed build row — and the app's own settings page shows
// the same sentence before anyone tries. Four wordings of one rule is how a
// platform ends up sounding like it has four rules.
//
// It names both ways out because they are genuinely different choices and the
// platform has no business making it: delete builds you no longer need, or
// accept automatic cleanup and stop deciding.
func (r Retention) BlockReason(versions int) string {
	if versions < 0 || !r.BlocksDeploy(versions) {
		return ""
	}
	return fmt.Sprintf(
		"this app is keeping %d versions and the platform maximum is %d — "+
			"delete versions you no longer need, or turn on automatic cleanup in the app's settings",
		versions, r.Max)
}

// SetAppRetention stores an app's cleanup settings.
//
// Its own setter rather than a field on UpdateApp, for the reason branch,
// auto-deploy and execution mode each have one: the builder calls UpdateApp
// mid-build with an App it loaded before the build started, so a retention
// change written there could be silently reverted by an overlapping deploy —
// and this is a setting whose whole purpose is to be read by the next deploy.
//
// keep is stored as given, including a figure above the platform maximum. See
// Retention.Requested.
func (db *DB) SetAppRetention(id string, autoDelete bool, keep int) error {
	if keep < 0 {
		keep = 0
	}
	_, err := db.conn.Exec(
		`UPDATE apps SET auto_delete_releases=?, releases_kept=?, updated_at=? WHERE id=?`,
		autoDelete, keep, time.Now().UTC(), id)
	return err
}

// SetAppNeverSleep exempts an app from idle sleep, or returns it to the
// install-wide timeout. Administrator-only at the API; nothing here enforces
// that, the handler does.
//
// Audited by the caller in the same request rather than here: unlike execution
// mode this changes nothing about who may do what, so it does not belong in
// the same transaction as an authorization write.
func (db *DB) SetAppNeverSleep(id string, neverSleep bool) error {
	_, err := db.conn.Exec(`UPDATE apps SET never_sleep=?, updated_at=? WHERE id=?`,
		neverSleep, time.Now().UTC(), id)
	return err
}

// CountAppsOverVersionCap reports how many apps would be blocked by a version
// cap of max — cleanup off, and already at or above it.
//
// It exists so an administrator setting the cap is told what it affects before
// they save rather than afterwards, the way CountAppsInMode does for execution
// mode. Deliberately counts deployment *rows* that succeeded rather than
// releases still on disk: this is a warning, it runs across every app on the
// install, and one that over-reports slightly is far better than one that
// issues an artifact-store round trip per deployment row.
func (db *DB) CountAppsOverVersionCap(max int) (int, error) {
	if max <= 0 {
		return 0, nil
	}
	var n int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT apps.id
			FROM apps
			JOIN deployments ON deployments.app_id = apps.id AND deployments.status = ?
			WHERE NOT apps.auto_delete_releases
			GROUP BY apps.id
			HAVING COUNT(deployments.id) >= ?
		) AS over`, DeployStatusSuccess, max).Scan(&n)
	return n, err
}
