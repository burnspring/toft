package builder

import (
	"toft/internal/artifact"
	"toft/internal/database"
)

// Counting versions, for the platform's per-app version cap.
//
// A "version" here is a build that can still be made current: a successful
// deployment whose release is present on this host or fetchable from the
// artifact store. It is deliberately not "a row in `deployments`". A failed
// build owns no release, and a row whose release was pruned years ago costs
// nothing and can never be rolled back to — counting either against a cap
// would block deploys over history rather than over storage, which is the
// opposite of what an administrator setting the cap is trying to bound.
//
// This is the same predicate the rollback handler uses to decide whether a
// build is still restorable (ReleaseAvailable), which is what keeps "versions
// you have" and "versions you could go back to" the same number.

// retentionFor resolves what this deploy should keep, reading the platform
// settings fresh — see the call site.
func (b *Builder) retentionFor(app *database.App) (database.Retention, error) {
	s, err := b.db.GetPlatformSettings()
	if err != nil {
		return database.Retention{}, err
	}
	return database.ResolveRetention(app, s), nil
}

// VersionPolicy resolves an app's retention and, when the number can change a
// decision, how many versions it has. The count is -1 when it was not taken.
//
// It lives here rather than in the API because counting versions means asking
// the release directory and the artifact store, and neither of those is
// something a caller outside this package may reach for: a release is this
// package's to locate. Both the deploy paths and the auto-deploy poller ask
// the same question, and asking it in one place is what keeps a build the
// poller starts and a build a person starts subject to the same cap.
func (b *Builder) VersionPolicy(app *database.App) (database.Retention, int, error) {
	ret, err := b.retentionFor(app)
	if err != nil {
		return database.Retention{}, -1, err
	}
	if !ret.NeedsCount() {
		return ret, -1, nil
	}
	versions, err := b.CountAppVersions(app)
	if err != nil {
		return ret, -1, err
	}
	return ret, versions, nil
}

// CountAppVersions counts one app's remaining versions unconditionally. Callers
// that only want the number when it decides something should ask
// Retention.NeedsCount first — see VersionPolicy, which is that pairing.
func (b *Builder) CountAppVersions(app *database.App) (int, error) {
	deployments, err := b.db.ListDeployments(app.ID)
	if err != nil {
		return -1, err
	}
	return CountVersions(b.opts.AppsDir, b.artifacts, app.ID, deployments), nil
}

// CountVersions reports how many of an app's deployments still have a release.
//
// The cost is one filesystem check per successful deployment, plus a store
// round trip for each one whose directory is gone. In the case this is called
// for — an app with automatic cleanup turned off, which is the only app a cap
// can block — nothing has been pruned, so every check is answered by the
// filesystem and the store is never asked.
func CountVersions(appsDir string, store artifact.Store, appID string, deployments []database.Deployment) int {
	n := 0
	for i := range deployments {
		d := &deployments[i]
		if d.Status != database.DeployStatusSuccess {
			continue
		}
		if ReleaseAvailable(appsDir, store, appID, d.ID, d.ArtifactKey) {
			n++
		}
	}
	return n
}
