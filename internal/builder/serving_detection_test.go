package builder

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

// A restore of the serving release starts what that release's build detected,
// not what a later build wrote to the app row (ADR §127). An
// isolated start has no directory to re-detect from, so before the columns it
// restarted a pod mid-build, or after a failed build, as the new release.
func TestARestoreStartsTheServingReleasesOwnDetection(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "reports", string(runtime.ModeIsolated))

	serving := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess}
	if err := db.CreateDeployment(serving, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeploymentDetection(serving.ID, "flask", "app.py", ""); err != nil {
		t.Fatal(err)
	}
	app.CurrentRelease = ReleaseProcessName(app.Slug, serving.ID)
	app.Framework, app.Entrypoint, app.StartCmd = "streamlit", "main.py", "streamlit run main.py"

	if fw, ep, sc := b.servingDetection(app); fw != "flask" || ep != "app.py" || sc != "" {
		t.Errorf("restore would start %q/%q/%q, want the serving build's flask/app.py", fw, ep, sc)
	}

	// A deployment that predates the columns is described by the app row.
	old := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess}
	if err := db.CreateDeployment(old, nil); err != nil {
		t.Fatal(err)
	}
	app.CurrentRelease = ReleaseProcessName(app.Slug, old.ID)
	if fw, ep, _ := b.servingDetection(app); fw != "streamlit" || ep != "main.py" {
		t.Errorf("undescribed release read %q/%q, want the app row's", fw, ep)
	}
}
