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

func newStore(t *testing.T) artifact.Store {
	t.Helper()
	s, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// This is the live defect the artifact store exists to fix: on a topology
// where the apps directory does not survive a task replacement, the release
// tree is gone at boot while the database still says the app is running. The
// resolver must bring it back rather than report a missing release.
func TestResolverRestoresAReleaseWhoseDirectoryIsGone(t *testing.T) {
	store := newStore(t)
	appsDir := t.TempDir()
	appID, deployID := "app-1", "deploy-1"
	ctx := context.Background()

	// Build once: a release tree exists and is published.
	built := ReleaseDir(appsDir, appID, deployID)
	if err := os.MkdirAll(built, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(built, "app.py"), []byte("print('hi')"), 0644); err != nil {
		t.Fatal(err)
	}
	key := artifact.ReleaseKey(appID, deployID)
	if _, err := store.Put(ctx, key, built); err != nil {
		t.Fatal(err)
	}

	// The host is replaced: the apps directory is empty again.
	if err := os.RemoveAll(filepath.Join(appsDir, appID)); err != nil {
		t.Fatal(err)
	}

	resolver := ArtifactResolver{Store: store, AppsDir: appsDir}
	dir, err := resolver.Resolve(runtime.ArtifactRelease(key))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dir != built {
		t.Errorf("resolved to %q, want %q", dir, built)
	}

	body, err := os.ReadFile(filepath.Join(dir, "app.py"))
	if err != nil || string(body) != "print('hi')" {
		t.Errorf("release was not restored: %v %q", err, body)
	}

	// And an operator following `current` finds the live code.
	target, err := filepath.EvalSymlinks(CurrentLink(appsDir, appID))
	if err != nil {
		t.Fatalf("current link not repointed: %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(built); target != resolved {
		t.Errorf("current -> %q, want %q", target, resolved)
	}
}

// A shared start runs the tree on disk, and never asks the store for it (ADR
// §7, mode-scoped). The consequence is stated rather than designed around: an
// edit the app made to its own code survives the start. A redeploy, a rollback
// to another release and an emptied disk replace it; an isolated pod start
// fetches regardless.
func TestSharedResolverRunsTheTreeOnDisk(t *testing.T) {
	appsDir := t.TempDir()
	appID, deployID := "app-1", "deploy-1"

	built := ReleaseDir(appsDir, appID, deployID)
	if err := os.MkdirAll(built, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(built, "app.py"), []byte("edited by the app"), 0644); err != nil {
		t.Fatal(err)
	}

	// A store that fails every call: if the resolver touches it, this fails.
	resolver := ArtifactResolver{Store: refusingStore{}, AppsDir: appsDir}
	dir, err := resolver.Resolve(runtime.ArtifactRelease(artifact.ReleaseKey(appID, deployID)))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dir != built {
		t.Errorf("resolved to %q, want %q", dir, built)
	}
	if body, _ := os.ReadFile(filepath.Join(built, "app.py")); string(body) != "edited by the app" {
		t.Errorf("the tree on disk was replaced: %q", body)
	}
}

// A tree the disk no longer has, and a store that cannot be reached: the start
// fails, naming the outage, and leaves nothing half-unpacked behind for the
// next start to mistake for a release.
func TestResolverFailsWithoutAPartialTreeWhenTheFetchFails(t *testing.T) {
	appsDir := t.TempDir()
	appID, deployID := "app-1", "deploy-1"

	resolver := ArtifactResolver{Store: refusingStore{}, AppsDir: appsDir}
	if _, err := resolver.Resolve(runtime.ArtifactRelease(artifact.ReleaseKey(appID, deployID))); err == nil {
		t.Fatal("resolve should fail when the store cannot be reached")
	}
	if fileExists(ReleaseDir(appsDir, appID, deployID)) {
		t.Error("a failed fetch left a release directory behind")
	}
}

// An install with no artifact store must say so plainly rather than fail with
// something that looks like a missing release.
func TestResolverWithoutAStoreRefusesArtifactReferences(t *testing.T) {
	resolver := ArtifactResolver{AppsDir: t.TempDir()}
	_, err := resolver.Resolve(runtime.ArtifactRelease("releases/app-1/deploy-1.tar.gz"))
	if !errors.Is(err, runtime.ErrUnresolvableRelease) {
		t.Fatalf("err = %v, want ErrUnresolvableRelease", err)
	}
}

// Local references keep working untouched — a VM install's entire experience.
//
// And the other half of the replace-on-start rule, recorded rather than left as
// an omission: a local release has nothing to be restored *from*, so a write
// into the tree survives here. An install with no artifact store keeps the
// persistence every install used to have. Absent, not dormant.
func TestResolverPassesLocalReferencesThrough(t *testing.T) {
	dir := t.TempDir()
	resolver := ArtifactResolver{Store: newStore(t), AppsDir: t.TempDir()}
	got, err := resolver.Resolve(runtime.LocalRelease(dir))
	if err != nil || got != dir {
		t.Errorf("got (%q, %v), want (%q, nil)", got, err, dir)
	}

	if err := os.WriteFile(filepath.Join(dir, "written-by-the-app"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(runtime.LocalRelease(dir)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "written-by-the-app")); err != nil {
		t.Errorf("a local release must not be replaced on start: %v", err)
	}
}

// The key format and the on-disk layout have to agree. If they drift, a
// restore unpacks into a directory nothing will ever start from — which would
// look like a successful restore of an app that stays down.
func TestReleaseKeyRoundTripsToItsDirectory(t *testing.T) {
	appID, deployID := "a1b2-c3d4", "d9e8-f7g6"
	key := artifact.ReleaseKey(appID, deployID)

	gotApp, gotDeploy, err := parseReleaseKey(key)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if gotApp != appID || gotDeploy != deployID {
		t.Errorf("parsed (%q, %q), want (%q, %q)", gotApp, gotDeploy, appID, deployID)
	}
}

func TestParseReleaseKeyRejectsMalformedKeys(t *testing.T) {
	for _, key := range []string{
		"",
		"not-a-release-key",
		"releases/",
		"releases/app-1/",
		"releases//deploy-1.tar.gz",
	} {
		t.Run(key, func(t *testing.T) {
			if _, _, err := parseReleaseKey(key); err == nil {
				t.Errorf("%q was accepted", key)
			}
		})
	}
}

// A store outage must not be reported as a pruned release: the remedy for one
// is to wait, and for the other to redeploy, and telling an operator to
// redeploy every app during an outage is the worse mistake.
func TestOutageIsNotReportedAsAMissingRelease(t *testing.T) {
	if missingReleaseErr(artifact.ErrUnreachable) {
		t.Error("an unreachable store must not read as a missing release")
	}
	if !missingReleaseErr(artifact.ErrNotFound) {
		t.Error("a genuinely absent artifact should read as a missing release")
	}
	if !missingReleaseErr(errNoRelease) {
		t.Error("a missing local release should read as a missing release")
	}
}

// refusingStore fails every call, so a test can prove a code path did not
// reach the store at all.
type refusingStore struct{}

func (refusingStore) Put(context.Context, string, string) (artifact.Descriptor, error) {
	return artifact.Descriptor{}, errors.New("store must not be called")
}
func (refusingStore) Get(context.Context, string, string) error {
	return errors.New("store must not be called")
}
func (refusingStore) Stat(context.Context, string) (artifact.Descriptor, error) {
	return artifact.Descriptor{}, errors.New("store must not be called")
}
func (refusingStore) Delete(context.Context, string) error {
	return errors.New("store must not be called")
}
func (refusingStore) Expire(context.Context, string, []string) (int, error) {
	return 0, errors.New("store must not be called")
}

// A host job's release is its own. Resolving in place swaps the directory the
// app's service runs out of, and a job started beside a live app on an install
// with an artifact store deleted the serving tree half way and left one the
// next restart could not start.
func TestResolvePrivateLeavesTheServingTreeAlone(t *testing.T) {
	store := newStore(t)
	appsDir := t.TempDir()
	appID, deployID := "app-1", "deploy-1"
	ctx := context.Background()
	key := artifact.ReleaseKey(appID, deployID)

	serving := ReleaseDir(appsDir, appID, deployID)
	if err := os.MkdirAll(serving, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serving, "app.py"), []byte("print('hi')"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, key, serving); err != nil {
		t.Fatal(err)
	}
	// What the running service has written since; a replace would remove it.
	marker := filepath.Join(serving, "written-while-serving")
	if err := os.WriteFile(marker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(serving)

	dest := filepath.Join(appsDir, appID, "runs", "run-1", "release")
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		t.Fatal(err)
	}
	resolver := ArtifactResolver{Store: store, AppsDir: appsDir}
	dir, err := resolver.ResolvePrivate(runtime.ArtifactRelease(key), dest)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dir != dest {
		t.Errorf("resolved to %q, want the run's own %q", dir, dest)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "app.py")); err != nil || string(body) != "print('hi')" {
		t.Errorf("the run's copy is not the release: %v %q", err, body)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the serving tree was replaced: %v", err)
	}
	if after, err := os.Stat(serving); err != nil || !os.SameFile(before, after) {
		t.Errorf("the serving directory is not the one the service was running out of")
	}

	// A local release is the directory itself: nothing fetched, nothing replaced.
	got, err := resolver.ResolvePrivate(runtime.LocalRelease(serving), filepath.Join(appsDir, "unused"))
	if err != nil || got != serving {
		t.Errorf("local release resolved to %q, %v; want %q", got, err, serving)
	}
}

// A shared app whose APPS_DIR did not survive the restart comes back from the
// store rather than being reported "release directory missing". trytoft moved
// APPS_DIR onto the container's own layer and every shared app crashed at the
// next boot, because Start read only `current` and never the row's reference.
func TestStartRestoresASharedReleaseFromTheStore(t *testing.T) {
	store := newStore(t)
	appsDir := t.TempDir()
	app := &database.App{ID: "app-1", Slug: "one"}
	deployID := "deploy-1"

	built := ReleaseDir(appsDir, app.ID, deployID)
	if err := os.MkdirAll(built, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(built, "app.R"), []byte("shiny"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := artifact.ReleaseKey(app.ID, deployID)
	if _, err := store.Put(context.Background(), key, built); err != nil {
		t.Fatal(err)
	}
	app.ReleaseRef = runtime.ArtifactRelease(key).String()
	if err := os.RemoveAll(filepath.Join(appsDir, app.ID)); err != nil {
		t.Fatal(err)
	}

	b := &Builder{opts: Options{AppsDir: appsDir}, artifacts: store}
	if err := b.restoreCurrent(app); err != nil {
		t.Fatalf("restoreCurrent: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(CurrentLink(appsDir, app.ID), "app.R")); err != nil || string(body) != "shiny" {
		t.Errorf("current does not lead to the restored release: %v %q", err, body)
	}

	// No store, or a row naming a local release: nothing to restore from.
	for _, c := range []struct {
		name string
		b    *Builder
		ref  string
	}{
		{"no store", &Builder{opts: Options{AppsDir: t.TempDir()}}, app.ReleaseRef},
		{"local reference", &Builder{opts: Options{AppsDir: t.TempDir()}, artifacts: store}, runtime.LocalRelease(built).String()},
	} {
		if err := c.b.restoreCurrent(&database.App{ID: app.ID, ReleaseRef: c.ref}); !errors.Is(err, errNoRelease) {
			t.Errorf("%s: restoreCurrent = %v, want errNoRelease", c.name, err)
		}
	}
}
