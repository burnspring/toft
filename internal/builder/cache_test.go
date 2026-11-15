package builder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSizeIsReadFromTheCachedMeasurement is D6 asserted rather than trusted.
//
// Every reader of the size is on a path that cannot afford a directory walk:
// the Checks report gives each check five seconds, and the metric export runs
// on the collector's interval. So the accessor must return what the measurer
// left behind and never go near the disk.
//
// The proof is to point the builder at a directory that does not exist and ask
// anyway. A reader that walked would fail or answer zero; one reading the stored
// figure answers the figure.
func TestSizeIsReadFromTheCachedMeasurement(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: filepath.Join(t.TempDir(), "does-not-exist")}}
	measured := time.Now().UTC().Add(-3 * time.Minute)
	b.cacheState = CacheState{Bytes: 4_294_967_296, MeasuredAt: measured}

	st := b.CacheStatus()
	if st.Bytes != 4_294_967_296 {
		t.Errorf("CacheStatus().Bytes = %d, want the stored measurement. The accessor is "+
			"doing I/O instead of reading what the measurer left behind, which puts a "+
			"directory walk on the Checks page's five-second budget.", st.Bytes)
	}
	if !st.MeasuredAt.Equal(measured) {
		t.Errorf("CacheStatus().MeasuredAt = %v, want %v", st.MeasuredAt, measured)
	}
}

// TestNeverMeasuredIsNotZero is the distinction every surface depends on.
//
// "Not measured yet" and "measured, and it is empty" are different facts, and a
// zero that cannot tell them apart is what makes the Checks page confidently
// wrong for the first fifteen minutes after every restart.
func TestNeverMeasuredIsNotZero(t *testing.T) {
	var fresh Builder
	if fresh.CacheStatus().Measured() {
		t.Error("a builder that has never measured reports Measured() == true")
	}

	empty := &Builder{}
	empty.cacheState = CacheState{Bytes: 0, MeasuredAt: time.Now().UTC()}
	if !empty.CacheStatus().Measured() {
		t.Error("a completed measurement of an empty cache reports Measured() == false, " +
			"so a genuinely empty cache is indistinguishable from an unmeasured one")
	}
}

// TestMeasurementSumsTheTreeAndSurvivesWhatItCannotRead covers the walk itself:
// the figure is the regular files underneath, a symlink cannot inflate it with
// somebody else's disk, and an unreadable subtree is skipped rather than fatal.
func TestMeasurementSumsTheTreeAndSurvivesWhatItCannotRead(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, n int) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("npm/_cacache/a", 1000)
	write("uv/archive-v0/b", 2000)
	write("go-mod/cache/download/c", 3000)

	// A symlink pointing outside the cache must contribute nothing: the size is
	// what this directory holds, not what it can reach.
	outside := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(outside, make([]byte, 9_000_000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "npm", "link")); err != nil {
		t.Fatal(err)
	}

	got, err := directorySize(context.Background(), root)
	if err != nil {
		t.Fatalf("measuring: %v", err)
	}
	if got != 6000 {
		t.Errorf("directorySize = %d, want 6000 (the three regular files, and the symlink "+
			"contributing nothing)", got)
	}
}

// TestAMissingCacheDirectoryIsReportedNotFatal — the root not existing is the
// one walk error worth surfacing, because it means the install is misconfigured
// rather than that one file went away mid-build.
func TestAMissingCacheDirectoryIsReportedNotFatal(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: filepath.Join(t.TempDir(), "gone")}}
	b.measureCacheOnce()

	st := b.CacheStatus()
	if st.Err == "" {
		t.Error("measuring a cache directory that does not exist recorded no error")
	}
	if st.Measured() {
		t.Error("a failed measurement was recorded as a measurement, so the surfaces would " +
			"show a confident zero for a directory that is not there")
	}
}

// TestAFailedMeasurementKeepsThePreviousFigure — a stale number with an honest
// timestamp beside it beats no number, so a failure updates Err and leaves the
// last good reading alone.
func TestAFailedMeasurementKeepsThePreviousFigure(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: filepath.Join(t.TempDir(), "gone")}}
	earlier := time.Now().UTC().Add(-time.Hour)
	b.cacheState = CacheState{Bytes: 123_456, MeasuredAt: earlier}

	b.measureCacheOnce()

	st := b.CacheStatus()
	if st.Bytes != 123_456 || !st.MeasuredAt.Equal(earlier) {
		t.Errorf("a failed measurement discarded the previous figure: got %d at %v, "+
			"want 123456 at %v", st.Bytes, st.MeasuredAt, earlier)
	}
	if st.Err == "" {
		t.Error("a failed measurement left no error behind, so the stale figure is " +
			"presented as current")
	}
}

// ---------------------------------------------------------------------------
// The reclaim.
// ---------------------------------------------------------------------------

// TestASecondReclaimIsRefused is the 409 condition, asserted at the Builder
// where it is decided rather than at the route that reports it.
func TestASecondReclaimIsRefused(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: t.TempDir()}}

	// Hold the cache's write side from outside so the reclaim cannot finish
	// before the second request arrives; otherwise this races.
	b.cacheMu.Lock()

	first, _ := b.StartCacheReclaim(nil)
	if !first {
		t.Fatal("the first reclaim was refused")
	}
	// Wait for the goroutine to have marked itself running.
	deadline := time.Now().Add(2 * time.Second)
	for !b.CacheStatus().Reclaiming && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	second, st := b.StartCacheReclaim(nil)
	if second {
		t.Error("a second reclaim was started while the first was running. Two sets of " +
			"prune verbs against one content-addressed store at once is the corruption " +
			"the write lock exists to prevent.")
	}
	if st.StartedAt.IsZero() {
		t.Error("the refusal carried no start time, so the 409 cannot say what is running " +
			"and since when")
	}
	b.cacheMu.Unlock()
}

// TestReclaimWaitsForAnInFlightBuild — the reclaim does not begin its verbs
// until the build holding the read side returns.
func TestReclaimWaitsForAnInFlightBuild(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: t.TempDir()}}

	// A build is in flight.
	b.cacheMu.RLock()

	done := make(chan ReclaimResult, 1)
	if started, _ := b.StartCacheReclaim(func(r ReclaimResult) { done <- r }); !started {
		t.Fatal("the reclaim was refused")
	}

	select {
	case <-done:
		t.Fatal("the reclaim finished while a build still held the cache's read side — it " +
			"deleted out from under an in-flight dependency install, which is a corrupt " +
			"release")
	case <-time.After(150 * time.Millisecond):
		// Correct: it is waiting.
	}
	if !b.CacheStatus().Reclaiming {
		t.Error("a waiting reclaim does not report itself as running, so the card would " +
			"show nothing happening while an administrator waits")
	}

	// The build finishes; the reclaim proceeds.
	b.cacheMu.RUnlock()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the reclaim never ran after the build released the cache")
	}
	if b.CacheStatus().Reclaiming {
		t.Error("the reclaim finished but still reports itself as running")
	}
}

// TestAMissingToolIsSkippedNotFailed — an install without a tool has no cache
// of that kind worth reclaiming, and calling that an error would make a healthy
// install look broken every time somebody pressed the button.
func TestAMissingToolIsSkippedNotFailed(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: t.TempDir()}}

	step := b.runReclaimVerb(reclaimVerb{
		tool:   "go-mod",
		name:   "definitely-not-a-real-binary-toft",
		args:   []string{"clean"},
		absent: "Go is not installed on this host.",
	}, nil)

	if step.Outcome != ReclaimSkipped {
		t.Errorf("a missing tool produced outcome %q, want %q", step.Outcome, ReclaimSkipped)
	}
	if step.Detail == "" {
		t.Error("a skipped tool said nothing about why, so the card cannot explain the gap")
	}
}

// TestTheRenvCacheIsRemovedBecauseNothingReferencesIt — renv has no verb that
// clears a whole cache, and every release holds a copy, so removing the
// directory is the clearing verb. Absent is skipped, never failed.
func TestTheRenvCacheIsRemovedBecauseNothingReferencesIt(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: t.TempDir()}}

	if step := b.reclaimRenv(); step.Outcome != ReclaimSkipped || step.Detail == "" {
		t.Errorf("no renv cache gave %+v, want skipped with a reason", step)
	}

	pkg := filepath.Join(b.renvCacheDir(), "v5", "linux", "R-4.5", "x86_64", "Rcpp", "1.1.1", "h", "Rcpp")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "DESCRIPTION"), []byte("Package: Rcpp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if step := b.reclaimRenv(); step.Outcome != ReclaimDone {
		t.Errorf("reclaiming a filled renv cache gave %+v", step)
	}
	if fileExists(b.renvCacheDir()) {
		t.Error("the renv cache survived its reclaim, so the card promises disk it did not free")
	}
}

// TestModcacheIsTheEscalation — the module cache is the most expensive of the
// four to lose, so it is left alone when the cheap verbs already gave back a
// meaningful share.
func TestModcacheIsTheEscalation(t *testing.T) {
	b := &Builder{}

	if b.modcacheIsWorthIt(1000, 400) {
		t.Error("the cheap verbs freed 60% and the module cache was emptied anyway — every " +
			"Go app on the install re-downloads its modules for disk that was already back")
	}
	if !b.modcacheIsWorthIt(1000, 950) {
		t.Error("the cheap verbs freed 5% and the module cache was left alone, so the " +
			"escalation never escalates and the button does not solve a full volume")
	}
	if b.modcacheIsWorthIt(0, 0) {
		t.Error("an empty cache escalated; there is nothing there to reclaim")
	}
}

// TestReclaimReportsWhatItFreedAndRemeasures — the figure on the card changes
// because of what the administrator just did, not fifteen minutes later.
func TestReclaimReportsWhatItFreedAndRemeasures(t *testing.T) {
	root := t.TempDir()
	b := &Builder{opts: Options{CacheDir: root}}

	done := make(chan ReclaimResult, 1)
	if started, _ := b.StartCacheReclaim(func(r ReclaimResult) { done <- r }); !started {
		t.Fatal("refused")
	}
	var res ReclaimResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the reclaim never completed")
	}

	if res.Freed < 0 {
		t.Errorf("Freed = %d, want never negative — a build finishing between two walks "+
			"must not be reported as the reclaim adding disk", res.Freed)
	}
	st := b.CacheStatus()
	if !st.Measured() {
		t.Error("a completed reclaim left the cache unmeasured, so the card still says " +
			"\"not measured yet\" immediately after the administrator emptied it")
	}
	if st.LastReclaim.IsZero() {
		t.Error("a completed reclaim recorded no completion time")
	}
	// Every tool on this box is either present or absent; either way no step
	// may be left with an empty outcome.
	for _, s := range res.Steps {
		if s.Outcome == "" {
			t.Errorf("step %q has no outcome", s.Tool)
		}
		if s.Outcome != ReclaimDone && s.Detail == "" {
			t.Errorf("step %q was %q with no explanation", s.Tool, s.Outcome)
		}
	}
}

// TestTheFourCacheDirectoriesAreNamedOnce — the reclaim's verbs and every build
// must point at the same four directories. Two lists would be one list and a
// bug: a build filling a directory the reclaim does not empty.
func TestTheFourCacheDirectoriesAreNamedOnce(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: "/var/lib/toft/cache"}}

	env := b.cacheEnv()
	want := map[string]string{
		"npm_config_cache": "/var/lib/toft/cache/npm",
		"UV_CACHE_DIR":     "/var/lib/toft/cache/uv",
		"GOCACHE":          "/var/lib/toft/cache/go-build",
		"GOMODCACHE":       "/var/lib/toft/cache/go-mod",
		"UV_LINK_MODE":     "copy",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("cacheEnv()[%q] = %q, want %q", k, env[k], v)
		}
	}

	// And the build environment must carry exactly those, so the two cannot
	// drift.
	buildEnv := b.buildEnvironment(nil, "/apps/x")
	for k, v := range want {
		if buildEnv[k] != v {
			t.Errorf("buildEnvironment()[%q] = %q, want %q — a build is using a cache "+
				"directory the reclaim does not empty", k, buildEnv[k], v)
		}
	}

	if b2 := (&Builder{}); b2.cacheEnv() != nil {
		t.Error("a builder with no cache directory still named cache paths")
	}
}

// TestAnEmptyDirectoryIsNotAProblem — the shared renv and deps directories are
// never made on an install whose apps build as accounts. A reclaim that emptied
// the accounts listed them on the card in the attention tone, beside the bytes
// it freed; the audit row keeps them, the card does not.
func TestAnEmptyDirectoryIsNotAProblem(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: t.TempDir()}}

	res := ReclaimResult{Steps: []ReclaimStep{
		b.reclaimRenv(), b.reclaimDeps(), b.reclaimAccounts(),
		{Tool: "npm", Outcome: ReclaimDone},
	}}
	for _, s := range res.Steps[:3] {
		if s.Outcome != ReclaimSkipped || s.Detail == "" {
			t.Errorf("an absent %s directory gave %+v, want skipped with a reason", s.Tool, s)
		}
	}
	if p := res.Problems(); p != "" {
		t.Errorf("empty directories were reported as problems: %q", p)
	}

	res.Steps = append(res.Steps,
		ReclaimStep{Tool: "uv", Outcome: ReclaimSkipped, Detail: "uv is not installed."},
		ReclaimStep{Tool: "go-mod", Outcome: ReclaimFailed, Detail: "permission denied"})
	if p, want := res.Problems(), "uv: uv is not installed. go-mod: permission denied"; p != want {
		t.Errorf("Problems() = %q, want %q", p, want)
	}
}
