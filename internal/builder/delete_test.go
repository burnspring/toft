package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"toft/internal/artifact"
	"toft/internal/database"
	"toft/internal/runtime"
)

// releaseFixture lays out an apps directory with one release on disk and the
// same release published to a local artifact store.
type releaseFixture struct {
	appsDir  string
	store    *artifact.LocalStore
	appID    string
	deployID string
	key      string
}

func newReleaseFixture(t *testing.T) releaseFixture {
	t.Helper()

	appsDir := t.TempDir()
	store, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("local store: %v", err)
	}

	const appID, deployID = "app-1", "deploy-1"
	dir := ReleaseDir(appsDir, appID, deployID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte("listen()"), 0644); err != nil {
		t.Fatal(err)
	}

	key := artifact.ReleaseKey(appID, deployID)
	if _, err := store.Put(context.Background(), key, dir); err != nil {
		t.Fatalf("publishing the release: %v", err)
	}

	return releaseFixture{appsDir: appsDir, store: store, appID: appID, deployID: deployID, key: key}
}

// Deleting a release has to reach both places a release costs anything.
// Removing only the directory looks like it worked and keeps billing.
func TestDeleteReleaseRemovesDirectoryAndArtifact(t *testing.T) {
	f := newReleaseFixture(t)

	if err := DeleteRelease(f.appsDir, f.store, f.appID, f.deployID, f.key); err != nil {
		t.Fatalf("DeleteRelease: %v", err)
	}

	if _, err := os.Stat(ReleaseDir(f.appsDir, f.appID, f.deployID)); !os.IsNotExist(err) {
		t.Errorf("release directory survived deletion (err=%v)", err)
	}
	if _, err := f.store.Stat(context.Background(), f.key); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("artifact survived deletion: got %v, want ErrNotFound", err)
	}
}

// Deletion is retryable, which means deleting what is already gone has to
// succeed. A first attempt that removed the directory and then failed on the
// artifact must not leave the operator with an error they can never clear.
func TestDeleteReleaseIsIdempotent(t *testing.T) {
	f := newReleaseFixture(t)

	for i := range 2 {
		if err := DeleteRelease(f.appsDir, f.store, f.appID, f.deployID, f.key); err != nil {
			t.Fatalf("DeleteRelease attempt %d: %v", i+1, err)
		}
	}
}

// A rollback row owns no release: its bytes are the source deployment's, and
// the source may still be serving them. Deleting the rollback row must not
// reach through to the release it was pointing at.
func TestDeleteReleaseOfARollbackRowLeavesTheSourceAlone(t *testing.T) {
	f := newReleaseFixture(t)

	// The rollback row's own ID names nothing on disk and published nothing,
	// so its artifact key is empty — which is exactly what the deployment row
	// carries after a rollback.
	if err := DeleteRelease(f.appsDir, f.store, f.appID, "rollback-row", ""); err != nil {
		t.Fatalf("DeleteRelease: %v", err)
	}

	if _, err := os.Stat(ReleaseDir(f.appsDir, f.appID, f.deployID)); err != nil {
		t.Errorf("the source release was deleted along with the rollback row: %v", err)
	}
	if _, err := f.store.Stat(context.Background(), f.key); err != nil {
		t.Errorf("the source artifact was deleted along with the rollback row: %v", err)
	}
}

// The whole point of publishing releases: a build pruned from disk is still
// promotable, because the artifact store can put it back. Answering this from
// the filesystem alone refuses builds the builder would have restored.
func TestReleaseAvailableFallsBackToTheStore(t *testing.T) {
	f := newReleaseFixture(t)

	if err := os.RemoveAll(ReleaseDir(f.appsDir, f.appID, f.deployID)); err != nil {
		t.Fatal(err)
	}

	if !ReleaseAvailable(f.appsDir, f.store, f.appID, f.deployID, f.key) {
		t.Error("a pruned release with a live artifact reported unavailable")
	}
	if ReleaseAvailable(f.appsDir, f.store, f.appID, "never-built", artifact.ReleaseKey(f.appID, "never-built")) {
		t.Error("a release that exists in neither place reported available")
	}
	// No store configured is the VM case: disk is the only answer there.
	if ReleaseAvailable(f.appsDir, nil, f.appID, f.deployID, f.key) {
		t.Error("a pruned release reported available with no store to restore it from")
	}
}

// An install with no artifact store deletes only the directory, and must not
// report failure for the object it was never able to store.
func TestDeleteReleaseWithoutAStore(t *testing.T) {
	f := newReleaseFixture(t)

	if err := DeleteRelease(f.appsDir, nil, f.appID, f.deployID, ""); err != nil {
		t.Fatalf("DeleteRelease: %v", err)
	}
	if _, err := os.Stat(ReleaseDir(f.appsDir, f.appID, f.deployID)); !os.IsNotExist(err) {
		t.Errorf("release directory survived deletion (err=%v)", err)
	}
}

// After a rollback the app's newest deployment row and the release underneath
// it are different rows. Everything that deletes or prunes storage keys on the
// second one, so this is the distinction that stops a tidy-up from deleting
// running code.
func TestLiveReleaseIDFollowsTheReleaseNotTheRow(t *testing.T) {
	appsDir := t.TempDir()

	cases := []struct {
		name string
		app  *database.App
		want string
	}{
		{
			name: "artifact reference names the release it points at",
			app:  &database.App{ID: "app-1", ReleaseRef: runtime.ArtifactRelease(artifact.ReleaseKey("app-1", "source-deploy")).String()},
			want: "source-deploy",
		},
		{
			name: "local reference is the release directory itself",
			app:  &database.App{ID: "app-1", ReleaseRef: runtime.LocalRelease(ReleaseDir(appsDir, "app-1", "source-deploy")).String()},
			want: "source-deploy",
		},
		{
			name: "an app that has never deployed protects nothing",
			app:  &database.App{ID: "app-2"},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LiveReleaseID(appsDir, tc.app); got != tc.want {
				t.Errorf("LiveReleaseID = %q, want %q", got, tc.want)
			}
		})
	}
}

// Rows written before release references existed have only the symlink, and it
// points at the release being served rather than at the newest deployment.
func TestLiveReleaseIDFallsBackToTheCurrentSymlink(t *testing.T) {
	appsDir := t.TempDir()
	app := &database.App{ID: "app-1"}

	if err := EnsureAppDirs(appsDir, app.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ReleaseDir(appsDir, app.ID, "source-deploy"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := linkCurrent(appsDir, app.ID, "source-deploy"); err != nil {
		t.Fatal(err)
	}

	if got := LiveReleaseID(appsDir, app); got != "source-deploy" {
		t.Errorf("LiveReleaseID = %q, want %q", got, "source-deploy")
	}
}
