package builder

import (
	"os"
	"slices"
	"testing"
	"time"

	"toft/internal/database"
)

// The test drive's sequence: two good deploys, two failed ones, then a deploy
// that succeeded and is serving. With three kept, the failed builds used to
// take two of the slots and both rollback targets went.
func TestPruneReleasesCountsOnlySuccessfulBuilds(t *testing.T) {
	appsDir := t.TempDir()
	order := []struct{ id, status string }{
		{"good1", database.DeployStatusSuccess},
		{"good2", database.DeployStatusSuccess},
		{"fail1", database.DeployStatusFailed},
		{"fail2", database.DeployStatusFailed},
		{"live", database.DeployStatusSuccess},
		{"running", database.DeployStatusBuilding},
		{"orphan", ""},
	}
	status := map[string]string{}
	base := time.Now().Add(-time.Hour)
	for i, r := range order {
		dir := ReleaseDir(appsDir, "app", r.id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
		if r.status != "" {
			status[r.id] = r.status
		}
	}

	pruned, failed := pruneReleases(appsDir, "app", "live", 3, status, nil)
	if len(pruned) != 0 {
		t.Errorf("pruned %v; live plus two rollback targets fit in three", pruned)
	}
	slices.Sort(failed)
	if !slices.Equal(failed, []string{"fail1", "fail2"}) {
		t.Errorf("failed = %v, want both failed builds removed", failed)
	}
	for _, id := range []string{"good1", "good2", "live", "running", "orphan"} {
		if !fileExists(ReleaseDir(appsDir, "app", id)) {
			t.Errorf("%s was removed", id)
		}
	}

	// Two kept: the one in use and the newest other success.
	pruned, _ = pruneReleases(appsDir, "app", "live", 2, status, nil)
	if !slices.Equal(pruned, []string{"good1"}) {
		t.Errorf("pruned = %v, want [good1]", pruned)
	}
}

// A rollback serves an older release. It counts as one of the kept, and the
// newer builds fill the rest.
func TestPruneReleasesKeepsTheReleaseInUse(t *testing.T) {
	appsDir := t.TempDir()
	status := map[string]string{}
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"old", "mid", "new", "newest"} {
		dir := ReleaseDir(appsDir, "app", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(dir, at, at)
		status[id] = database.DeployStatusSuccess
	}
	pruned, _ := pruneReleases(appsDir, "app", "old", 3, status, nil)
	if !slices.Equal(pruned, []string{"mid"}) {
		t.Errorf("pruned = %v, want [mid]", pruned)
	}
	if !fileExists(ReleaseDir(appsDir, "app", "old")) {
		t.Error("the release in use was removed")
	}
}

// A queued job retry waits with its first attempt's release. Reclaiming it
// failed every remaining attempt before any code ran (trytoft drills rerun #1),
// so a pinned release is kept outside the count.
func TestPruneReleasesKeepsAReleaseAJobRunIsPinnedTo(t *testing.T) {
	appsDir := t.TempDir()
	status := map[string]string{}
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"pinned", "mid", "live"} {
		dir := ReleaseDir(appsDir, "app", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(dir, at, at)
		status[id] = database.DeployStatusSuccess
	}
	pruned, _ := pruneReleases(appsDir, "app", "live", 1, status, map[string]bool{"pinned": true})
	if !slices.Equal(pruned, []string{"mid"}) {
		t.Errorf("pruned = %v, want [mid]", pruned)
	}
	if !fileExists(ReleaseDir(appsDir, "app", "pinned")) {
		t.Error("the release a queued run is pinned to was removed")
	}
}

// Both spellings a run's release_ref is persisted in name their deployment.
func TestReleaseDeployIDReadsBothKinds(t *testing.T) {
	for ref, want := range map[string]string{
		"artifact:releases/app-1/dep-9.tar.gz":  "dep-9",
		"local:/data/apps/app-1/releases/dep-9": "dep-9",
		"/data/apps/app-1/releases/dep-9":       "dep-9",
		"local:/srv/somewhere":                  "",
		"":                                      "",
	} {
		if got := ReleaseDeployID(ref); got != want {
			t.Errorf("ReleaseDeployID(%q) = %q, want %q", ref, got, want)
		}
	}
}
