package builder

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The shared build cache: what the platform knows about its size.
//
// Four language tools write into one root — npm, uv and Go's build and module
// caches, wired in buildEnvironment — and until now nothing anywhere knew how
// large it had become. Not this package, not a metric, not the Checks page,
// which stats the directory and throws the result away.
//
// The failure that makes the number worth having is not a slow build. CACHE_DIR
// shares a filesystem with shared-mode release checkouts and every app's logs,
// so a cache that grows without bound does not degrade deploys, it stops them —
// and Repair with them, which means the next shared-mode workload needing its
// venv rebuilt fails to start. Everything already serving keeps serving, which
// is precisely what makes it arrive as a surprise.

const (
	// cacheMeasureInterval is how often the size is retaken.
	//
	// Fifteen minutes is chosen so the figure is never more than usefully stale
	// while the cost stays invisible: a walk of a content-addressed store is
	// real I/O on the same volume the platform serves releases from, and doing
	// it four times an hour is nothing while doing it per request would be the
	// diagnostic that breaks the install.
	//
	// A constant rather than a setting, deliberately. Nobody has ever measured
	// a walk of a cache that has been accumulating for a year, so a
	// configurable interval would be a dial with no evidence behind it. The
	// first install to find this too slow or too eager is the argument for
	// changing the number.
	cacheMeasureInterval = 15 * time.Minute

	// cacheMeasureTimeout bounds one walk. It exists so a pathological tree —
	// a mount that has gone away, a directory with millions of entries — costs
	// one abandoned measurement rather than a goroutine that never returns and
	// a figure that is never retaken.
	cacheMeasureTimeout = 5 * time.Minute
)

// CacheState is everything the platform knows about the shared build cache.
//
// Read, never stored: there is no column and no settings row behind any of it,
// and a restart forgets the lot. That is correct — the only question anybody
// asks of it is about now, and the measurer answers it again within fifteen
// minutes.
type CacheState struct {
	// Bytes is the last completed measurement. Meaningless unless MeasuredAt
	// is set; see Measured.
	Bytes int64

	// MeasuredAt is when that measurement finished. **Zero means never**, and
	// the distinction is load-bearing: an install in the first minutes after a
	// restart has not got an empty cache, and every surface has to be able to
	// say "not measured yet" rather than show a confident zero.
	MeasuredAt time.Time

	// Err is why the last attempt did not produce a figure, empty when it did.
	// A failed measurement leaves the previous Bytes and MeasuredAt alone: a
	// stale number with an honest timestamp beside it is worth more than no
	// number at all.
	Err string

	// Reclaiming, and since when. The pair is what a second request is refused
	// against, and what lets the card say how long the administrator has been
	// waiting rather than only that they are.
	Reclaiming bool
	StartedAt  time.Time

	// What the last reclaim did. Started, finished and freed are enough for a
	// surface to narrate itself without a stream: there is nothing between
	// "running" and "done" that a person can act on, so no progress is offered.
	LastFreed   int64
	LastReclaim time.Time

	// LastError is the tools that skipped or failed, one line, empty when they
	// all worked. Not a fourth status — a reclaim in which npm was absent still
	// reclaimed whatever uv and Go had.
	LastError string
}

// Measured reports whether a figure has ever been taken. The accessor exists so
// that no caller has to remember that the zero time is the sentinel.
func (c CacheState) Measured() bool { return !c.MeasuredAt.IsZero() }

// CacheStatus is the whole read side, and it does no I/O.
//
// Every caller is on a path that cannot afford a directory walk — the Checks
// report runs each check under a five-second deadline, and the metric export
// reads its gauges on the collector's interval — so the walk happens on the
// measurer's ticker and everyone else reads what it left behind. A reader that
// walked would be the check that fails because the thing it is checking is
// large, which reads as broken rather than big.
func (b *Builder) CacheStatus() CacheState {
	b.cacheStateMu.Lock()
	defer b.cacheStateMu.Unlock()
	return b.cacheState
}

// CacheDir is where the shared cache lives, or empty on a builder configured
// without one. Exported because the surfaces that report the size also name the
// directory, and nothing outside this package should be reassembling the path
// from configuration of its own.
func (b *Builder) CacheDir() string { return b.opts.CacheDir }

// startCacheMeasurer takes the size once and then on the interval, for the life
// of the process.
//
// It has no stop: the Builder's workers do not have one either, and a process
// that is shutting down has nothing to gain from an orderly end to a directory
// walk. It is started only when a cache directory is configured, so a builder
// standing on its own in a test walks nothing.
func (b *Builder) startCacheMeasurer() {
	go func() {
		b.measureCacheOnce()
		for range time.Tick(cacheMeasureInterval) {
			b.measureCacheOnce()
		}
	}()
}

// measureCacheOnce walks the cache and stores what it found.
func (b *Builder) measureCacheOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), cacheMeasureTimeout)
	defer cancel()

	bytes, err := directorySize(ctx, b.opts.CacheDir)
	b.cacheStateMu.Lock()
	defer b.cacheStateMu.Unlock()
	if err != nil {
		// The previous figure and its timestamp survive. The screen shows how
		// old it is, which is the honest thing to show when the newest attempt
		// did not land.
		b.cacheState.Err = err.Error()
		log.Printf("build cache: could not measure %s: %v", b.opts.CacheDir, err)
		return
	}
	b.cacheState.Bytes = bytes
	b.cacheState.MeasuredAt = time.Now().UTC()
	b.cacheState.Err = ""
}

// directorySize sums the regular files under root.
//
// Two things it deliberately does not do. It does not follow symlinks, so a
// link out of the cache cannot make the platform report somebody else's disk.
// And it does not stop for an unreadable entry or one that vanished underneath
// it — a cache is being written by npm and uv while this runs, and a file
// disappearing mid-walk is the normal case rather than an error. The figure is
// approximate by nature and one missed entry does not change what it is for.
//
// Hard-linked content is counted once per link rather than once per inode, so
// the number is an upper bound on what emptying the directory would return.
// Saying that is cheaper than an inode table, and the question the figure
// answers — is this growing without bound — is not sensitive to it.
func directorySize(ctx context.Context, root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// Unreadable or gone. Skip the subtree and keep counting; only a
			// failure at the root itself is worth reporting, and WalkDir gives
			// us that as the error from the call.
			if path == root {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Emptying it.
// ---------------------------------------------------------------------------

const (
	// cacheReclaimVerbTimeout bounds one tool. Each verb gets its own, so one
	// hanging tool costs its own step rather than holding the write lock — and
	// every build behind it — for as long as it feels like.
	cacheReclaimVerbTimeout = 5 * time.Minute

	// modcacheEscalationFraction is how much of the cache the cheap verbs have
	// to give back before `go clean -modcache` is left alone.
	//
	// The module cache is the most expensive of the four to lose — a
	// re-download is bandwidth and wall time on the next build of every Go app
	// — so it is the escalation rather than a peer. A quarter is a judgement
	// with no data behind it yet, which is said plainly here rather than
	// dressed up as a setting: nobody has measured a real estate's cache, and a
	// dial nobody can set correctly is worse than a constant somebody can argue
	// with.
	modcacheEscalationFraction = 0.25
)

// The three outcomes one verb can have. A missing tool is skipped and reported
// rather than failed: an install with no Go toolchain has no module cache worth
// reclaiming, and calling that an error would make a healthy install look
// broken every time somebody pressed the button.
const (
	ReclaimDone    = "reclaimed"
	ReclaimSkipped = "skipped"
	ReclaimFailed  = "failed"
)

// ReclaimStep is what one tool did.
type ReclaimStep struct {
	Tool    string `json:"tool"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`

	// empty marks a skip whose directory was never made: nothing to clear is
	// the step working, so the audit row keeps it and Problems does not.
	empty bool
}

// ReclaimResult is the whole act, and it is what the audit row carries.
//
// Before and After are measurements rather than the tools' own claims about
// what they removed: npm and uv report in their own formats and about their own
// stores, and the only figure an operator can act on is what the directory
// weighs now against what it weighed then.
type ReclaimResult struct {
	Before   int64         `json:"before_bytes"`
	After    int64         `json:"after_bytes"`
	Freed    int64         `json:"freed_bytes"`
	Steps    []ReclaimStep `json:"steps"`
	Duration time.Duration `json:"-"`
}

// Problems is the one-line summary of what did not go to plan, empty when
// everything did. It is what the card shows and what LastError keeps. A step
// that found its directory already empty is not on it: listing those beside a
// reclaim that freed gigabytes turned a healthy result into a warning, and
// named the shared renv cache as empty while the accounts' own were the ones
// emptied.
func (r ReclaimResult) Problems() string {
	var parts []string
	for _, s := range r.Steps {
		if s.Outcome != ReclaimDone && !s.empty {
			parts = append(parts, s.Tool+": "+s.Detail)
		}
	}
	return strings.Join(parts, " ")
}

// reclaimVerb is one tool's own emptying command.
//
// Each tool is asked in its own words rather than having its directory walked
// and deleted. Only the tools know what is still referenced: npm's cacache and
// uv's store are content-addressed with their own indexes, and a hand-rolled
// mtime sweep over either is a corruption waiting for a version bump.
type reclaimVerb struct {
	tool   string
	name   string
	args   []string
	absent string
}

var (
	// The cheap two, in order. Neither gives up anything a build cannot
	// reconstruct without a network round trip per package.
	//
	// These are each tool's *clearing* verb, and the distinction is the whole
	// feature. Both tools also have a maintenance verb that looks like the
	// polite choice and reclaims nothing: `uv cache prune` drops only dangling
	// entries and answers "No unused entries found" against a cache whose every
	// entry is still referenced, and `npm cache verify` is a checksum pass that
	// garbage-collects unreferenced content once and nothing on any run after.
	// A button wired to those two reports a few hundred bytes against gigabytes,
	// which is not a smaller reclaim — it is the button not working.
	cheapReclaimVerbs = []reclaimVerb{
		{tool: "uv", name: "uv", args: []string{"cache", "clean"},
			absent: "uv is not installed on this host, so there is no uv cache to clear."},
		{tool: "npm", name: "npm", args: []string{"cache", "clean", "--force"},
			absent: "npm is not installed on this host, so there is no npm cache to clear."},
	}

	// The escalation. `go-build` is deliberately not here and never will be:
	// Go trims it itself — entries unused for about five days, at most once a
	// day — and `go clean -cache` would throw away the compile results that
	// make the *next* build fast for no disk the module cache does not give up
	// more of.
	modcacheVerb = reclaimVerb{tool: "go-mod", name: "go", args: []string{"clean", "-modcache"},
		absent: "Go is not installed on this host, so there is no module cache to clean."}
)

// StartCacheReclaim begins a reclaim and returns immediately.
//
// It returns false, with the state describing what is already running, when a
// reclaim is in flight — the only thing it refuses. It deliberately does *not*
// refuse because a build is running: the reclaim takes the cache's write side
// and waits, which is safe and bounded, and "try again when nothing is
// deploying" is an instruction an administrator on a busy install can never
// satisfy.
//
// onDone is called once the work has finished, with what it did. It is how the
// audit row comes to carry the figure it freed rather than only the fact that
// somebody asked — a row saying an administrator pressed a button and not what
// happened is the row you cannot use six weeks later.
func (b *Builder) StartCacheReclaim(onDone func(ReclaimResult)) (bool, CacheState) {
	b.cacheStateMu.Lock()
	if b.cacheState.Reclaiming {
		st := b.cacheState
		b.cacheStateMu.Unlock()
		return false, st
	}
	b.cacheState.Reclaiming = true
	b.cacheState.StartedAt = time.Now().UTC()
	b.cacheState.LastError = ""
	st := b.cacheState
	b.cacheStateMu.Unlock()

	go func() {
		res := b.reclaimCache()

		b.cacheStateMu.Lock()
		b.cacheState.Reclaiming = false
		b.cacheState.LastFreed = res.Freed
		b.cacheState.LastReclaim = time.Now().UTC()
		b.cacheState.LastError = res.Problems()
		// Re-measured by the reclaim itself, so the number the administrator is
		// looking at changes because of what they just did rather than fifteen
		// minutes later.
		b.cacheState.Bytes = res.After
		b.cacheState.MeasuredAt = time.Now().UTC()
		b.cacheStateMu.Unlock()

		if onDone != nil {
			onDone(res)
		}
	}()
	return true, st
}

// reclaimCache is the work, and it holds the cache's write side for all of it.
//
// Taken here rather than before the goroutine is launched, so the route can
// answer 202 immediately and the waiting happens where nobody is watching.
func (b *Builder) reclaimCache() ReclaimResult {
	started := time.Now()

	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()

	env := b.cacheEnv()
	res := ReclaimResult{Before: b.measureNow()}

	for _, v := range cheapReclaimVerbs {
		res.Steps = append(res.Steps, b.runReclaimVerb(v, env))
	}
	res.After = b.measureNow()

	// The escalation, decided on measurements rather than on what the tools
	// said about themselves.
	// renv's cache is in it with the module cache, for the same reason: what
	// it holds is the packages a lockfile pins below what the repository
	// serves as a binary, compiled at a minute or more each, and losing it is
	// that compile again on the next deploy of every R app.
	if b.modcacheIsWorthIt(res.Before, res.After) {
		res.Steps = append(res.Steps, b.reclaimRenv())
		res.Steps = append(res.Steps, b.runReclaimVerb(modcacheVerb, env))
		res.Steps = append(res.Steps, b.reclaimAccounts())
		res.Steps = append(res.Steps, b.reclaimDeps())
		res.After = b.measureNow()
	}

	res.Freed = res.Before - res.After
	if res.Freed < 0 {
		// A build finished between two walks, or a tool wrote while another
		// pruned. Nothing was freed; saying so beats reporting a negative.
		res.Freed = 0
	}
	res.Duration = time.Since(started)
	return res
}

// modcacheIsWorthIt decides whether the module cache is emptied too.
//
// An empty cache to begin with means there is nothing to escalate about. A
// cache the cheap verbs already gave up a meaningful share of is left with its
// modules: the disk has been recovered, and the next Go build should not have
// to re-download the world to prove it.
func (b *Builder) modcacheIsWorthIt(before, after int64) bool {
	if before <= 0 {
		return false
	}
	return float64(before-after) < modcacheEscalationFraction*float64(before)
}

// measureNow walks the cache and reports what it weighs, or zero if it cannot.
// A failed walk inside a reclaim is not worth abandoning the reclaim for — the
// verbs are the point and the figure is the narration.
func (b *Builder) measureNow() int64 {
	ctx, cancel := context.WithTimeout(context.Background(), cacheMeasureTimeout)
	defer cancel()
	n, err := directorySize(ctx, b.opts.CacheDir)
	if err != nil {
		return 0
	}
	return n
}

// runReclaimVerb runs one tool by bare name, through the same mechanism a build
// uses, so a missing tool arrives as exec.ErrNotFound and is skipped rather
// than failed.
func (b *Builder) runReclaimVerb(v reclaimVerb, env map[string]string) ReclaimStep {
	err := b.runCommandFor(cacheReclaimVerbTimeout, "", "", v.name, v.args, env,
		func(line string) { log.Printf("build cache: %s: %s", v.tool, strings.TrimSpace(line)) })

	switch {
	case errors.Is(err, exec.ErrNotFound):
		return ReclaimStep{Tool: v.tool, Outcome: ReclaimSkipped, Detail: v.absent}
	case err != nil:
		// The other two still run. One tool that will not answer is a step that
		// did not happen, not a reclaim that did not happen.
		return ReclaimStep{Tool: v.tool, Outcome: ReclaimFailed, Detail: firstLine(err.Error())}
	}
	return ReclaimStep{Tool: v.tool, Outcome: ReclaimDone}
}

// cacheEnv points the four language tools at the four directories the platform
// owns. It is the single naming of them: buildEnvironment merges it into every
// build's environment, and the reclaim runs its verbs under it, so a build
// cannot fill a directory the reclaim does not empty.
func (b *Builder) cacheEnv() map[string]string {
	if b.opts.CacheDir == "" {
		return nil
	}
	return map[string]string{
		"npm_config_cache": filepath.Join(b.opts.CacheDir, "npm"),
		"UV_CACHE_DIR":     filepath.Join(b.opts.CacheDir, "uv"),
		"GOCACHE":          filepath.Join(b.opts.CacheDir, "go-build"),
		"GOMODCACHE":       filepath.Join(b.opts.CacheDir, "go-mod"),
		// uv hardlinks from its cache into the venv and falls back to a copy
		// when the two are on different filesystems — which CACHE_DIR and a
		// release directory routinely are — printing "Failed to hardlink
		// files" into every Python build log on the way. Copy is what it was
		// doing anyway; saying so up front removes the warning, and a release
		// that shares no inode with the cache is one the reclaim cannot touch.
		"UV_LINK_MODE": "copy",
	}
}

// renvCacheDir is the fifth directory, and the one deliberately **not** in
// cacheEnv. Everything in cacheEnv reaches every build command; this reaches
// only the renv commands, through runner.RenvEnv, which is also what turns
// renv's symlinks off. A render or an app's own .Rprofile that loads renv sees
// the cache path without that setting would fill the library with links into
// it — the release that cannot travel (ADR §64). Named here, beside the other
// four, so the reclaim empties the directory the builds fill.
func (b *Builder) renvCacheDir() string {
	if b.opts.CacheDir == "" {
		return ""
	}
	return filepath.Join(b.opts.CacheDir, "renv")
}

// reclaimAccounts empties every app account's own caches (identity.go) by
// removing them. Deleted rather than asked, like renv's: the verbs above would
// have to run once per account, as that account, and the write lock this runs
// under means no build is using any of them. On the escalation path because it
// is the expensive one — each app's next build fetches what it had.
func (b *Builder) reclaimAccounts() ReclaimStep {
	dir := filepath.Join(b.opts.CacheDir, accountsDirName)
	if b.opts.CacheDir == "" || !fileExists(dir) {
		return ReclaimStep{Tool: "accounts", Outcome: ReclaimSkipped, empty: true,
			Detail: "No app has built as an account of its own here, so there is nothing to clear."}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ReclaimStep{Tool: "accounts", Outcome: ReclaimFailed, Detail: firstLine(err.Error())}
	}
	for _, e := range entries {
		// RemoveAll does not follow links on Linux: it walks with openat and
		// unlinkat, so an account's tree cannot turn this into a delete
		// elsewhere.
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return ReclaimStep{Tool: "accounts", Outcome: ReclaimFailed, Detail: firstLine(err.Error())}
		}
	}
	return ReclaimStep{Tool: "accounts", Outcome: ReclaimDone}
}

// reclaimDeps removes the kept dependency trees of an install with no accounts
// (depreuse.go); an account's own go with reclaimAccounts. Removed rather than
// asked, like renv's: nothing references a kept tree — a build copies out of
// it — and the write lock this runs under means no build is copying.
func (b *Builder) reclaimDeps() ReclaimStep {
	dir := filepath.Join(b.opts.CacheDir, depsDirName)
	if b.opts.CacheDir == "" || !fileExists(dir) {
		return ReclaimStep{Tool: "deps", Outcome: ReclaimSkipped, empty: true,
			Detail: "The shared directory of kept Node dependency trees is empty; an app account's own are cleared with its account."}
	}
	if err := os.RemoveAll(dir); err != nil {
		return ReclaimStep{Tool: "deps", Outcome: ReclaimFailed, Detail: firstLine(err.Error())}
	}
	return ReclaimStep{Tool: "deps", Outcome: ReclaimDone}
}

// reclaimRenv empties the R package cache by removing it.
//
// The one directory that is deleted rather than asked: renv has no verb that
// clears a whole cache (`renv::purge` takes one package at a time), and the
// reason a hand-rolled delete is wrong for npm and uv — only the tool knows
// what is still referenced — does not hold here. Every release holds a *copy*
// (RENV_CONFIG_CACHE_SYMLINKS=FALSE), so nothing references the cache at all,
// and the write lock this runs under means no restore is filling it.
func (b *Builder) reclaimRenv() ReclaimStep {
	dir := b.renvCacheDir()
	if !fileExists(dir) {
		return ReclaimStep{Tool: "renv", Outcome: ReclaimSkipped, empty: true,
			Detail: "The shared R package cache is empty; an app account's own is cleared with its account."}
	}
	if err := os.RemoveAll(dir); err != nil {
		return ReclaimStep{Tool: "renv", Outcome: ReclaimFailed, Detail: firstLine(err.Error())}
	}
	return ReclaimStep{Tool: "renv", Outcome: ReclaimDone}
}
