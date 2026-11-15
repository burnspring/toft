package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
	"toft/internal/toolchain"
)

// System checks: the install's self-diagnosis, computed on demand.
//
// Nothing here is stored. A check history would be a second, weaker copy of
// facts the platform already keeps — `audit_log` for what people did,
// `app_events` for what broke, the metrics page for what the host is doing —
// and the question this page answers is always about *now*: an operator opens
// it because something is wrong, or before they change something. So the
// report is recomputed per request and the button re-runs it rather than
// filing another row.
//
// Every check is cheap: a database ping, a stat, readings the runtime already
// caches. Nothing here may deploy, restart, write, or block on a network call
// to a third party — a diagnostic that can itself break the install is not a
// diagnostic.
const (
	checkPass = "pass"
	checkWarn = "warn"
	checkFail = "fail"
	// checkSkip is a capability this install does not offer. Absent, not
	// broken — the same distinction the rest of the platform makes about
	// optional capabilities, and the reason it is not a warning: an install
	// with no artifact store configured is not a degraded install.
	checkSkip = "skip"
)

// The categories are a closed list. The category is the only grouping this
// page has, and with close to forty rows it stops being decoration — a
// free-text one drifts into three spellings of "Security" the first time two
// people add a check in the same week. Enforced by the registry test, and the
// reason a check no longer writes its own: it is declared once, beside the
// function, where a reviewer sees the whole set.
const (
	catPlatform      = "Platform"
	catStorage       = "Storage"
	catExecution     = "Execution"
	catApps          = "Apps"
	catSignIn        = "Sign-in"
	catNotifications = "Notifications"
	catSources       = "Sources"
	catSecurity      = "Security"
	catAutomation    = "Automation"
)

// checkCategories is the closed list itself, for the test that walks it.
var checkCategories = []string{
	catPlatform, catStorage, catExecution, catApps, catSignIn,
	catNotifications, catSources, catSecurity, catAutomation,
}

// systemCheck is one line of the report. Detail is the whole value of the
// page: "fail" without the reading that produced it sends an operator to the
// logs, which is where they already were.
//
// Href and Setting are where a *finding* is fixed, and at most one is ever set.
// Two fields rather than one string because the route and anchor of every
// setting are already a table in the SPA with a test in front of it (ADR §50):
// a backend writing "/admin/settings#security-txt" by hand would be a second,
// unguarded copy of that table, wrong the first time an anchor is renamed. The
// backend knows *which setting* is wrong and has no business knowing which tab
// it is currently on.
//
// Neither is set on a pass or a skip — stamped by the report, not by the check
// — so a fact line links nowhere, which is the quiet second signal of which
// rows are findings.
type systemCheck struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	Href     string `json:"href,omitempty"`
	Setting  string `json:"setting,omitempty"`
}

// checkDef is one entry in the list, and **the list is the specification**.
//
// The eleven checks used to be eleven method calls in the body of the handler.
// At eleven that is a style; at close to forty it is how a check silently stops
// running, because deleting a line in a merge looks exactly like a check that
// passed. So the list is data, a test walks it, and the same rule the
// transports registry and appint.Known already hold applies here.
//
// A def's id is the *check's* identity, not a row's: several of these emit more
// than one row and the rows keep their own ids. The def id is what a row is
// filed under when the check could not speak for itself — it timed out, it
// panicked, or the report ran out of budget before reaching it.
type checkDef struct {
	id       string
	name     string
	category string
	// href is an admin route and setting is a settings key; at most one, and
	// only on a check whose findings have a page that owns the fix. Many do
	// not — a full disk, an unreachable backend and a missing interpreter are
	// all fixed on the host — and those carry neither.
	href    string
	setting string
	run     func(*Server, context.Context, *checkReport)
}

// checks is the report, in the order it is produced.
var checks = []checkDef{
	{id: "database", name: "Database", category: catPlatform, run: (*Server).checkDatabase},
	{id: "storage", name: "Storage", category: catStorage, run: (*Server).checkStorage},
	{id: "host", name: "Host", category: catPlatform, run: (*Server).checkHost},
	{id: "execution", name: "Execution backends", category: catExecution, run: (*Server).checkExecution},
	// Fixed on the host or in the deployment's grants — no page here owns them.
	{id: "agent", name: "Shared-mode agent", category: catExecution, run: (*Server).checkAgent},
	{id: "toolchains", name: "Toolchains", category: catExecution, run: (*Server).checkToolchains},
	{id: "estate", name: "App health", category: catApps, run: (*Server).checkEstate},
	{id: "notifications", name: "Notifications", category: catNotifications, run: (*Server).checkNotifications},
	{id: "sign_in", name: "Sign-in", category: catSignIn, run: (*Server).checkSignIn},
	{id: "directory_groups", name: "Directory groups", category: catSignIn, run: (*Server).checkDirectoryGroups},
	{id: "login_group_gate", name: "Permitted login groups", category: catSignIn, run: (*Server).checkLoginGroupGate},
	{id: "scim", name: "Directory feed", category: catSignIn, run: (*Server).checkSCIM},

	{id: "git_connections", name: "Clone credentials", category: catSources,
		href: "/admin/sources", run: (*Server).checkGitConnections},
	{id: "security_txt", name: "security.txt", category: catSecurity,
		setting: "security_txt_expires", run: (*Server).checkSecurityTxt},
	{id: "advisory_corpus", name: "Advisory corpus", category: catPlatform,
		href: "/admin/dependencies", run: (*Server).checkCorpus},
	// Nor here — a clock is fixed on the host.
	{id: "clock", name: "Clock", category: catPlatform, run: (*Server).checkClock},

	// A fact, never a finding, so it has no destination by construction.
	{id: "encryption", name: "At-rest encryption", category: catSecurity,
		run: (*Server).checkEncryption},
	{id: "stuck_work", name: "Stuck work", category: catAutomation,
		href: "/admin/automation", run: (*Server).checkStuckWork},
	{id: "scheduled_jobs", name: "Scheduled jobs", category: catAutomation,
		href: "/admin/automation", run: (*Server).checkScheduledJobs},
	{id: "telemetry", name: "Telemetry", category: catPlatform,
		href: "/admin/telemetry", run: (*Server).checkTelemetry},
	// The pool is sized in configuration no page here can reach.
	{id: "db_pool", name: "Database pool", category: catPlatform, run: (*Server).checkPool},
	{id: "runner_images", name: "Runner images", category: catExecution,
		run: (*Server).checkRunnerImages},

	// Both fixed in the environment this process was started with, not on a
	// page — so neither carries a destination.
	{id: "dev_switches", name: "Development switches", category: catSecurity,
		run: (*Server).checkDevSwitches},
	{id: "trusted_proxy", name: "Trusted proxy", category: catSecurity,
		run: (*Server).checkTrustedProxy},
	{id: "hsts", name: "HTTPS only (HSTS)", category: catSecurity,
		run: (*Server).checkHSTS},
	// Fixed in the environment, like the two above.
	{id: "app_containment", name: "App containment", category: catSecurity,
		run: (*Server).checkAppContainment},
	// Nor this one: a migration that will not apply is a database role or a
	// second binary, and neither is something this product configures.
	{id: "migrations", name: "Schema migrations", category: catPlatform,
		run: (*Server).checkMigrations},
}

// The two bounds are one mechanism rather than two that can disagree: a check
// gets min(checkBudget, what is left of reportBudget).
//
// Twenty seconds is chosen against the failure it exists for — one hung
// backend, not all of them — and against this page's own promise that it is
// safe to press during an incident. Five seconds each is what makes running
// them serially tolerable.
// The thresholds, in one block, named after what they mean.
//
// Stated together so that an operator reading this file sees one policy rather
// than seven numbers scattered across five files. They are deliberately not
// settings, on usageStatus's reasoning: these are not a policy an install
// tunes, they are when a thing starts failing. An install that wants different
// numbers wants a monitoring system, and this page is not one.
const (
	// A token renewal that needs a human takes about a fortnight to schedule.
	credentialExpiryWarning = 14 * 24 * time.Hour
	// Long enough to reach whoever owns the mailbox in the contact line.
	securityTxtExpiryWarning = 30 * 24 * time.Hour
	// Against the feed's own cadence rather than a round number: a daily sync
	// that has missed a week has stopped, not slipped.
	corpusStaleAfter = 7 * 24 * time.Hour
	// Three weeks, which is two renewal attempts for a 90-day certificate.
	certificateExpiryWarning = 21 * 24 * time.Hour

	// Two seconds is past anything ntpd would leave alone; thirty is where
	// token expiry and cron claims start disagreeing with each other.
	clockSkewWarn = 2 * time.Second
	clockSkewFail = 30 * time.Second

	// Ten minutes is longer than the tightest schedule anyone writes here and
	// short enough that a stopped scheduler is noticed the same morning.
	scheduleOverdueAfter = 10 * time.Minute
	// Past ninety per cent the pool is queueing rather than serving, and the
	// symptom is latency with no error anywhere.
	poolSaturation = 0.90
	// A wait for a connection shorter than this, on average, is not one a
	// page shows (recentWaits).
	poolSlowWait = 50 * time.Millisecond
)

const checkBudget = 5 * time.Second

// A var, not a const, for one reason: the "the report ran out of time" branch
// is a real row an operator can see, and the only way to exercise it without
// shipping a check that sleeps for twenty seconds is to shorten the budget.
var reportBudget = 20 * time.Second

// checkReport collects the lines. A check writes into one of these; the handler
// merges it, which is what lets a check that overran be dropped without racing
// the goroutine still writing to it.
type checkReport struct {
	checks []systemCheck

	// req is the request that asked for the report, and exactly one check
	// reads it. Stated here as the exception it is: every other finding on this
	// page is derived from configuration or from something stored, and the
	// trusted-proxy one cannot be — it is about what an actual request carried.
	// Carried on the report rather than in the registry's signature so that all
	// checks stay one shape and the walk stays uniform.
	req *http.Request
}

// add takes no category: it comes from the def, so a check cannot invent one.
func (rp *checkReport) add(id, name, status, detail string) {
	rp.checks = append(rp.checks, systemCheck{
		ID: id, Name: name, Status: status, Detail: detail,
	})
}

// addLinked is `add` for a check whose *rows* differ in where they are fixed.
//
// The def's `href` is one destination for every row a check produces, which is
// right for almost all of them. App health is the exception the split below
// creates: crashed and failed are triaged differently and land on two
// different narrowings of the apps list, so the destination belongs to the row
// rather than to the check. Applied only to a finding, like the def's — a pass
// that linked somewhere would cost the missing link its meaning.
func (rp *checkReport) addLinked(id, name, status, detail, href string) {
	rp.checks = append(rp.checks, systemCheck{
		ID: id, Name: name, Status: status, Detail: detail, Href: href,
	})
}

// runCheck runs one check under its own deadline and contains what it does.
//
// The goroutine is not decoration. checkExecution reaches a pod list whose own
// timeout is thirty seconds and which takes no context, so a context deadline
// alone would bound nothing: an install whose API server is unreachable — the
// exact incident somebody opens this page during — served the report thirty
// seconds late with nothing on it saying which row cost that. The check is
// therefore run beside us and abandoned if it overruns, writing into a report
// of its own so that abandoning it cannot race the merge.
//
// A check that overran and one the budget never reached are both `warn` and
// never `fail`: "the platform could not ask" is a different finding from "the
// answer was bad", and an operator has to be able to tell them apart on the
// row.
func (s *Server) runCheck(ctx context.Context, req *http.Request, def checkDef) []systemCheck {
	done := make(chan []systemCheck, 1) // buffered: an abandoned check must not block on send
	go func() {
		local := &checkReport{req: req}
		defer func() {
			if v := recover(); v != nil {
				// The value is logged, never rendered: a panic message is not a
				// string this page has vetted for what it might carry, and the
				// row a reader needs says which check and that it was contained.
				slog.Error("system check panicked", "check", def.id, "panic", fmt.Sprint(v))
				done <- []systemCheck{{
					ID: def.id, Name: def.name, Status: checkFail,
					Detail: "The check itself failed and was contained. Every other row still ran; see the platform log for the cause.",
				}}
				return
			}
			done <- local.checks
		}()
		def.run(s, ctx, local)
	}()

	select {
	case rows := <-done:
		return rows
	case <-ctx.Done():
		return []systemCheck{{
			ID: def.id, Name: def.name, Status: checkWarn,
			Detail: fmt.Sprintf("Did not answer in %s, so the report moved on. This is the platform failing to ask, not an answer.", checkBudget),
		}}
	}
}

// runChecks walks the list and produces the report. Separate from the handler
// because the handler's own job is three lines of counting and a JSON write,
// and because the tests that matter here are about the walk: that every check
// runs, that one that hangs is abandoned rather than allowed to hold the page,
// and that nothing an install has not configured comes back as a warning.
func (s *Server) runChecks(r *http.Request) []systemCheck {
	ctx := r.Context()
	deadline := time.Now().Add(reportBudget)
	var out []systemCheck

	// Serially, in list order. Concurrency was the obvious answer and is
	// rejected: two dozen goroutines against one connection pool is a
	// diagnostic that can cause the saturation it reports, and the wall-clock
	// it would save only matters on an install where something is already
	// hanging — which is what the per-check deadline is for.
	for _, def := range checks {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			out = append(out, systemCheck{
				ID: def.id, Name: def.name, Category: def.category, Status: checkWarn,
				Detail: "Not run — the report ran out of time before reaching this check.",
			})
			continue
		}
		budget := checkBudget
		if remaining < budget {
			budget = remaining
		}
		cctx, cancel := context.WithTimeout(ctx, budget)
		rows := s.runCheck(cctx, r, def)
		cancel()

		for _, row := range rows {
			row.Category = def.category
			// A destination on findings only. A pass that linked somewhere
			// would cost the missing link its meaning.
			if row.Status == checkWarn || row.Status == checkFail {
				// A row that named its own destination keeps it: see addLinked.
				if row.Href == "" {
					row.Href = def.href
				}
				row.Setting = def.setting
			} else {
				// And the rule stays the report's, not each check's: a fact
				// links nowhere, whatever a check put on the row.
				row.Href, row.Setting = "", ""
			}
			out = append(out, row)
		}
	}
	return out
}

// handleSystemChecks — GET /system/checks. system_admin only: every line of it
// is infrastructure detail, on the same rule as the `server` block of
// /system/health and the whole of /system/processes.
func (s *Server) handleSystemChecks(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}

	started := time.Now()
	rows := s.runChecks(r)

	var passed, warned, failed, skipped int
	for _, c := range rows {
		switch c.Status {
		case checkPass:
			passed++
		case checkWarn:
			warned++
		case checkFail:
			failed++
		case checkSkip:
			skipped++
		}
	}
	status := checkPass
	switch {
	case failed > 0:
		status = checkFail
	case warned > 0:
		status = checkWarn
	}

	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"ran_at":      time.Now().UTC(),
		"hostname":    host,
		"duration_ms": time.Since(started).Milliseconds(),
		"status":      status,
		"passed":      passed,
		"warned":      warned,
		"failed":      failed,
		"skipped":     skipped,
		"checks":      rows,
		// The build cache's size and the state of any reclaim. It rides on the
		// report because the card lives on this page and the page already
		// refetches; nil on an install with no cache directory, which is what
		// keeps the card off a surface that has nothing to put in it.
		"build_cache": s.buildCacheBlock(),
		// Every filesystem the install writes to and how full it is — statfs
		// once per folder, on this request only (disks.go).
		"disks": s.disksBlock(),
	})
}

// dbReadFailed is the detail of a check whose database read failed: a fixed
// sentence, with the driver's error in the platform log instead. pgx's connect
// error carries the DSN's user, database and host:port, and no detail carries
// a DSN (docs/invariants.md § Optional capabilities) — the report is readable
// with a personal API key.
func dbReadFailed(id, what string, err error) string {
	slog.Warn("system check could not read the database", "check", id, "err", err)
	return "Could not read " + what + " from the database. The error is in the platform log."
}

func (s *Server) checkDatabase(ctx context.Context, rp *checkReport) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	started := time.Now()
	if err := s.db.Conn().SQL().PingContext(ctx); err != nil {
		slog.Warn("system check: the database did not answer", "check", "database", "err", err)
		rp.add("database", "Database", checkFail,
			"The database did not answer the ping within 3s. The error is in the platform log.")
		return
	}
	rp.add("database", "Database", checkPass,
		fmt.Sprintf("Answered in %dms.", time.Since(started).Milliseconds()))
}

// checkStorage stats and writes into each configured directory. Writability is
// the failure that matters: a read-only APPS_DIR fails every build with an
// error a long way from its cause.
func (s *Server) checkStorage(_ context.Context, rp *checkReport) {
	for _, d := range []struct{ id, name, path string }{
		{"apps_dir", "App directory", s.cfg.AppsDir},
		{"logs_dir", "Log directory", s.cfg.LogsDir},
		{"cache_dir", "Build cache directory", s.cfg.CacheDir},
	} {
		if why := probeDirectory(d.path); why != "" {
			rp.add(d.id, d.name, checkFail, d.path+": "+why+".")
			continue
		}
		rp.add(d.id, d.name, checkPass, d.path+" exists and is writable."+s.cacheSizeNote(d.id))
	}

	s.checkReleaseDisk(rp)
	s.checkStorageRoots(rp)
	s.checkHostDataPlane(rp)

	switch {
	case s.artifacts != nil:
		rp.add("artifact_store", "Artifact store", checkPass,
			"Configured. Releases are published to it and isolated workloads can fetch them.")
	case s.rts.IsolatedConfigured():
		// The one combination that is broken rather than merely absent: an
		// isolated workload has no other way to reach its own code.
		rp.add("artifact_store", "Artifact store", checkFail,
			"Not configured, but this install offers isolated execution — an isolated deploy has no way to fetch its release.")
	default:
		rp.add("artifact_store", "Artifact store", checkSkip,
			"Not configured. Shared-mode releases are served from disk, so nothing needs it.")
	}
}

// checkReleaseDisk reads the free space of each filesystem the platform writes
// release trees, job runs, caches and logs to — once per filesystem, naming the
// directories on it — against the two watermarks (builder/disk.go). Soft is a
// warning: the platform is reclaiming what the store holds. Hard is a failure,
// because builds and fetches are being refused.
func (s *Server) checkReleaseDisk(rp *checkReport) {
	if s.builder == nil {
		return
	}
	type fs struct {
		usage builder.DiskUsage
		paths []string
	}
	var order []uint64
	byDev := map[uint64]*fs{}
	for _, p := range []string{s.cfg.AppsDir, s.cfg.JobRunDir, s.cfg.CacheDir, s.cfg.LogsDir} {
		if p == "" {
			continue
		}
		u, err := s.builder.MeasureDisk(p)
		if err != nil {
			continue
		}
		if f, ok := byDev[u.Device]; ok {
			f.paths = append(f.paths, p)
			continue
		}
		byDev[u.Device] = &fs{usage: u, paths: []string{p}}
		order = append(order, u.Device)
	}
	if len(order) == 0 {
		return
	}
	worst := builder.DiskOK
	var parts []string
	for _, dev := range order {
		f := byDev[dev]
		pct := 0.0
		if f.usage.Total > 0 {
			pct = float64(f.usage.Free) * 100 / float64(f.usage.Total)
		}
		parts = append(parts, fmt.Sprintf("%s free of %s (%.0f%%) on %s",
			humanBytes(f.usage.Free), humanBytes(f.usage.Total), pct, strings.Join(f.paths, ", ")))
		if f.usage.Level > worst {
			worst = f.usage.Level
		}
	}
	detail := strings.Join(parts, "; ") + fmt.Sprintf(". Cleanup starts below %d%% free; new releases stop below %d%%.",
		builder.DiskSoftFree, builder.DiskHardFree)
	if sw := s.builder.LastOrphanSweep(); !sw.At.IsZero() {
		detail += fmt.Sprintf(" Last removed %d directories no app owns (%s), %s.",
			sw.Removed, humanBytes(uint64(sw.Bytes)), sw.At.UTC().Format("2006-01-02 15:04 UTC"))
	}
	switch worst {
	case builder.DiskHard:
		rp.add("release_disk", "Release disk", checkFail,
			detail+" Builds, rollbacks and downloads that need the disk are being refused — free or grow it.")
	case builder.DiskSoft:
		rp.add("release_disk", "Release disk", checkWarn,
			detail+" Reclaiming what the artifact store holds; grow the disk if this persists.")
	default:
		rp.add("release_disk", "Release disk", checkPass, detail)
	}
}

// cacheSizeNote is the build cache's size, appended to that one directory's
// detail and empty for the other two.
//
// It is a sentence on an existing check rather than a check of its own, because
// a large cache is not a fault: nothing is failing, nothing is degraded, and an
// install that has never emptied it is exactly where every install starts. What
// an operator needs is the number, beside the directory it belongs to, at the
// moment they are already looking at storage.
//
// It reads the builder's last measurement and never walks anything. That is not
// a preference — this function runs inside a five-second check deadline, and a
// recursive walk of a multi-gigabyte content-addressed store would blow it and
// be rendered as a check that did not answer, which reads as a broken cache
// rather than a big one.
func (s *Server) cacheSizeNote(id string) string {
	if id != "cache_dir" || s.builder == nil {
		return ""
	}
	return cacheSizeDetail(s.builder.CacheStatus())
}

// cacheSizeDetail is the sentence itself, split out from the lookup so the copy
// can be tested against every state the measurement has — never measured,
// measured, measured-then-failed — without standing up a builder to hold them.
func cacheSizeDetail(st builder.CacheState) string {
	if !st.Measured() {
		// Never a zero. An install in the first minutes after a restart has an
		// unmeasured cache, not an empty one, and saying so is what stops
		// somebody concluding the cache is fine and looking elsewhere.
		return " Size not measured yet."
	}
	note := fmt.Sprintf(" Holding %s, measured %s.",
		humanBytes(uint64(st.Bytes)), humanAgo(st.MeasuredAt))
	if st.Err != "" {
		// The figure stands, with the reason the newest attempt did not land.
		note += " The last measurement did not complete: " + st.Err
	}
	return note
}

// humanAgo says how stale a reading is, in the plain register the checks page
// writes in. The age is the point: the figure is taken on a fifteen-minute
// ticker, so a number with no timestamp beside it invites somebody to empty the
// cache and conclude it did nothing.
func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "an hour ago"
	default:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
}

// checkStorageRoots asks, of every host root a resource lives under, whether
// the operator's mount is really there.
//
// This is the check that catches an operator's mount not coming back after a
// reboot, which is what shared mode's whole promise rests on: the operator
// mounts, Toft mounts nothing. A resource whose root has gone is not a
// degraded feature, it is an app writing into an empty directory on the host.
//
// **The root is resolved by name to the path it was declared at.** A resource
// records which root it is under (`storage_root` is `efs`, not `/mnt/storage`),
// and this used to stat that name as a path — relative to the working
// directory, where nothing called `efs` exists — so it failed on every host
// install that had storage. Then four questions per root, and one per resource:
//
//   - declared at all: a root dropped from SHARED_STORAGE_ROOTS strands every
//     resource under it, and that is a `fail`;
//   - a directory, and writable unless declared `:ro` — the data directories'
//     own probe;
//   - **a mount point**: on the same device as its parent, nothing is mounted
//     there, and an empty directory on the host's own disk passes every other
//     test while apps write into it. A `warn` rather than a `fail`, because a
//     plain directory is a legitimate root on a single-disk host and the check
//     cannot tell that from a mount that did not come back;
//   - each resource's own directory, `<root>/<name>`, still there.
//
// `skip` when no resource names a root — a cluster install binds through the
// isolated backend and has no host directory to probe.
func (s *Server) checkStorageRoots(rp *checkReport) {
	resources, err := s.db.ListStorageResources()
	if err != nil {
		rp.add("storage_roots", "Storage roots", checkWarn, dbReadFailed("storage_roots", "the storage resources", err))
		return
	}

	under := map[string][]string{}
	for _, r := range resources {
		if r.Root != "" {
			under[r.Root] = append(under[r.Root], r.Slug)
		}
	}
	if len(under) == 0 {
		rp.add("storage_roots", "Storage roots", checkSkip,
			"No storage resource is backed by a directory on this host.")
		return
	}
	declared := map[string]runtime.StorageRoot{}
	for _, root := range s.storageRoots() {
		declared[root.Name] = root
	}

	names := make([]string, 0, len(under))
	for name := range under {
		names = append(names, name)
	}
	sort.Strings(names)

	var broken, unmounted []string
	checked := 0
	stranded := false
	for _, name := range names {
		slugs := under[name]
		checked += len(slugs)
		root, ok := declared[name]
		if !ok {
			stranded = true
			broken = append(broken, fmt.Sprintf("%s: no longer in SHARED_STORAGE_ROOTS, so %s under it cannot be reached",
				name, countOf(len(slugs), "resource")))
			continue
		}
		if why := probeRoot(root); why != "" {
			broken = append(broken, name+" ("+root.Path+"): "+why)
			continue
		}
		if !isMountPoint(root.Path) {
			unmounted = append(unmounted, name+" ("+root.Path+")")
		}
		for _, slug := range slugs {
			if info, err := os.Stat(filepath.Join(root.Path, slug)); err != nil || !info.IsDir() {
				broken = append(broken, slug+" ("+filepath.Join(root.Path, slug)+"): its directory is gone")
			}
		}
	}

	switch {
	case len(broken) > 0 && stranded:
		// Linked, because a stranded root has a fix on a page: the Drives
		// section's re-point. A missing directory or a lost mount does not.
		rp.addLinked("storage_roots", "Storage roots", checkFail,
			"Apps are mounted on directories that are gone or read-only: "+list(broken)+
				". Re-point the stranded drive's resources on Admin → Storage.", "/admin/storage#drives")
	case len(broken) > 0:
		rp.add("storage_roots", "Storage roots", checkFail,
			"Apps are mounted on directories that are gone or read-only: "+list(broken)+".")
	case len(unmounted) > 0:
		rp.add("storage_roots", "Storage roots", checkWarn,
			"Nothing is mounted at "+list(unmounted)+", so it is a directory on the host's own disk. "+
				"If a file system belongs there, it did not come back and apps are writing to the wrong place.")
	default:
		rp.add("storage_roots", "Storage roots", checkPass,
			fmt.Sprintf("%s under %s, all mounted and present.",
				countOf(checked, "host-backed resource"), countOf(len(names), "root")))
	}
}

// isMountPoint reports whether something is mounted at path: a different
// device from its parent's, or the file system root. A path the platform cannot
// read the device of answers true, so an unknown is never a finding.
var isMountPoint = func(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	parent, err := os.Stat(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return true
	}
	a, okA := info.Sys().(*syscall.Stat_t)
	b, okB := parent.Sys().(*syscall.Stat_t)
	if !okA || !okB {
		return true
	}
	return a.Dev != b.Dev || a.Ino == b.Ino
}

// probeRoot is probeDirectory for a declared root: a `:ro` root is meant to
// refuse a write, so it is held to existing and being a directory.
func probeRoot(root runtime.StorageRoot) string {
	if root.ReadOnly {
		info, err := os.Stat(root.Path)
		switch {
		case err != nil:
			return err.Error()
		case !info.IsDir():
			return "not a directory"
		}
		return ""
	}
	return probeDirectory(root.Path)
}

// checkHostDataPlane says whether people can reach a host-root resource with
// `toft store` and the Files page, and why not when they cannot — the one
// condition of it an administrator cannot see from the storage pages.
func (s *Server) checkHostDataPlane(rp *checkReport) {
	switch {
	case s.hostStore != nil:
		rp.add("host_data_plane", "Host storage access", checkPass,
			"People granted a host-backed resource reach it through toft store and the Files page.")
	case len(s.storageRoots()) == 0:
		rp.add("host_data_plane", "Host storage access", checkSkip, "No storage root is declared on this host.")
	default:
		rp.add("host_data_plane", "Host storage access", checkWarn,
			"Host-backed resources take no grants here: "+s.hostStoreAbsent+".")
	}
}

// probeDirectory is the stat-and-write the three data directories already got,
// as one function, so a storage root is held to exactly the same standard.
// Returns why it failed, or "".
func probeDirectory(path string) string {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return err.Error()
	case !info.IsDir():
		return "not a directory"
	}
	probe := filepath.Join(path, ".toft-check")
	if err := os.WriteFile(probe, []byte("check"), 0o600); err != nil {
		return "not writable: " + err.Error()
	}
	os.Remove(probe)
	return ""
}

// checkHost reads the shared backend's view of the machine. An install whose
// workloads all run elsewhere has no such machine to report on, and says so
// rather than reporting an idle one.
func (s *Server) checkHost(_ context.Context, rp *checkReport) {
	m, err := s.rts.SystemMetrics()
	switch {
	case errors.Is(err, runtime.ErrNotSupported), m == nil && err == nil:
		rp.add("disk_space", "Disk space", checkSkip,
			"This install's execution backend has no host to report on.")
		rp.add("memory", "Memory", checkSkip,
			"This install's execution backend has no host to report on.")
		return
	case err != nil:
		rp.add("disk_space", "Disk space", checkWarn, "Could not read: "+err.Error())
		rp.add("memory", "Memory", checkWarn, "Could not read: "+err.Error())
		return
	}

	rp.add("disk_space", "Disk space", usageStatus(m.DiskPercent),
		fmt.Sprintf("%.0f%% used — %s of %s.", m.DiskPercent,
			humanBytes(m.DiskUsed), humanBytes(m.DiskTotal)))
	rp.add("memory", "Memory", usageStatus(m.MemoryPercent),
		fmt.Sprintf("%.0f%% used — %s of %s.", m.MemoryPercent,
			humanBytes(m.MemoryUsed), humanBytes(m.MemoryTotal)))
}

// usageStatus turns a percentage full into a verdict. The thresholds are here
// and not in settings on purpose: they are not a policy an install tunes, they
// are when a build starts failing.
func usageStatus(pct float64) string {
	switch {
	case pct >= 95:
		return checkFail
	case pct >= 85:
		return checkWarn
	default:
		return checkPass
	}
}

// list renders the names behind a count. Never truncated: an operator's next
// move is to go and look at the ones it names, and a detail that stopped at
// three would hide the fourth.
func list(items []string) string { return strings.Join(items, "; ") }

// sentence joins several findings into one detail. Two findings about one
// subject stay one row, because the next move is the same for both.
func sentence(parts []string) string {
	joined := strings.Join(parts, "; ")
	if joined == "" {
		return ""
	}
	return strings.ToUpper(joined[:1]) + joined[1:] + "."
}

// humanBytes is the builder's formatter, so a size on this page and the same
// size in a build log are written alike — and match the SPA's formatBytes.
func humanBytes(b uint64) string {
	if b > math.MaxInt64 { // a filesystem's total near 2^64 must not wrap negative
		b = math.MaxInt64
	}
	return builder.FormatBytes(int64(b))
}

// checkExecution asks each configured backend whether it is answering. A
// backend that is briefly unreachable is the failure this page exists to
// surface: every app on it is unmanageable until it comes back.
func (s *Server) checkExecution(_ context.Context, rp *checkReport) {
	if shared := s.rts.Shared(); shared != nil {
		if workloads, err := shared.List(); err != nil {
			rp.add("shared_backend", "Shared execution backend", checkFail, err.Error())
		} else {
			rp.add("shared_backend", "Shared execution backend", checkPass,
				fmt.Sprintf("Answering — %s known.", countOf(len(workloads), "workload")))
		}
	} else {
		rp.add("shared_backend", "Shared execution backend", checkSkip,
			"Not configured on this install.")
	}

	if !s.rts.IsolatedConfigured() {
		rp.add("isolated_backend", "Isolated execution backend", checkSkip,
			"Not configured. Apps and background jobs run on this host, under the shared-mode agent.")
		return
	}
	if workloads, err := s.rts.Isolated().List(); err != nil {
		rp.add("isolated_backend", "Isolated execution backend", checkFail, err.Error())
	} else {
		rp.add("isolated_backend", "Isolated execution backend", checkPass,
			fmt.Sprintf("Answering — %s known.", countOf(len(workloads), "workload")))
	}
}

// checkRunnerImages asks whether an isolated install can actually deploy
// everything it says it can.
//
// The startup check requires only that *at least one* RUNNER_IMAGE_* is set, so
// an install can be one family short and find out at the first deploy of that
// language. That is the real gap here; the runner *contract* version is a
// different thing entirely and is already enforced per callback.
//
// A moving tag is the second finding. "Never latest" is already this platform's
// rule for its own image and for the same reason: a runner on a tag that moves
// means the interpreter set a pod gets is whatever the registry last accepted,
// which is not what any deploy resolved against.
//
// The declared-versions half of this question is answered by the toolchain rows
// and is not repeated.
func (s *Server) checkRunnerImages(_ context.Context, rp *checkReport) {
	if !s.rts.IsolatedConfigured() {
		rp.add("runner_images", "Runner images", checkSkip,
			"No isolated backend, so nothing here runs from an image.")
		return
	}

	// The languages, plus static — a family with an image and no language, so
	// an install missing it cannot deploy a folder of HTML.
	families := append(append([]string{}, toolchain.Languages...), "static")

	var missing, moving []string
	for _, family := range families {
		ref := strings.TrimSpace(s.cfg.RunnerImages[family])
		if ref == "" {
			missing = append(missing, family)
			continue
		}
		if tag := imageTag(ref); tag == "" || tag == "latest" {
			moving = append(moving, family+" ("+ref+")")
		}
	}

	var findings []string
	if len(missing) > 0 {
		findings = append(findings, "no runner image for "+list(missing)+
			", so an isolated deploy of one finds out at the deploy")
	}
	if len(moving) > 0 {
		findings = append(findings, "on a moving tag: "+list(moving))
	}
	if len(findings) > 0 {
		rp.add("runner_images", "Runner images", checkWarn, sentence(findings))
		return
	}
	rp.add("runner_images", "Runner images", checkPass,
		fmt.Sprintf("%d families, each on a fixed tag.", len(families)))
}

// imageTag is the tag on an image reference, or "" when it carries none. The
// colon inside a registry host's port is not a tag, which is why this is not a
// LastIndex on the whole reference.
func imageTag(ref string) string {
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		return ref[at+1:] // a digest is as pinned as a reference gets
	}
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || colon < strings.LastIndex(ref, "/") {
		return ""
	}
	return ref[colon+1:]
}

// checkToolchains reports the binaries shared-mode builds and processes exec.
// Absent is a warning rather than a failure: a Python-only install needs no
// Node, and only the deploy that wanted it can say the toolchain was required.
//
// For a language the detail is the whole set rather than the default alone, and
// it names the runner image's declared version beside it. That pair
// is the one thing that makes a platform-image / runner-image divergence
// visible: the two are held in step today by a comment in each Dockerfile, and
// the last time they drifted the same app got Python 3.11 or 3.12 depending on
// which execution mode it landed in.
//
// **Every reading is dated, and an absent one is retaken here.** getServerInfo
// is a sync.Once and the probes behind it are a second one, so what this page
// rendered was a fact about start-up: an operator read "Node — Not installed",
// installed Node, pressed Run checks, and got the same amber row until the
// platform was restarted. The button's own copy — computed when asked for, and
// the button asks again — was false for exactly the rows somebody was most
// likely to be acting on.
//
// So a row reporting absence is probed again on this path, and a row reporting
// presence carries the age of its reading instead. The asymmetry is deliberate
// and is not laziness: the cached set is what every build resolves against, and
// a page free to disagree with it in the *present* direction would be a worse
// lie than the one this fixes. A toolchain removed since boot therefore still
// reads present, and the detail says when that was read.
// toolchainCheckID is a toolchain row's id: lower_snake like every other, and
// keyed on the language rather than its display label where there is one. It
// was "toolchain_" + the label — `toolchain_Node.js` — so the id broke the
// registry's own rule and would have moved if a label were ever reworded
// (docs/bugs 2026-10-22-e2e-cli #2).
func toolchainCheckID(label string, langByLabel map[string]string) string {
	if lang, ok := langByLabel[label]; ok {
		return "toolchain_" + lang
	}
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "toolchain_" + strings.Trim(b.String(), "_")
}

func (s *Server) checkToolchains(_ context.Context, rp *checkReport) {
	shared := s.rts.Shared() != nil
	langByLabel := map[string]string{}
	for _, lang := range toolchain.Languages {
		langByLabel[toolchain.Label(lang)] = lang
	}

	for _, rv := range getServerInfo().Runtimes {
		id := toolchainCheckID(rv.Name, langByLabel)

		if lang, isLanguage := langByLabel[rv.Name]; isLanguage {
			// The *set*, never rv.Version. rv.Version is the default alone, and
			// it is empty on an install that declares every interpreter path
			// explicitly and has none on PATH — which read here as "Not
			// installed" while three interpreters sat in the same page's
			// toolchain block.
			cached := toolchain.Available(lang)
			set := toolchain.AvailableNow(lang)
			retaken := len(cached) == 0
			switch {
			case len(set) > 0:
				rp.add(id, rv.Name, checkPass,
					toolchainDetail(lang, set)+toolchainReading(retaken))
			case !shared:
				rp.add(id, rv.Name, checkSkip,
					"Not installed. Nothing on this install builds or runs on this host."+
						toolchainReading(retaken))
			default:
				rp.add(id, rv.Name, checkWarn,
					"Not installed — apps that need it cannot be built or started here."+
						toolchainReading(retaken))
			}
			continue
		}

		// The platform's own tooling: not a language, no set, and nothing
		// resolves against it. Absent is retaken for the same reason.
		version, retaken := rv.Version, false
		if version == "" {
			if rv.Name == "Quarto" {
				_, version = toolchain.QuartoNow()
			} else {
				version = retakeAbsentRuntime(rv.Name)
			}
			retaken = true
		}
		switch {
		case version != "":
			rp.add(id, rv.Name, checkPass, version+"."+toolchainReading(retaken))
		case rv.Name == "Quarto":
			// Absent, not dormant, and not a warning: an install with no Quarto
			// is a complete configuration in which one engine is refused by
			// name and everything else is untouched. Saying "not installed" in
			// the same tone as a missing Node would read as something broken.
			rp.add(id, rv.Name, checkSkip,
				"Not installed (it is not bundled), so .qmd files and Quarto notebooks are refused. "+
					"Install it and set "+toolchain.QuartoBinEnv+", or run the image tag that carries it."+
					toolchainReading(true))
		case !shared:
			rp.add(id, rv.Name, checkSkip,
				"Not installed. Nothing on this install builds or runs on this host."+
					toolchainReading(true))
		default:
			rp.add(id, rv.Name, checkWarn,
				"Not installed — apps that need it cannot be built or started here."+
					toolchainReading(true))
		}
	}
}

// toolchainReading dates the reading a row was built from.
//
// A present toolchain is the start-up probe, held for the life of the process
// on purpose because it is what every deploy resolves against. So that row is
// answering about *then*, and a toolchain removed since would still read
// present. Saying which reading it is costs a clause, and it is the whole
// difference between a cache and a lie.
func toolchainReading(retaken bool) string {
	if retaken {
		return " Looked up just now."
	}
	return fmt.Sprintf(" Read at startup, %s ago.",
		humanDuration(time.Since(serverStarted)))
}

// humanDuration is an age for a person, to the unit that carries the meaning.
// Nobody reading "installed 3 minutes ago" needs the seconds.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return countOf(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return countOf(int(d.Hours()), "hour")
	default:
		return countOf(int(d.Hours()/24), "day")
	}
}

// countOf pluralizes properly rather than with "(s)", and **every counted noun
// on this page goes through it**.
//
// It was written for durations only, on the argument that a count reads as a
// reading rather than as prose. That argument does not survive contact with the
// page: eleven strings said "3 connection(s)" and "1 provider(s)" on the
// operator's most-read screen, in a product that pluralizes properly
// everywhere else — the fingerprint of server-authored copy that never met the
// same eye the SPA's did.
// thousands is a figure a person reads at a glance rather than counts digits
// in. `261380 advisories` on a diagnostic page is a number nobody parses; the
// SPA has done this everywhere since it had numbers (`toLocaleString`), and
// this page is the one place a raw six-figure integer still reached a reader.
func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return thousands(n) + " " + noun + "s"
}

// toolchainDetail is one language's set as a sentence: what this host has, the
// default marked, and the runner image's declaration beside it.
//
// The set is a parameter rather than a second read, so that one caller decides
// whether the row is a cached reading or a retaken one and the sentence cannot
// disagree with the verdict standing next to it.
func toolchainDetail(lang string, versions []toolchain.Version) string {
	var parts []string
	for _, v := range versions {
		if v.Default {
			parts = append(parts, v.Version+" (default)")
			continue
		}
		parts = append(parts, v.Version)
	}
	detail := strings.Join(parts, ", ")

	if runner := toolchain.Configured(lang); len(runner) > 0 {
		// Not an assertion that they match — the platform builds and the runner
		// runs, and only a workload can settle whether the image is what it was
		// declared to be. Printing both is what lets somebody notice.
		//
		// All of them, because the declaration is a list (D19): a check that
		// printed one version of three would make a partly-wrong declaration
		// look right on the one page an administrator goes to for this.
		declared := make([]string, 0, len(runner))
		for _, v := range runner {
			declared = append(declared, v.Version)
		}
		detail += fmt.Sprintf(". Isolated runner image declared: %s", strings.Join(declared, ", "))
	}
	if lang == toolchain.Go {
		if posture := toolchain.GoToolchainPosture(); posture != "" {
			detail += fmt.Sprintf(". GOTOOLCHAIN=%s", posture)
		} else {
			detail += ". GOTOOLCHAIN unset (auto): a newer toolchain a go.mod asks for is downloaded"
		}
	}
	return detail + "."
}

// checkEstate is the only check about the apps rather than the machine: what
// the platform believes is broken right now.
func (s *Server) checkEstate(_ context.Context, rp *checkReport) {
	apps, err := s.db.ListAllApps()
	if err != nil {
		rp.add("app_health", "App health", checkWarn, dbReadFailed("app_health", "the apps", err))
		return
	}
	var crashed, failed, locked, withheld int
	for _, a := range apps {
		if a.Status == database.AppStatusCrashed {
			crashed++
		}
		if a.Status == database.AppStatusFailed {
			failed++
		}
		if a.LockedAt != nil {
			locked++
		}
		if a.PublicWithheld {
			withheld++
		}
	}

	// The two words the platform has gained since this check was written.
	// **None of them raises the verdict**: a lock is an administrator's
	// deliberate act, and a page that amber-lit somebody's own decision would
	// be teaching them to ignore amber. They are in the detail because "no
	// crashed or failed apps out of 40" reads as *everything is serving* on an
	// install where four are locked.
	var also []string
	if locked > 0 {
		also = append(also, fmt.Sprintf("%d locked", locked))
	}
	if withheld > 0 {
		also = append(also, fmt.Sprintf("%d withheld from publication", withheld))
	}
	tail := ""
	if len(also) > 0 {
		tail = " Also: " + strings.Join(also, ", ") + "."
	}

	// Two rows when there are two kinds of broken, because an operator triages
	// them differently — a crashed app is a workload that died and a failed one
	// is a build that never shipped — and one `?status=` takes one value, so a
	// single row could only ever link to half of what it counted. The links
	// carry `scope=all`: these counts are the estate's and the apps list
	// defaults to the reader's own.
	if crashed > 0 && failed > 0 {
		rp.addLinked("app_health", "App health", checkWarn,
			fmt.Sprintf("%s crashed, out of %s.", countOf(crashed, "app"), countOf(len(apps), "app")),
			"/?scope=all&status=crashed")
		rp.addLinked("app_health_failed", "App health", checkWarn,
			fmt.Sprintf("%s with a failed deploy.", countOf(failed, "app"))+tail,
			"/?scope=all&status=failed")
		return
	}
	if crashed > 0 {
		rp.addLinked("app_health", "App health", checkWarn,
			fmt.Sprintf("%s crashed, out of %s.", countOf(crashed, "app"), countOf(len(apps), "app"))+tail,
			"/?scope=all&status=crashed")
		return
	}
	if failed > 0 {
		rp.addLinked("app_health", "App health", checkWarn,
			fmt.Sprintf("%s with a failed deploy, out of %s.",
				countOf(failed, "app"), countOf(len(apps), "app"))+tail,
			"/?scope=all&status=failed")
		return
	}
	rp.add("app_health", "App health", checkPass,
		fmt.Sprintf("No crashed or failed apps out of %d.", len(apps))+tail)
}

// lastRefusedEmail is the newest app email of the last day, if it failed and
// nothing was delivered after it — the relay's own refusal, read from the sends
// ledger rather than from a test send this check would have to make.
func (s *Server) lastRefusedEmail() *database.IntegrationSend {
	since := time.Now().Add(-24 * time.Hour)
	sends, err := s.db.ListSends(database.SendFilter{Kind: "email", Since: &since, Limit: 50})
	if err != nil {
		return nil
	}
	return refusedLast(sends)
}

// refusedLast is the newest finished send if it failed, else nil. Sends are
// newest first; queued, blocked and abandoned ones say nothing about the relay.
func refusedLast(sends []database.IntegrationSend) *database.IntegrationSend {
	for i := range sends {
		switch sends[i].Status {
		case database.SendSent:
			return nil
		case database.SendFailed:
			return &sends[i]
		}
	}
	return nil
}

// checkNotifications reports whether an install can tell anyone anything. None
// of it is a failure: notification channels are optional and never
// load-bearing, so an unconfigured relay is a skip, and only a configured one
// with nowhere to deliver is a warning.
func (s *Server) checkNotifications(_ context.Context, rp *checkReport) {
	mailOn := s.mailer != nil && s.mailer.Enabled()
	switch {
	case s.mailer == nil:
		rp.add("email", "Email", checkSkip, "This build has no email support.")
	case !mailOn:
		rp.add("email", "Email", checkSkip,
			"No relay configured. Notifications are skipped; every workflow still completes in the UI.")
	default:
		to, err := s.db.AdminRecipients()
		if err != nil {
			rp.add("email", "Email", checkWarn, dbReadFailed("email", "the recipients", err))
		} else if len(to) == 0 {
			rp.add("email", "Email", checkWarn,
				"A relay is configured, but no administrator has an address on file and no group address is set — administrator mail has nowhere to go.")
		} else if last := s.lastRefusedEmail(); last != nil {
			// Configured is not working: a relay that rejects its credential
			// passed this check while every send failed (trytoft, 2026-10-23).
			rp.add("email", "Email", checkWarn,
				fmt.Sprintf("A relay is configured, but it refused the last email sent through it (%s): %s",
					last.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"), last.Error))
		} else {
			rp.add("email", "Email", checkPass,
				fmt.Sprintf("Relay configured; administrator mail goes to %s.", countOf(len(to), "recipient")))
		}
	}

	switch {
	case s.slack == nil:
		rp.add("slack", "Slack", checkSkip, "This build has no Slack support.")
	case !s.slack.Enabled():
		rp.add("slack", "Slack", checkSkip, "No bot token configured.")
	default:
		rp.add("slack", "Slack", checkPass, "Bot token configured and Slack is on.")
	}

	// Where links in a notification point. No request exists when one is
	// written, so this is the one address that must be configured rather than
	// worked out — and a notification carrying a localhost link is a
	// notification nobody can act on.
	if !mailOn && (s.slack == nil || !s.slack.Enabled()) {
		return
	}
	base := s.db.ResolvePublicBaseURL(s.cfg.BaseURL)
	if isLoopbackURL(base) {
		rp.add("public_base_url", "Public address", checkWarn,
			"Links in notifications will point at "+base+", which is only reachable from this machine. Set the public address in Admin → Settings.")
		return
	}
	rp.add("public_base_url", "Public address", checkPass,
		"Links in notifications point at "+base+".")
}

// isLoopbackURL reports whether an address only resolves on this machine. Used
// to judge an outbound link, never to admit or refuse a request.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkSignIn covers the two ways an install locks itself out or leaves itself
// open: no working door, and a door with the published default key still in it.
func (s *Server) checkSignIn(_ context.Context, rp *checkReport) {
	providers, err := s.db.ListAuthProviders()
	if err != nil {
		rp.add("sign_in", "Sign-in providers", checkWarn, dbReadFailed("sign_in", "the sign-in providers", err))
		return
	}
	var enabled, localEnabled int
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		if p.Kind == database.ProviderKindPassword {
			localEnabled++
			continue
		}
		enabled++
	}
	// D20: one administrator is not a configuration. Folded in here rather than
	// given a row of its own, because it is the same question this check
	// already asks from the other side — how many ways in does this install
	// have, and how many of them survive one thing going wrong.
	//
	// Not a warning when local sign-in is on: there is always exactly one way
	// in that needs no provider, and that is the answer to this risk rather
	// than a second finding about it.
	if localEnabled == 0 {
		if admins, err := s.db.CountSystemAdmins(); err == nil && admins == 1 {
			rp.add("lockout_risk", "Lockout risk", checkWarn,
				"One system administrator, and local sign-in is off. One provider outage or one "+
					"dormant account and nobody can administer this install. Re-enable local sign-in "+
					"from a shell with `toft local-admin enable`, or appoint a second administrator.")
		}
	}

	switch {
	case enabled == 0 && localEnabled == 0:
		rp.add("sign_in", "Sign-in providers", checkFail,
			"No provider is enabled and local sign-in is off — nobody can sign in. Re-enable local sign-in from a shell with `toft local-admin enable`.")
	case enabled == 0:
		rp.add("sign_in", "Sign-in providers", checkWarn,
			"Local sign-in is the only way in. Everyone but the built-in administrator needs a configured provider.")
	default:
		rp.add("sign_in", "Sign-in providers", checkPass,
			fmt.Sprintf("%s enabled.", countOf(enabled, "provider")))
	}

	if localEnabled == 0 {
		rp.add("builtin_admin", "Built-in administrator", checkSkip,
			"Local sign-in is disabled, so the built-in administrator cannot be used.")
		return
	}
	def, err := s.db.BuiltinAdminHasDefaultPassword()
	if err != nil {
		rp.add("builtin_admin", "Built-in administrator", checkWarn, dbReadFailed("builtin_admin", "the built-in administrator", err))
		return
	}
	if def {
		// Not a failure: the default password mints no session, only the right
		// to replace itself. It is still the first thing to fix on a reachable
		// install, because the first person to use it becomes the administrator.
		rp.add("builtin_admin", "Built-in administrator", checkWarn,
			"Still holds the published default password. Sign in as `admin` and set a real one — whoever does it first owns the account.")
		return
	}
	rp.add("builtin_admin", "Built-in administrator", checkPass,
		"Password has been changed from the published default.")
}

// checkDirectoryGroups reports whether the group claim is actually arriving.
//
// **`skip` on an install with no mappings**, which is every install until an
// administrator attaches one — absent, not off, and a check that warned over the
// majority install would train everybody to ignore the page.
//
// The `warn` is the one worth having: mappings exist for a provider and no
// membership has ever come through them. Two conditions look like that and both
// have the same remedy. Either the IdP was never configured to send the claim,
// or it is Entra declining to send it because the person is in more groups than
// it will assert — its overage limit — and in both cases the answer is at the
// IdP rather than here. This install cannot tell them apart, so it says what it
// knows and does not guess.
func (s *Server) checkDirectoryGroups(_ context.Context, rp *checkReport) {
	coverage, err := s.db.DirectoryMappingCoverage()
	if err != nil {
		rp.add("directory_groups", "Directory groups", checkWarn, dbReadFailed("directory_groups", "the group mappings", err))
		return
	}
	if len(coverage) == 0 {
		rp.add("directory_groups", "Directory groups", checkSkip,
			"No group takes its membership from a directory.")
		return
	}
	var silent, missing []string
	mappings, memberships := 0, 0
	for _, c := range coverage {
		mappings += c.Mappings
		memberships += c.Memberships
		if c.Memberships == 0 {
			// A provider the install does not have can never send anything,
			// and sending the administrator to its IdP is the wrong errand:
			// the mapping is the mistake (trytoft, 2026-10-23).
			if _, err := s.authProvider(c.ProviderID); errors.Is(err, errNoSuchProvider) {
				missing = append(missing, c.ProviderID)
			} else {
				silent = append(silent, c.ProviderID)
			}
		}
	}
	if len(missing) > 0 {
		rp.add("directory_groups", "Directory groups", checkWarn,
			fmt.Sprintf("Groups take members from %s, and this install has no sign-in provider by that id — "+
				"those mappings match nobody. Remove them from the group, or add the provider.",
				strings.Join(missing, ", ")))
		return
	}
	if len(silent) > 0 {
		rp.add("directory_groups", "Directory groups", checkWarn,
			fmt.Sprintf("%s: claim values are mapped and no membership has arrived. Check the provider "+
				"is configured to send the group claim — and that people are not in more groups than it will assert.",
				strings.Join(silent, ", ")))
		return
	}
	rp.add("directory_groups", "Directory groups", checkPass,
		fmt.Sprintf("%s mapped, %s from a directory.",
			countOf(mappings, "claim value"), countOf(memberships, "membership")))
}

// checkLoginGroupGate reports which providers refuse a sign-in on directory
// group membership, and what that costs if the claim stops arriving.
//
// **`skip` when no provider gates**, which is every install until an
// administrator fills the field in — absent, not off, and the rule that keeps
// this page from decaying into a wall of warnings.
//
// **`pass` when one does**, not `warn`, and that is the decision here. A gate
// somebody configured on purpose and that is working is not a fault, and a
// warning an administrator can never clear is exactly how this page decays into
// a wall of yellow nobody reads. What the detail carries instead is the thing
// worth knowing before the bad day: that a claim which stops arriving locks
// everyone out at once, and where the way back in is.
//
// It cannot do better than that. Nothing in this install can tell an IdP that
// is about to stop sending the claim from one that never will, and it
// deliberately does not read the audit log to count refusals — that would be a
// better signal, and `audit_log` has exactly two readers by invariant, neither
// of them this page.
func (s *Server) checkLoginGroupGate(_ context.Context, rp *checkReport) {
	providers, err := s.db.ListAuthProviders()
	if err != nil {
		rp.add("login_group_gate", "Permitted login groups", checkWarn, dbReadFailed("login_group_gate", "the sign-in providers", err))
		return
	}
	var gated []string
	for _, p := range providers {
		if len(p.PermittedLoginGroupList()) > 0 {
			gated = append(gated, p.ID)
		}
	}
	if len(gated) == 0 {
		rp.add("login_group_gate", "Permitted login groups", checkSkip,
			"No provider restricts sign-in by directory group.")
		return
	}
	rp.add("login_group_gate", "Permitted login groups", checkPass,
		fmt.Sprintf("%s: only members of the listed groups may sign in. If the provider stops sending groups, "+
			"everyone using it is locked out; the built-in administrator still gets in.",
			strings.Join(gated, ", ")))
}

// checkSCIM reports whether a configured feed has ever been used.
//
// **`skip` with no token**, which is every install until an administrator mints
// one — absent, not off.
//
// The `warn` is the shape of every SCIM misconfiguration: a token exists, an
// administrator believes deprovisioning is wired up, and the directory has never
// called. From this side that is indistinguishable from a connector somebody
// configured and did not enable, and both are fixed in the same place, so the
// check says what it knows rather than guessing which.
//
// It deliberately does not warn about a feed that worked and went quiet. That
// would need the last-call stamp compared against a threshold nobody has set,
// and an IdP that syncs weekly would look broken every Tuesday.
func (s *Server) checkSCIM(_ context.Context, rp *checkReport) {
	providers, err := s.db.ListAuthProviders()
	if err != nil {
		rp.add("scim", "Directory feed", checkWarn, dbReadFailed("scim", "the sign-in providers", err))
		return
	}
	var configured, silent []string
	for _, p := range providers {
		if p.SCIMTokenHash == "" {
			continue
		}
		configured = append(configured, p.ID)
		if p.SCIMLastCallAt == nil {
			silent = append(silent, p.ID)
		}
	}
	switch {
	case len(configured) == 0:
		rp.add("scim", "Directory feed", checkSkip,
			"No sign-in provider has a SCIM token, so nothing is pushing account changes here.")
	case len(silent) > 0:
		rp.add("scim", "Directory feed", checkWarn,
			fmt.Sprintf("%s: a token is configured and the directory has never called. Check the "+
				"connector is enabled at the provider and that the base URL and token were pasted whole.",
				strings.Join(silent, ", ")))
	default:
		rp.add("scim", "Directory feed", checkPass,
			fmt.Sprintf("%s pushing account changes.", countOf(len(configured), "provider")))
	}
}
