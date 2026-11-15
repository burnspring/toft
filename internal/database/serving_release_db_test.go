package database_test

import (
	"strings"
	"testing"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deps"
)

// D4: after a rollback the platform named the rolled-away release as serving.
// A rollback leaves no successful row of its own — the builder deletes the
// transient row and the source row is current again — so "the newest success"
// is the release the app rolled *away* from. The serving release is what the
// app row says it serves, in every form a release reference takes.
func TestTheServingReleaseIsWhatTheAppServesAfterARollback(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), at)

	short := func(id string) string { return strings.ReplaceAll(id, "-", "")[:8] }
	cases := map[string]func(app *database.App, back *database.Deployment) (ref, current string){
		"artifact reference": func(app *database.App, back *database.Deployment) (string, string) {
			return "artifact:releases/" + app.ID + "/" + back.ID + ".tar.gz", app.Slug + "-" + short(back.ID)
		},
		"local reference": func(app *database.App, back *database.Deployment) (string, string) {
			return "local:/var/lib/toft/apps/" + app.ID + "/releases/" + back.ID, app.Slug + "-" + short(back.ID)
		},
		"workload name alone": func(app *database.App, back *database.Deployment) (string, string) {
			return "", app.Slug + "-" + short(back.ID)
		},
	}
	for name, refs := range cases {
		t.Run(name, func(t *testing.T) {
			app := mkDepApp(t, db, "rolled-"+strings.ReplaceAll(name, " ", "-"))
			// 46 is vulnerable and is what the app went back to; 47 is the
			// newer, clean release it rolled away from.
			back := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(-time.Hour))
			if err := db.SaveDeploymentDependencies(back.ID, []deps.Dependency{
				{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.15"},
			}); err != nil {
				t.Fatal(err)
			}
			away := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())
			if err := db.SaveDeploymentDependencies(away.ID, []deps.Dependency{
				{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.21"},
			}); err != nil {
				t.Fatal(err)
			}
			app.ReleaseRef, app.CurrentRelease = refs(app, back)
			if err := db.SetAppRelease(app); err != nil {
				t.Fatal(err)
			}

			got, err := db.ServingDeploymentID(app.ID)
			if err != nil || got != back.ID {
				t.Errorf("ServingDeploymentID = %q (err %v), want the rolled-back-to %q, not %q",
					got, err, back.ID, away.ID)
			}
			// The estate-wide readers share the definition.
			affected, err := db.AppsAffectedBy("lodash", blockPolicy(at))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range affected {
				found = found || a.AppID == app.ID
			}
			if !found {
				t.Errorf("the serving release carries a finding the estate did not see: %+v", affected)
			}
		})
	}
}

// A reference naming a build that is not a success — a failed row, or one
// still building — never makes it serving.
func TestTheServingReleaseIsAlwaysASuccessfulBuild(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "never-failed")
	ok := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(-time.Hour))
	failed := mkDeploy(t, db, app.ID, database.DeployStatusFailed, time.Now())
	app.ReleaseRef = "artifact:releases/" + app.ID + "/" + failed.ID + ".tar.gz"
	if err := db.SetAppRelease(app); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ServingDeploymentID(app.ID); got != ok.ID {
		t.Errorf("ServingDeploymentID = %q, want %q", got, ok.ID)
	}
	if got, err := db.ServingDeploymentID("no-such-app"); err != nil || got != "" {
		t.Errorf("an unknown app = %q (err %v), want empty", got, err)
	}
}
