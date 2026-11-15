package builder

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

// Where a release runs, and what it may reach — decided before the deploy does
// any work, so a refusal costs a queue slot rather than a build.

func testBuilder(t *testing.T, db *database.DB) *Builder {
	t.Helper()
	return &Builder{db: db, rts: runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)}
}

func mkApp(t *testing.T, db *database.DB, slug, mode string) *database.App {
	t.Helper()
	owner := &database.User{ID: "owner-" + slug, Login: "owner-" + slug}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		ID: "app-" + slug, Slug: slug, Name: slug, OwnerID: owner.ID,
		RepoURL: "https://github.com/example/" + slug, Branch: "main",
		Status: database.AppStatusStopped, ExecutionMode: mode,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

func mapStorage(t *testing.T, db *database.DB, app *database.App, slug, path, access string) {
	t.Helper()
	res := &database.StorageResource{Name: slug, Slug: slug, Kind: database.StorageKindVolume}
	if err := db.CreateStorageResource(res, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	m := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: path, Access: access}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
}

func discard(string) {}

// An app with storage mapped, deploying in shared mode, fails before anything
// is cloned.
//
// This is the invariant the whole feature rests on. Starting it anyway would
// leave the app writing into a release directory that the next deploy abandons
// and pruning deletes — data loss that looks exactly like a successful deploy.
func TestSharedDeployWithStorageIsRefused(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "reports", string(runtime.ModeShared))
	mapStorage(t, db, app, "shared-cache", "/data/cache", runtime.AccessWrite)

	_, _, err := b.resolveTarget(app, &BuildJob{App: app}, discard)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// The reader has a deploy that used to work and needs to know which of two
	// things to change.
	for _, want := range []string{"shared-cache", "isolated mode", "remove its storage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q, got: %v", want, err)
		}
	}
}

// A store mapping is not storage with nowhere to put it: a shared workload
// reaches it over signed URLs. So it deploys in shared mode on an install with no
// root — and a volume beside it is still refused, by name.
func TestASharedDeployWithAStoreIsNotRefused(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "reports", string(runtime.ModeShared))
	res := &database.StorageResource{Name: "uploads", Slug: "uploads", Kind: database.StorageKindStore,
		Bucket: "acme", Prefix: "team/uploads"}
	if err := db.CreateStorageResource(res, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	m := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: "/data/uploads", Access: runtime.AccessWrite}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}

	_, mounts, err := b.resolveTarget(app, &BuildJob{App: app}, discard)
	if err != nil {
		t.Fatalf("a store mapping was refused: %v", err)
	}
	if len(mounts) != 1 || mounts[0].Kind != runtime.MountStore {
		t.Fatalf("mounts = %+v", mounts)
	}

	mapStorage(t, db, app, "shared-cache", "/data/cache", runtime.AccessWrite)
	_, _, err = b.resolveTarget(app, &BuildJob{App: app}, discard)
	if err == nil || !strings.Contains(err.Error(), "shared-cache") || strings.Contains(err.Error(), "uploads") {
		t.Errorf("refusal = %v, want the volume named and the store not", err)
	}
}

// The same app, promoted, deploys — and carries its mounts.
func TestIsolatedDeployCarriesItsStorage(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "reports", string(runtime.ModeIsolated))
	mapStorage(t, db, app, "shared-cache", "/data/cache", runtime.AccessWrite)

	mode, mounts, err := b.resolveTarget(app, &BuildJob{App: app}, discard)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if mode != runtime.ModeIsolated {
		t.Errorf("mode = %q, want isolated", mode)
	}
	if len(mounts) != 1 || mounts[0].Path != "/data/cache" {
		t.Fatalf("mounts = %+v", mounts)
	}
}

// An app with no storage is untouched by any of this, which is the
// overwhelmingly common case.
func TestOrdinaryAppsResolveToTheirDesiredMode(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "plain", string(runtime.ModeShared))

	mode, mounts, err := b.resolveTarget(app, &BuildJob{App: app}, discard)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if mode != runtime.ModeShared || len(mounts) != 0 {
		t.Errorf("mode = %q, mounts = %v", mode, mounts)
	}
}

// A rollback restarts an earlier release's directory exactly as it was built,
// so it inherits the mode that release ran under.
//
// Anything else would be a mode change with no rebuild: a natively built tree
// started inside a runner image, or the reverse. Both fail at exec, for a
// reason nobody traces back to a rollback.
func TestRollbackInheritsTheSourceReleasesMode(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	// The app has since been returned to shared…
	app := mkApp(t, db, "reports", string(runtime.ModeShared))

	// …but the release being rolled back to was built and started isolated.
	source := &database.Deployment{
		AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess,
		ExecutionMode: string(runtime.ModeIsolated),
	}
	if err := db.CreateDeployment(source, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	var logged []string
	mode, _, err := b.resolveTarget(app, &BuildJob{App: app, SourceDeployID: source.ID},
		func(line string) { logged = append(logged, line) })
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if mode != runtime.ModeIsolated {
		t.Fatalf("mode = %q, want the source release's isolated", mode)
	}
	// And the admin is told, because the rollback is also a mode change.
	if !strings.Contains(strings.Join(logged, "\n"), "isolated") {
		t.Errorf("the mode change should be logged, got: %v", logged)
	}
}

// A deployment row with no mode is one written before modes were per-release,
// and every one of those is a shared process. Reading it as anything else would
// resurrect them somewhere they were never built to run.
func TestOlderDeploymentsRollBackAsShared(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "reports", string(runtime.ModeIsolated))

	source := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess}
	if err := db.CreateDeployment(source, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	mode, _, err := b.resolveTarget(app, &BuildJob{App: app, SourceDeployID: source.ID}, discard)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if mode != runtime.ModeShared {
		t.Errorf("mode = %q, want shared", mode)
	}
}

// A rollback whose inherited mode would need storage the mode cannot provide is
// refused for the same reason a fresh deploy is — the validation applies to the
// inherited mode, not to the app's current setting.
func TestRollbackToASharedReleaseWithStorageIsRefused(t *testing.T) {
	db := dbtest.New(t)
	b := testBuilder(t, db)
	app := mkApp(t, db, "reports", string(runtime.ModeIsolated))
	mapStorage(t, db, app, "shared-cache", "/data/cache", runtime.AccessWrite)

	source := &database.Deployment{
		AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess,
		ExecutionMode: string(runtime.ModeShared),
	}
	if err := db.CreateDeployment(source, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	if _, _, err := b.resolveTarget(app, &BuildJob{App: app, SourceDeployID: source.ID}, discard); err == nil {
		t.Fatal("rolling back to a shared release with storage mapped must be refused")
	}
}

// Loopback is right for a process sharing Toft's network namespace and
// wrong for a workload whose namespace *is* the boundary. The invariant —
// unreachable without passing the gateway — is the same either way.
func TestBindAddressFollowsTheMode(t *testing.T) {
	if got := bindAddressFor(runtime.ModeShared); got != runtime.BindLoopback {
		t.Errorf("shared binds %q, want loopback", got)
	}
	if got := bindAddressFor(runtime.ModeIsolated); got != runtime.BindAll {
		t.Errorf("isolated binds %q, want all interfaces", got)
	}
}
