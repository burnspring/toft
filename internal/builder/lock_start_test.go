package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/ports"
	"toft/internal/runtime"
)

// A lock withholds starting, and a start that is not a request — a boot
// reconcile, a wake, a restart — is still a start.
func TestALockedAppIsNeverStarted(t *testing.T) {
	db := dbtest.New(t)
	mock := runtime.NewMockRuntime(nil)
	b := &Builder{db: db, rts: runtime.NewRuntimes(mock, nil)}
	app := mkApp(t, db, "held", string(runtime.ModeShared))
	app.CurrentRelease = "held-r1"
	app.Status = database.AppStatusRunning
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAppLock(app.ID, database.BuiltinAdminID, "", time.Now().UTC(), app.Slug, database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}

	// The caller's snapshot predates the lock; startOn reads it live.
	if err := b.startOn(app, mock); !errors.Is(err, ErrAppLocked) {
		t.Fatalf("startOn on a locked app = %v, want ErrAppLocked", err)
	}

	before, _ := db.GetApp(app.ID)
	b.Reconcile()
	if m, err := mock.Metrics("held-r1"); err == nil && runtime.WorkloadIsUp(m.Status) {
		t.Error("boot reconcile started a locked app")
	}
	got, _ := db.GetApp(app.ID)
	if got.Status != before.Status {
		t.Errorf("reconcile moved a locked app from %q to %q", before.Status, got.Status)
	}
}

// specSpy records what a start asked the backend for and goes no further.
type specSpy struct {
	*runtime.MockRuntime
	got *runtime.WorkloadSpec
}

func (s *specSpy) Deploy(spec runtime.WorkloadSpec) (runtime.Endpoint, error) {
	s.got = &spec
	return runtime.Endpoint{}, errors.New("spy: not starting")
}

// A start that is not a deploy — a wake, a restart, a reboot's restore — hands
// a shared workload its storage too. It used to read the mappings for isolated
// workloads only, so a shared app came back with its links and its group and
// no TOFT_STORAGE describing them.
func TestASharedRestartCarriesItsStorage(t *testing.T) {
	db := dbtest.New(t)
	appsDir := t.TempDir()
	spy := &specSpy{MockRuntime: runtime.NewMockRuntime(nil)}
	b := &Builder{db: db, rts: runtime.NewRuntimes(spy, nil), opts: Options{AppsDir: appsDir}}
	app := mkApp(t, db, "reports", string(runtime.ModeShared))
	app.CurrentRelease = "reports-r1"
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	mapStorage(t, db, app, "nightly", "/data/reports", runtime.AccessWrite)
	if err := os.MkdirAll(CurrentLink(appsDir, app.ID), 0o755); err != nil {
		t.Fatal(err)
	}

	_ = b.startOn(app, spy)
	if spy.got == nil {
		t.Fatal("the start never reached the backend")
	}
	if len(spy.got.Mounts) != 1 || spy.got.Mounts[0].Name != "nightly" {
		t.Errorf("mounts = %+v, want the app's mapping", spy.got.Mounts)
	}
}

// tokenSpy answers whether the outgoing process's token was still good at the
// moment the backend was asked to start its replacement.
type tokenSpy struct {
	*runtime.MockRuntime
	db       *database.DB
	oldHash  string
	oldValid bool
}

func (s *tokenSpy) Deploy(spec runtime.WorkloadSpec) (runtime.Endpoint, error) {
	got, _ := s.db.ResolveAppToken(s.oldHash)
	s.oldValid = got != nil
	return runtime.Endpoint{}, errors.New("spy: not starting")
}

// A shared restart reuses the workload's name, so the process it replaces is
// still the one serving until the new one answers. Superseding its token at
// mint time gave a slow-starting app a minute of `invalid_app_token` on every
// SDK call; and a start that never comes up must not have cut it off at all.
func TestARestartKeepsTheServingProcessTokenUntilItsReplacementIsUp(t *testing.T) {
	db := dbtest.New(t)
	appsDir := t.TempDir()
	app := mkApp(t, db, "slowstart", string(runtime.ModeShared))
	app.CurrentRelease = "slowstart-r1"
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAppToken(&database.AppToken{ID: "serving", AppID: app.ID,
		Workload: "slowstart-r1", TokenHash: "h-serving", Prefix: "tofta_srv"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(CurrentLink(appsDir, app.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	spy := &tokenSpy{MockRuntime: runtime.NewMockRuntime(nil), db: db, oldHash: "h-serving"}
	b := &Builder{db: db, rts: runtime.NewRuntimes(spy, nil), opts: Options{AppsDir: appsDir}}

	_ = b.startOn(app, spy)
	if !spy.oldValid {
		t.Error("the serving process's token was revoked before its replacement was started")
	}
	if got, _ := db.ResolveAppToken("h-serving"); got == nil {
		t.Error("a start that never came up revoked the serving process's token")
	}
	// And the token minted for that start is withdrawn: two valid tokens for
	// one workload, one of them never held by a process, was review #6.
	var live int
	if err := db.Conn().QueryRow(`SELECT COUNT(*) FROM app_tokens
		WHERE app_id = ? AND revoked_at IS NULL`, app.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Errorf("live tokens after a failed start = %d, want only the serving one", live)
	}
}

// freePorts hands the mock a free loopback port, so a start can answer and
// reach the supersede that happens only after readiness.
type freePorts struct{}

func (freePorts) Allocate() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
func (freePorts) Release(int) {}

// overlapSpy holds each Deploy until a second one is in flight, or until a
// short wait runs out. Unserialised, two starts of one app meet here — both
// tokens minted, neither process up — which is the interleaving that left
// trytoft's fixture serving a revoked token. Serialised, the second never
// arrives while the first waits, and the wait simply expires.
type overlapSpy struct {
	*runtime.MockRuntime
	mu       sync.Mutex
	inFlight int
	arrived  chan struct{}
	last     string // the AppToken the last completed Deploy was handed
}

func (s *overlapSpy) Deploy(spec runtime.WorkloadSpec) (runtime.Endpoint, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight == 2 {
		close(s.arrived)
	}
	s.mu.Unlock()
	select {
	case <-s.arrived:
	case <-time.After(300 * time.Millisecond):
	}
	ep, err := s.MockRuntime.Deploy(spec)
	s.mu.Lock()
	s.inFlight--
	if err == nil {
		s.last = spec.AppToken
	}
	s.mu.Unlock()
	return ep, err
}

// Two starts of one app at once — two credential detaches a second apart —
// leave exactly one live token, and it is the one the serving process holds.
func TestTwoStartsAtOnceLeaveTheServingProcessALiveToken(t *testing.T) {
	db := dbtest.New(t)
	appsDir := t.TempDir()
	app := mkApp(t, db, "twice", string(runtime.ModeShared))
	app.CurrentRelease = "twice-r1"
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(CurrentLink(appsDir, app.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	spy := &overlapSpy{MockRuntime: runtime.NewMockRuntime(freePorts{}), arrived: make(chan struct{})}
	b := &Builder{db: db, rts: runtime.NewRuntimes(spy, nil), opts: Options{AppsDir: appsDir}}
	t.Cleanup(func() { _ = spy.MockRuntime.Delete("twice-r1") })

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, _ := db.GetApp(app.ID)
			errs[i] = b.startOn(a, spy)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}

	var live int
	if err := db.Conn().QueryRow(`SELECT COUNT(*) FROM app_tokens
		WHERE app_id = ? AND revoked_at IS NULL`, app.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("live tokens after two starts = %d, want exactly 1", live)
	}
	sum := sha256.Sum256([]byte(spy.last))
	if got, _ := db.ResolveAppToken(hex.EncodeToString(sum[:])); got == nil {
		t.Error("the serving process holds a revoked token — every SDK call it makes answers " +
			"invalid_app_token while the app reads running")
	}
}

// A deploy that fails over a sleeping, stopped or crashed release puts that
// state back; writing `running` sent the gateway to a workload that was not
// there (2026-11-03 evening drills).
func TestAFailedDeployRestoresTheStateOfTheReleaseItLeaves(t *testing.T) {
	for before, want := range map[string]string{
		database.AppStatusSleeping: database.AppStatusSleeping,
		database.AppStatusStopped:  database.AppStatusStopped,
		database.AppStatusCrashed:  database.AppStatusCrashed,
		database.AppStatusRunning:  database.AppStatusRunning,
		database.AppStatusBuilding: database.AppStatusRunning,
		"":                         database.AppStatusRunning,
	} {
		if got := restoredStatus(before, "flask"); got != want {
			t.Errorf("restoredStatus(%q) = %q, want %q", before, got, want)
		}
	}
	if got := restoredStatus(database.AppStatusRunning, "batch-python"); got != database.AppStatusReady {
		t.Errorf("a batch app's live status = %q, want ready", got)
	}
}

// A shared start uses the detection the deploy recorded, not a fresh read of a
// tree the app has been writing into: a manifest-less Python app that wrote
// one .html beside app.py came back from sleep as a static site (code review
// 2026-11-11, E-6).
func TestASharedStartTrustsTheRecordedDetection(t *testing.T) {
	db := dbtest.New(t)
	appsDir := t.TempDir()
	spy := &specSpy{MockRuntime: runtime.NewMockRuntime(nil)}
	b := &Builder{db: db, rts: runtime.NewRuntimes(spy, nil), opts: Options{AppsDir: appsDir}}
	app := mkApp(t, db, "writer", string(runtime.ModeShared))
	app.CurrentRelease, app.Framework, app.Entrypoint = "writer-r1", string(FrameworkPython), "app.py"
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	release := CurrentLink(appsDir, app.ID)
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(release, "app.py"), []byte("print('hi')\n"), 0o644)
	os.WriteFile(filepath.Join(release, "report.html"), []byte("<h1>written at run time</h1>"), 0o644)

	_ = b.startOn(app, spy)
	if spy.got == nil {
		t.Fatal("the start never reached the backend")
	}
	if spy.got.Framework != string(FrameworkPython) || spy.got.Entrypoint != "app.py" {
		t.Errorf("started as %s %q, want python app.py", spy.got.Framework, spy.got.Entrypoint)
	}
}

// A start asked for a release that is no longer the app's — a deploy flipped
// between the request and the start lock — starts nothing, rather than the
// old workload beside the new one (code review 2026-11-11, F-5).
func TestAStartForASupersededReleaseStartsNothing(t *testing.T) {
	db := dbtest.New(t)
	appsDir := t.TempDir()
	spy := &specSpy{MockRuntime: runtime.NewMockRuntime(nil)}
	b := &Builder{db: db, rts: runtime.NewRuntimes(spy, nil), opts: Options{AppsDir: appsDir}}
	app := mkApp(t, db, "late", string(runtime.ModeShared))
	app.CurrentRelease = "late-r2"
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	snapshot := *app
	snapshot.CurrentRelease = "late-r1"
	if err := b.startOn(&snapshot, spy); !errors.Is(err, ErrStartSuperseded) {
		t.Fatalf("err = %v, want ErrStartSuperseded", err)
	}
	if spy.got != nil {
		t.Fatalf("the old release was started: %+v", spy.got)
	}
}

// A start whose guarded status write declined owes one more look at the row: a
// Stop that landed before the start placed its workload found nothing to stop,
// and the workload the start then placed must not be left live under the
// `stopped` row. A row that still says running is left alone.
func TestAStopDuringAStartIsStoppedAgain(t *testing.T) {
	db := dbtest.New(t)
	mock := runtime.NewMockRuntime(nil)
	b := &Builder{db: db, rts: runtime.NewRuntimes(mock, nil)}
	app := mkApp(t, db, "held", string(runtime.ModeShared))
	app.CurrentRelease = "held-r1"
	app.Status = database.AppStatusRunning
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAppStatus(app.ID, database.AppStatusRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := mock.Deploy(runtime.WorkloadSpec{AppID: app.ID, Slug: app.Slug, Name: "held-r1"}); err != nil {
		t.Fatal(err)
	}
	up := func() bool {
		m, err := mock.Metrics("held-r1")
		return err == nil && runtime.WorkloadIsUp(m.Status)
	}

	b.StopIfNoLongerWanted(app.ID)
	if !up() {
		t.Fatal("a running app's workload was stopped")
	}

	if err := db.UpdateAppStatus(app.ID, database.AppStatusStopped); err != nil {
		t.Fatal(err)
	}
	b.StopIfNoLongerWanted(app.ID)
	if up() {
		t.Error("the workload is still up under a stopped row")
	}
}

// Halt removes the current workload, and an absent one is a successful halt.
func TestHaltRemovesTheCurrentWorkload(t *testing.T) {
	db := dbtest.New(t)
	mock := runtime.NewMockRuntime(nil)
	b := &Builder{db: db, rts: runtime.NewRuntimes(mock, nil)}
	app := mkApp(t, db, "held", string(runtime.ModeShared))
	app.CurrentRelease = "held-r1"
	if _, err := mock.Deploy(runtime.WorkloadSpec{AppID: app.ID, Slug: app.Slug, Name: "held-r1"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Halt(app); err != nil {
		t.Fatal(err)
	}
	if _, err := mock.Metrics("held-r1"); err == nil {
		t.Error("the workload survived Halt")
	}
	if err := b.Halt(app); err != nil {
		t.Errorf("halting an absent workload = %v, want nil", err)
	}
}

// stopWhilePlacing writes `stopped` to the app row as its workload is being
// placed: a Stop that found nothing to stop yet, and was answered as a success.
type stopWhilePlacing struct {
	*runtime.MockRuntime
	db    *database.DB
	appID string
}

func (s stopWhilePlacing) Deploy(spec runtime.WorkloadSpec) (runtime.Endpoint, error) {
	s.db.UpdateAppStatus(s.appID, database.AppStatusStopped)
	return s.MockRuntime.Deploy(spec)
}

// A Stop that lands while a wake is starting the app stands, and the workload
// the wake placed is stopped again under it (security review 2026-11-14,
// DB-U1). The same helper closes the boot reconcile and the supervisor.
func TestAStopDuringAStartLeavesNoLiveWorkload(t *testing.T) {
	db := dbtest.New(t)
	appsDir := t.TempDir()
	mock := runtime.NewMockRuntime(ports.New(45500, 45600, func() ([]int, error) { return nil, nil }))
	app := mkApp(t, db, "dozing", string(runtime.ModeShared))
	rt := stopWhilePlacing{MockRuntime: mock, db: db, appID: app.ID}
	b := &Builder{db: db, rts: runtime.NewRuntimes(rt, nil), opts: Options{AppsDir: appsDir}}
	app.CurrentRelease = "dozing-r1"
	if err := db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAppStatus(app.ID, database.AppStatusSleeping); err != nil {
		t.Fatal(err)
	}
	release := CurrentLink(appsDir, app.ID)
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(release, "index.html"), []byte("<!doctype html><title>dozing</title>"), 0o644)

	app, _ = db.GetApp(app.ID)
	if err := b.Wake(app); err != nil {
		t.Fatalf("wake = %v", err)
	}
	fresh, _ := db.GetApp(app.ID)
	if fresh.Status != database.AppStatusStopped {
		t.Errorf("status = %q, want the Stop to stand", fresh.Status)
	}
	if m, err := mock.Metrics(app.ProcessName()); err == nil && runtime.WorkloadIsUp(m.Status) {
		t.Errorf("the wake's workload is %q under a stopped row", m.Status)
	}
}
