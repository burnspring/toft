package builder

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/sources"
)

// Enforcement point 4, tested on its own.
//
// It is not redundant with the create route: the policy can change between an
// app's creation and any later build, and this is the last place where the
// decision is still cheap. It is also the point that protects creation paths
// added later by somebody who has read neither document — so it has to work
// with nothing else having run.

func TestSourceRefusalCatchesAPolicyChangedAfterTheAppWasCreated(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "probe", "") // https://github.com/example/probe

	// Created under the default policy, which refuses nothing.
	if why := b.sourceRefusal(app); why != "" {
		t.Fatalf("a fresh install refused a clone: %s", why)
	}

	// An administrator restricts the install afterwards. Nothing about the app
	// changed, and the next build must still be refused.
	if err := db.SetSourcePolicy(sources.ModeAllowlist, true,
		[]database.AllowedGitSource{{Pattern: "gitlab.internal/*"}}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}
	why := b.sourceRefusal(app)
	if why == "" {
		t.Fatal("the builder cloned from a source the policy no longer allows")
	}
	// The same sentence the New App form and the API's error field carry —
	// four wordings of one rule is how a platform sounds like it has four.
	if !strings.Contains(why, "github.com/example/probe") || !strings.Contains(why, "gitlab.internal/*") {
		t.Errorf("the build log line does not name the source and the way out: %q", why)
	}
}

// An uploaded app has no remote, so there is nothing for the git policy to
// refuse — including under `none`, which would otherwise stop every redeploy of
// every uploaded app on the install.
func TestSourceRefusalIgnoresUploadedApps(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "zipped", "")
	if err := db.SetAppRepoURL(app.ID, UploadScheme+"thing.zip"); err != nil {
		t.Fatalf("SetAppRepoURL: %v", err)
	}
	app.RepoURL = UploadScheme + "thing.zip"

	if err := db.SetSourcePolicy(sources.ModeNone, true, nil, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}
	if why := b.sourceRefusal(app); why != "" {
		t.Errorf("an uploaded app was refused by the git policy: %s", why)
	}
}
