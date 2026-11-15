package builder

import (
	"fmt"

	"toft/internal/database"
)

// The source policy, as the build subsystem sees it: enforcement point 4 of
// seven, and the one that catches everything the others miss.
//
// The refusal is phrased by internal/sources and returned unchanged, so the
// sentence in a build log is the same sentence the New App form shows and the
// same one the API's error field carries. Four wordings of one rule is how a
// platform ends up sounding like it has four rules.

// sourceRefusal reports why this app's repository may not be cloned, or "" to
// go ahead.
//
// It **fails closed**, which is the opposite of the version cap beside it, and
// the difference is worth stating because the two sit three lines apart. A cap
// the platform cannot read is not a cap it should enforce: getting it wrong
// costs an install where nobody can deploy. A source policy the platform cannot
// read is a security control, and getting *that* wrong the other way costs a
// clone from wherever the URL points. The blast radius of the safe direction is
// also small in practice — a settings read that fails means the database is
// down, and nothing else about a deploy works either.
func (b *Builder) sourceRefusal(app *database.App) string {
	if IsUploadedSource(app.RepoURL) {
		return ""
	}
	policy, err := b.db.ResolveSourcePolicy()
	if err != nil {
		return fmt.Sprintf("This install's source policy could not be read, so the clone was "+
			"refused rather than allowed: %v", err)
	}
	if refusal := policy.Allows(app.RepoURL); refusal != nil {
		return refusal.Error()
	}
	return ""
}

// SourceRefusal is sourceRefusal for callers outside this package that are
// about to make the platform fetch from a remote — today, the auto-deploy
// poller, which asks before ls-remote rather than after a clone.
//
// Exported from here rather than reimplemented there so that the two paths
// cannot drift: the poller and the builder must agree about which apps are
// blocked, or an app would poll, claim a commit, and then fail every build.
func (b *Builder) SourceRefusal(app *database.App) string { return b.sourceRefusal(app) }
