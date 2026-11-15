package deps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"toft/internal/advisory"
	"toft/internal/toolchain"
)

// Resolving what a build is about to install.
//
// **The scan happens before any dependency code executes**, or this is a
// report and not a control. Each ecosystem has a lockfile that answers the
// question without running anything, and a fallback that resolves metadata
// without running anything either:
//
//	npm    package-lock.json                 | npm install --package-lock-only --ignore-scripts
//	PyPI   a hashed requirements.txt, or uv.lock | uv pip compile --no-build
//	Go     go.mod + go.sum                   | go list -m -json all
//
// Two of those need saying out loud.
//
// **Python resolution can execute code.** A source distribution with no wheel
// may run setup.py to yield its metadata. `--no-build` is preferred and, where
// a package cannot be resolved without building, the log says *which package*
// forced it rather than silently doing it. This is a real hole in "before any
// dependency code executes"; naming it is the honest treatment, and closing it
// means refusing sdists, which would refuse a great many legitimate apps.
//
// **Go gets a coarser answer than it could.** `govulncheck` is symbol-level
// and would cut Go's false-positive rate enormously. It needs its own database
// and its own analysis pass, so it is a later refinement rather than this.

// resolveTimeout bounds a resolution command. Generous, because a cold npm
// metadata resolve on a large tree over a slow link is genuinely slow, and
// short enough that a hung registry does not hold a build slot all afternoon.
// waitDelay is how long Run may wait on the pipes after that. Variables only so
// a test can shorten them.
var (
	resolveTimeout = 5 * time.Minute
	waitDelay      = 10 * time.Second
)

// Dependency is one installed package, as the inventory records it.
type Dependency struct {
	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	Version   string `json:"version"`
	// Direct means the app's own manifest names it.
	Direct bool `json:"direct"`
	// Dev is a build-time dependency. Retained rather than dropped, because
	// for Node the build *runs* them on a host that holds the git token, so
	// whether they are judged is a policy question and not a resolver one.
	Dev bool `json:"dev"`
	// Path is one representative transitive chain, "app > webpack > lodash".
	Path string `json:"path,omitempty"`
	// Source is where the row came from: one of the Source constants. Empty is
	// SourceResolved, which is every row a lockfile or manifest produced.
	Source string `json:"source,omitempty"`
}

// The sources an inventory row can have.
//
// Every ecosystem but R is read before the build and is `resolved`. R is read
// off the library the build installed into, and that library holds three
// kinds of package an owner has to be able to tell apart beside a finding:
// the ones the app asked for, the ones Toft put there whatever the app said —
// which no change to the app's code can move — and the ones that appeared
// after the deploy, because the app installed them while it ran.
const (
	SourceResolved  = "resolved"
	SourceInstalled = "installed"
	SourcePlatform  = "platform"
	SourceRuntime   = "runtime"
)

// SourceOf is d's source, with the default spelled out.
func SourceOf(d Dependency) string {
	if d.Source == "" {
		return SourceResolved
	}
	return d.Source
}

// Resolution is what one build's dependencies turned out to be.
type Resolution struct {
	Dependencies []Dependency
	// Ecosystems is what was actually read, in order. An app with two
	// manifests has two answers, and an app whose only manifest could not be
	// read has none — which is *unresolvable*, a state with a name, and never
	// the same thing as "no findings".
	Ecosystems []string
	// Notes are lines for the build log: what was skipped, what forced a
	// build, what could not be read. **No silent caps** — anything this
	// resolution did not cover is said where somebody will see it.
	Notes []string
	// Gaps are what this resolution did not read, as facts with a kind rather
	// than sentences: the same things the Notes say, for a reader that must
	// not parse prose — the verdict records them, and an SBOM says from them
	// what it cannot vouch for. The kinds are the list.
	Gaps []Gap
	// HashedRequirements is a PyPI set read from a requirements.txt that pins
	// and hashes every package, rather than resolved: the build installs that
	// file with --require-hashes, so it is the lock. An SBOM names the file
	// from it, so a reader is not left looking for a uv.lock.
	HashedRequirements bool
}

// Gap is one thing a resolution did not read.
type Gap struct {
	Kind      string `json:"kind"`
	Ecosystem string `json:"ecosystem,omitempty"`
	File      string `json:"file,omitempty"`
	// Reason is why a resolver gave up, when it knows: a sentence fragment
	// that reads after "were not checked:". Empty is a real read failure — the
	// manifest could not be parsed, or the tool failed for its own reasons —
	// and only then may a summary say the file "could not be read". A
	// requirements.txt read perfectly whose packages could not be listed as
	// the build installs them is the other case, and telling its owner the
	// file was unreadable sends them looking at the wrong thing (docs/bugs
	// 2026-10-28 #4).
	Reason string `json:"reason,omitempty"`
}

// GapReasoner is an error that knows why its ecosystem was not checked, in the
// words Gap.Reason carries.
type GapReasoner interface{ GapReason() string }

// The gap kinds.
const (
	// GapForeignLock is a pnpm or Yarn lock passed over: npm resolved
	// package.json instead, so the versions can differ from the ones pinned.
	GapForeignLock = "foreign_lock"
	// GapUnlocked is an ecosystem resolved at build time with no lockfile, so
	// a later build can resolve different versions.
	GapUnlocked = "unlocked"
	// GapVendored is a committed node_modules or vendor directory, whose
	// contents are not resolved at all.
	GapVendored = "vendored"
	// GapUnresolved is an ecosystem whose resolver gave up: it has no packages
	// here and is not listed as resolved. File names the manifest when the
	// resolver failed on it, which is also an UnreadError from Resolve.
	GapUnresolved = "unresolved"
	// GapDirectOnly is Python's pinned fallback: the file's own lines and no
	// transitive set.
	GapDirectOnly = "direct_only"
)

// gap records one, where somebody is collecting them.
func gap(gaps *[]Gap, kind, ecosystem, file string) {
	if gaps != nil {
		*gaps = append(*gaps, Gap{Kind: kind, Ecosystem: ecosystem, File: file})
	}
}

// unresolved is whether this resolution's gaps leave ecosystem unread.
func unresolved(gaps []Gap, ecosystem string) bool {
	for _, g := range gaps {
		if g.Kind == GapUnresolved && g.Ecosystem == ecosystem {
			return true
		}
	}
	return false
}

// Logf is how a resolver talks to whoever is watching the build.
type Logf func(format string, args ...any)

// UnreadError is a Resolve that could not read one or more of the manifests the
// build is about to install from. It travels *with* a Resolution holding
// everything the other resolvers did read, because one ecosystem's file is no
// reason to throw away another's inventory.
//
// It is not the gate's own failure. The file is one the app's author chose —
// a version 1 package-lock.json that `npm ci` installs from and this platform
// does not read, a renv.lock that is not JSON — so under `block` it refuses
// the release rather than letting it through as *not scanned*, which would
// make committing an unreadable lockfile a way past the policy.
type UnreadError struct {
	Problems []UnreadProblem
}

// UnreadProblem is one manifest that could not be read.
type UnreadProblem struct {
	Ecosystem string
	File      string
	Err       error
	// Remedy is the way forward when it is not "commit a lockfile this
	// platform reads" — taken from an Err that knows it.
	Remedy string
}

func (e *UnreadError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, fmt.Sprintf("%s (%s): %v", p.File, p.Ecosystem, p.Err))
	}
	return strings.Join(parts, "; ")
}

// Files names the manifests, for a fingerprint and a headline.
func (e *UnreadError) Files() []string {
	out := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		out = append(out, p.File)
	}
	return out
}

// firstPresent is the first of names in dir, or the last when none is.
func firstPresent(dir string, names ...string) string {
	for _, n := range names {
		if exists(dir, n) {
			return n
		}
	}
	return names[len(names)-1]
}

// Resolve reads a release directory and returns what it would install.
//
// Every ecosystem present is resolved, because a repo with package.json and
// requirements.txt has two answers and interleaving them would hide that the
// fix, the lockfile and often the person are different for each.
//
// An ecosystem whose resolver fails is a GapUnresolved naming its manifest,
// and the others are still resolved; the error, when there is one, is an
// *UnreadError beside the partial Resolution.
func Resolve(ctx context.Context, dir string, logf Logf) (Resolution, error) {
	return ResolveWith(ctx, dir, Options{}, logf)
}

// Options are what a resolution needs to know about the release beyond its
// directory.
type Options struct {
	// Python is the interpreter the build will install with — the release's
	// resolved `[runtime] python`, or the install's default for an app that
	// is not Python — passed to every `uv pip compile` as --python. Without it
	// uv would discover an interpreter itself, from the release's uv.toml or
	// a committed .venv, and execute it as the platform (DG-2). Empty means
	// this install has no Python: uv is not run, and what needed resolving
	// is a gap.
	Python string
	// EarlierPythons are the Python lines this install offers below the
	// release's own, as series ("3.13"). A refusal for a missing wheel
	// suggests declaring one of them only when the package publishes a wheel
	// for one: a release with no Linux wheel for any line was told to try
	// an earlier line it had no wheel for either (BB-039, fourth 2026-11-03
	// black-box list).
	EarlierPythons []string
}

// ResolveWith is Resolve for a release whose interpreter is known.
func ResolveWith(ctx context.Context, dir string, opts Options, logf Logf) (Resolution, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var out Resolution
	var unread UnreadError
	failed := func(ecosystem, file string, err error) {
		g := Gap{Kind: GapUnresolved, Ecosystem: ecosystem, File: file}
		var why GapReasoner
		if errors.As(err, &why) {
			g.Reason = why.GapReason()
		}
		out.Gaps = append(out.Gaps, g)
		p := UnreadProblem{Ecosystem: ecosystem, File: file, Err: err}
		var r interface{ Remedy() string }
		if errors.As(err, &r) {
			p.Remedy = r.Remedy()
		}
		unread.Problems = append(unread.Problems, p)
	}

	if exists(dir, "package.json") {
		deps, notes, err := resolveNPM(ctx, dir, logf, &out.Gaps)
		out.Notes = append(out.Notes, notes...)
		if err != nil {
			failed(advisory.EcosystemNPM, firstPresent(dir, "package-lock.json", "package.json"), err)
		} else {
			out.Dependencies = append(out.Dependencies, deps...)
			out.Ecosystems = append(out.Ecosystems, advisory.EcosystemNPM)
		}
	}
	if exists(dir, "requirements.txt") || exists(dir, "pyproject.toml") || exists(dir, "uv.lock") {
		deps, notes, err := resolvePyPI(ctx, dir, opts.Python, opts.EarlierPythons, logf, &out.Gaps, &out.HashedRequirements)
		out.Notes = append(out.Notes, notes...)
		if err != nil {
			failed(advisory.EcosystemPyPI, firstPresent(dir, "uv.lock", "requirements.txt", "pyproject.toml"), err)
		}
		out.Dependencies = append(out.Dependencies, deps...)
		// Listed only when its resolver produced a set: one that gave up
		// recorded the gap, and listing it would put "resolved, no findings"
		// on an ecosystem nothing read.
		if !unresolved(out.Gaps, advisory.EcosystemPyPI) {
			out.Ecosystems = append(out.Ecosystems, advisory.EcosystemPyPI)
		}
	}
	if exists(dir, "go.mod") {
		deps, notes, err := resolveGo(ctx, dir, logf, &out.Gaps)
		out.Notes = append(out.Notes, notes...)
		if err != nil {
			failed(advisory.EcosystemGo, "go.mod", err)
		}
		out.Dependencies = append(out.Dependencies, deps...)
		if !unresolved(out.Gaps, advisory.EcosystemGo) {
			out.Ecosystems = append(out.Ecosystems, advisory.EcosystemGo)
		}
	}

	// R, keyed on renv.lock alone and never on the framework.
	//
	// Deliberately not on "is this an R app": a .qmd repository carrying a
	// requirements.txt resolves as PyPI whatever family it lands in, which is
	// exactly right for the Jupyter engine, and the same repository carrying a
	// renv.lock has R dependencies that are just as real. The file is the fact.
	//
	// The ecosystem is *not* appended to out.Ecosystems, and that is the one
	// place this branch differs from its three siblings. out.Ecosystems is the
	// **covered** list — the ecosystems whose silence is a verdict — and CRAN
	// is not one: OSV's corpus is 14 advisories in its entire history, so
	// listing it would put a permanent "0 findings" on every R app, a statement
	// about the feed wearing the appearance of a statement about the app.
	//
	// It does not follow that the packages go unchecked. They are matched
	// against the CRAN corpus like any others — see advisory.Synced, which is
	// the wider list, and the gate in internal/builder, which derives findings
	// on both branches and varies only `scanned`.
	if exists(dir, "renv.lock") {
		deps, notes, err := resolveCRAN(dir, logf)
		out.Notes = append(out.Notes, notes...)
		if err != nil {
			failed(advisory.EcosystemCRAN, "renv.lock", err)
		} else {
			out.Dependencies = append(out.Dependencies, deps...)
		}
	}

	// A committed dependency tree is not resolved, and the warning is the
	// whole of the mitigation — so it goes where somebody reads it rather than
	// only into a build log nobody opens twice.
	for _, vendored := range []struct{ path, what string }{
		{"node_modules", "a committed node_modules directory"},
		{"vendor", "a committed vendor directory"},
	} {
		if exists(dir, vendored.path) {
			out.Notes = append(out.Notes, "Warning: "+vendored.what+" is present. Its contents are "+
				"not resolved and are not checked against advisory data.")
			gap(&out.Gaps, GapVendored, "", vendored.path)
		}
	}

	// **Every row has to be one Postgres can store.** A JSON manifest can
	// spell a NUL as \u0000, which a text column refuses, and one refused row
	// fails the whole inventory insert — which the gate then read as a release
	// with nothing in it. A package or version that is not short, valid UTF-8
	// free of control characters is the app's own manifest being unreadable:
	// the row is dropped and the file is reported, so `block` refuses it and
	// `warn` says so (security review 2026-11-15, LB-1).
	kept := out.Dependencies[:0]
	reported := map[string]bool{}
	for _, d := range out.Dependencies {
		if Recordable(d.Package) && Recordable(d.Version) {
			kept = append(kept, d)
			continue
		}
		if !reported[d.Ecosystem] {
			reported[d.Ecosystem] = true
			failed(d.Ecosystem, manifestOf(dir, d.Ecosystem), &unrecordableError{pkg: d.Package, ver: d.Version})
		}
	}
	out.Dependencies = kept

	// A direct dependency's chain is the app and itself — what npm's walk
	// already writes. The other resolvers record no chain, so a direct finding
	// carried no `path` while the documents promise one (D-09, third
	// 2026-11-03 list).
	for i := range out.Dependencies {
		if d := &out.Dependencies[i]; d.Direct && d.Path == "" {
			d.Path = "(this app) > " + d.Package
		}
	}
	sortDependencies(out.Dependencies)
	if len(unread.Problems) > 0 {
		return out, &unread
	}
	return out, nil
}

// errGitDependency is an unlocked resolve this scan will not run because a git
// dependency would be fetched — or could be, on an npm that cannot be told not
// to. It is the app's manifest being unread: `block` refuses, `warn` warns.
var errGitDependency = errors.New("a git dependency is not resolved by the scan — " +
	"commit a package-lock.json")

// lockNPM writes a package-lock.json for an unlocked release, as the platform.
//
// Every flag here outranks the release's own .npmrc, which npm still reads as
// project configuration:
//
//   - --git=git: a project .npmrc can name the program npm runs for a git
//     dependency (security review 2026-11-10, E-2).
//   - --allow-git=none: a hosted git dependency at a full commit is cloned and,
//     when its package.json has workspaces or a prepare/build/install entry,
//     npm spawns a second `npm install --force` inside the clone — with the
//     dependency's own .npmrc live (its `git=`, the release's `node-options`),
//     which --git=git does not reach because a value equal to the default is
//     not exported to the child. A non-default flag is exported, so no git
//     dependency is fetched at all, by the scan or by a child (LB-U1). An npm
//     too old to know the flag would ignore it silently, so it is probed first
//     and the resolve is not run on one.
//   - --cache=<a fresh directory>: npm's HTTP cache is keyed by URL alone, and
//     the release's .npmrc picks the proxy and whether certificates are
//     checked. One cache for every app's scan let one app plant a packument
//     that another app's scan then locked and its build installed. Builds were
//     given per-account caches for exactly that reason; the scan gets one per
//     run, platform-owned and removed afterwards. The flag also outranks a
//     project `cache=` key, which would otherwise have the platform write
//     wherever the release names (LB-3).
func lockNPM(ctx context.Context, dir string) error {
	if !npmKnowsAllowGit() {
		return errGitDependency
	}
	cache, err := os.MkdirTemp("", "toft-npm-scan-")
	if err != nil {
		return fmt.Errorf("the scan's npm cache could not be made: %w", err)
	}
	defer os.RemoveAll(cache)
	err = run(ctx, dir, "npm", "install", "--package-lock-only", "--ignore-scripts",
		"--no-audit", "--no-fund", "--git=git", "--allow-git=none", "--cache="+cache)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "EALLOWGIT") {
		return errGitDependency
	}
	// The scan's own directory is no part of what a developer reads.
	return errors.New(strings.ReplaceAll(err.Error(), cache, "<scan cache>"))
}

var (
	allowGitMu    sync.Mutex
	allowGitKnown = map[string]bool{}
)

// npmKnowsAllowGit is whether the npm on PATH has the allow-git setting,
// asked once per npm binary with an empty directory as its project, so no
// release's .npmrc answers for it. An npm that cannot be asked is let through:
// the resolve itself then fails and says why.
func npmKnowsAllowGit() bool {
	bin, err := exec.LookPath("npm")
	if err != nil {
		return true
	}
	allowGitMu.Lock()
	defer allowGitMu.Unlock()
	if known, ok := allowGitKnown[bin]; ok {
		return known
	}
	empty, err := os.MkdirTemp("", "toft-npm-probe-")
	if err != nil {
		return true
	}
	defer os.RemoveAll(empty)
	out, err := output(context.Background(), empty, "npm", "config", "get", "allow-git")
	if err != nil {
		return true
	}
	known := strings.TrimSpace(out) != "undefined"
	allowGitKnown[bin] = known
	return known
}

// MaxRecordedName is the longest package name or version an inventory row
// keeps. Real ones are far shorter; the bound is what makes "one strange
// manifest value" unable to fail the insert (LB-1).
const MaxRecordedName = 256

// Recordable is whether s can be stored as a package or version: valid UTF-8,
// no control character (NUL among them), at most MaxRecordedName bytes.
func Recordable(s string) bool {
	if len(s) > MaxRecordedName || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// unrecordableError is a manifest row whose package or version this platform
// cannot store. The name is quoted with its control characters escaped, so the
// refusal a developer reads carries no raw byte the row could not.
type unrecordableError struct{ pkg, ver string }

func (e *unrecordableError) Error() string {
	return fmt.Sprintf("names a package or version this platform cannot record (%s %s)",
		strconv.Quote(clipName(e.pkg)), strconv.Quote(clipName(e.ver)))
}

func (e *unrecordableError) Remedy() string {
	return "spell every package name and version as plain text of at most 256 bytes"
}

func clipName(s string) string {
	if len(s) > 80 {
		return s[:80] + "\u2026"
	}
	return s
}

// manifestOf is the file an ecosystem's rows were read from, as Resolve names
// it in an UnreadProblem.
func manifestOf(dir, ecosystem string) string {
	switch ecosystem {
	case advisory.EcosystemNPM:
		return firstPresent(dir, "package-lock.json", "package.json")
	case advisory.EcosystemPyPI:
		return firstPresent(dir, "uv.lock", "requirements.txt", "pyproject.toml")
	case advisory.EcosystemGo:
		return "go.mod"
	case advisory.EcosystemCRAN:
		return "renv.lock"
	}
	return "a dependency manifest"
}

// uvCompile is every argv the scan hands uv: `pip compile` with the given
// arguments, always --no-config and always --python (security review
// 2026-11-14, DG-1/DG-2).
//
// **The release chooses neither the configuration nor the interpreter.** uv
// runs in the release directory, as the platform. Without --no-config it reads
// the app's uv.toml and `[tool.uv]` tables, and options there such as
// no-emit-package shape what the compile writes without changing what the
// install installs, so the judged set stops being the installed set. Without
// --python it discovers an interpreter itself — one a uv.toml names, or a
// committed .venv — and executes it to query it, which is app-chosen code
// running as the platform. Each flag closes a hole the other leaves:
// --no-config does not stop .venv discovery, and --python leaves the rest of
// the configuration applied. The install side sets UV_NO_CONFIG=1 for the same
// reason, in the same change.
func uvCompile(python string, args ...string) []string {
	out := append([]string{"pip", "compile", "--no-config"}, args...)
	return append(out, "--python", python)
}

func exists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, rel))
	return err == nil
}

// MergeDuplicates folds rows that name one package at one version in one
// ecosystem into a single row: direct if any was, dev only if all were, the
// shortest path, the first source. The inventory's key is that triple, and a
// lockfile legitimately names it twice — the same version hoisted to two
// places in a tree — but Postgres refuses a multi-row INSERT whose rows
// collide on the arbiter key ("ON CONFLICT DO UPDATE command cannot affect
// row a second time"), which lost the whole inventory and passed the release
// as scanned with nothing in it (code review 2026-11-11, G-2). Order is kept.
func MergeDuplicates(list []Dependency) []Dependency {
	type key struct{ eco, pkg, ver string }
	index := make(map[key]int, len(list))
	out := make([]Dependency, 0, len(list))
	for _, d := range list {
		k := key{d.Ecosystem, d.Package, d.Version}
		i, seen := index[k]
		if !seen {
			index[k] = len(out)
			out = append(out, d)
			continue
		}
		m := &out[i]
		m.Direct = m.Direct || d.Direct
		m.Dev = m.Dev && d.Dev
		if d.Path != "" && (m.Path == "" || len(d.Path) < len(m.Path)) {
			m.Path = d.Path
		}
	}
	return out
}

func sortDependencies(d []Dependency) {
	sort.Slice(d, func(i, j int) bool {
		if d[i].Ecosystem != d[j].Ecosystem {
			return d[i].Ecosystem < d[j].Ecosystem
		}
		if d[i].Package != d[j].Package {
			return d[i].Package < d[j].Package
		}
		return d[i].Version < d[j].Version
	})
}

// ── npm ──────────────────────────────────────────────────────────────────

// npmLock is the v2/v3 lockfile shape. v1's `dependencies` tree is not read:
// npm has written v2 since npm 7 (2020), and `--package-lock-only` upgrades a
// v1 file in place, so the fallback below covers it.
type npmLock struct {
	LockfileVersion int `json:"lockfileVersion"`
	Packages        map[string]struct {
		// Name is the published package when it differs from the folder: an
		// alias (`"foo": "npm:event-stream@3.3.6"` lands at node_modules/foo
		// with name event-stream). Inventoried under the folder, the row
		// matched no advisory and `block` passed it (code review 2026-11-11,
		// E-4).
		Name       string            `json:"name"`
		Version    string            `json:"version"`
		Dev        bool              `json:"dev"`
		DevOpt     bool              `json:"devOptional"`
		Link       bool              `json:"link"`
		Resolved   string            `json:"resolved"`
		Deps       map[string]string `json:"dependencies"`
		DevDeps    map[string]string `json:"devDependencies"`
		OptDeps    map[string]string `json:"optionalDependencies"`
		PeerDeps   map[string]string `json:"peerDependencies"`
		Extraneous bool              `json:"extraneous"`
	} `json:"packages"`
}

// foreignLocks are the lockfiles the other Node package managers write, in the
// order they are looked for. A release carrying one has pinned versions; they
// are simply not in the file this platform reads.
var foreignLocks = []struct{ file, tool string }{
	{"pnpm-lock.yaml", "pnpm"},
	{"yarn.lock", "Yarn"},
}

// unlockedNPM says what a release with no package-lock.json is about to have
// resolved for it: the line logged as it happens, and the note recorded beside
// it. Both reach the same build log, so the first stays terse and the second
// carries the statement worth reading twice.
//
// The two cases are not the same fact and must not read as one. An app with no
// lockfile at all has never pinned anything, and "resolved at build time" is
// the whole story. An app that locks with pnpm or Yarn **has** pinned its
// versions and is about to be installed at different ones, because every Node
// path in the product keys on package-lock.json: the note names the file that
// was passed over, or the operator is left reading "no lockfile" beside a
// lockfile.
//
// It is a note and not a refusal, deliberately. The build otherwise works, and
// turning a running app into a failed deploy is a larger decision than this
// one — whether to read pnpm-lock.yaml properly is open, and lives in
// docs/work-remaining.md.
func unlockedNPM(dir string) (line, note string) {
	for _, l := range foreignLocks {
		if !exists(dir, l.file) {
			continue
		}
		return l.file + " is not read by this platform — resolving dependency metadata from " +
				"package.json (no package code is run)",
			"This app locks its dependencies with " + l.tool + " (" + l.file + "), which this " +
				"platform does not read — it reads package-lock.json. Its versions were resolved " +
				"from package.json at build time, so they can differ from the ones " + l.file +
				" pins, and on a later build."
	}
	return "No package-lock.json — resolving dependency metadata (no package code is run)",
		"This app has no package-lock.json, so its dependency versions were " +
			"resolved at build time and can differ on a later build."
}

// declaresNPMDependencies reports whether package.json names any dependency
// of any kind. A file that cannot be read is said to — the resolver then
// says what is wrong with it.
func declaresNPMDependencies(dir string) bool {
	raw, err := toolchain.ReadRegularFile(filepath.Join(dir, "package.json"), toolchain.MaxManifestBytes)
	if err != nil {
		return true
	}
	var pkg map[string]json.RawMessage
	if json.Unmarshal(raw, &pkg) != nil {
		return true
	}
	// A workspaces root declares its members' dependencies: `npm install`
	// installs every one of them from here, so a root with `workspaces` and
	// no dependencies of its own is not "nothing to lock" (code review
	// 2026-11-11, G-3).
	if v, ok := pkg["workspaces"]; ok {
		var l []any
		var m map[string]any
		if (json.Unmarshal(v, &l) == nil && len(l) > 0) || (json.Unmarshal(v, &m) == nil && len(m) > 0) {
			return true
		}
	}
	for _, k := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies", "bundleDependencies", "bundledDependencies"} {
		v, ok := pkg[k]
		if !ok {
			continue
		}
		var m map[string]any
		if json.Unmarshal(v, &m) == nil && len(m) == 0 {
			continue
		}
		var l []any
		if json.Unmarshal(v, &l) == nil && len(l) == 0 {
			continue
		}
		return true
	}
	return false
}

// NPMLockfile is the lockfile npm installs from in dir: npm-shrinkwrap.json
// when it is present, which npm prefers, else package-lock.json — present or
// not. Every reader of the lock asks this, so the scan, the install command,
// the dependency cache key and the build log agree on one file.
func NPMLockfile(dir string) string {
	if exists(dir, "npm-shrinkwrap.json") {
		return "npm-shrinkwrap.json"
	}
	return "package-lock.json"
}

func resolveNPM(ctx context.Context, dir string, logf Logf, gaps *[]Gap) ([]Dependency, []string, error) {
	var notes []string
	// Resolving reaches the registry with the operator's credential too, so the
	// project's .npmrc is held to the same rule the install is.
	if err := toolchain.CheckProjectNPMRC(dir); err != nil {
		return nil, notes, err
	}
	// npm-shrinkwrap.json is the same document, and when both are present
	// npm installs from it and ignores package-lock.json — so a clean
	// package-lock.json beside a shrinkwrap pinning something else was a scan
	// of one set and an install of another (code review 2026-11-11, E-3).
	lockName := NPMLockfile(dir)
	lock := filepath.Join(dir, lockName)

	if !exists(dir, lockName) && !declaresNPMDependencies(dir) {
		// Nothing to lock: a package.json that declares no dependencies is
		// not an unlocked set, and warning that it has no lockfile was noise
		// on every dependency-free [batch] Node app (BB-029, second
		// 2026-11-03 bash).
		return nil, notes, nil
	}
	if !exists(dir, lockName) {
		// No lockfile npm can read. Resolving metadata writes one and runs no
		// package code — which is the whole reason this is a supported path
		// rather than a refusal. --ignore-scripts is belt and braces:
		// --package-lock-only installs nothing, so there is nothing to run a
		// lifecycle hook.
		line, note := unlockedNPM(dir)
		logf("    %s", line)
		kind, file := GapUnlocked, ""
		for _, l := range foreignLocks {
			if exists(dir, l.file) {
				kind, file = GapForeignLock, l.file
				break
			}
		}
		gap(gaps, kind, advisory.EcosystemNPM, file)
		if err := lockNPM(ctx, dir); err != nil {
			return nil, notes, err
		}
		notes = append(notes, note)
	}

	raw, err := toolchain.ReadRegularFile(lock, toolchain.MaxManifestBytes)
	if err != nil {
		return nil, notes, err
	}
	var doc npmLock
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, notes, fmt.Errorf("%s could not be read: %w", lockName, err)
	}
	if len(doc.Packages) == 0 {
		if doc.LockfileVersion == 1 {
			return nil, notes, fmt.Errorf("%s is version 1, which this platform "+
				"does not read — run `npm install` with npm 7 or later to upgrade it", lockName)
		}
		return nil, notes, nil
	}

	// The chain from the root to each package. npm resolves a dependency by
	// walking up from the dependent's own directory, so that is what this
	// does — an approximation would attribute a package to the wrong parent,
	// and the parent is the only actionable part of a transitive finding.
	paths := npmPaths(doc)

	out := make([]Dependency, 0, len(doc.Packages))
	for path, pkg := range doc.Packages {
		if path == "" || pkg.Link || pkg.Version == "" {
			// The root entry describes the app itself; a link is a workspace
			// or a file: reference and is not a published package.
			continue
		}
		folder := npmName(path)
		if folder == "" {
			continue
		}
		// The package is what was installed, not what the folder is called.
		name := folder
		if pkg.Name != "" {
			name = pkg.Name
		}
		out = append(out, Dependency{
			Ecosystem: advisory.EcosystemNPM,
			Package:   name,
			Version:   pkg.Version,
			// Direct means *this copy* is the one the root manifest asked
			// for, which is the top-level install and not a nested duplicate.
			// A nested webpack/node_modules/lodash is webpack's, whatever the
			// app's own package.json says about a lodash — and calling it
			// direct would tell an owner to bump a version they do not control.
			// The manifest names the folder (the alias), so that is what is
			// looked up.
			Direct: path == "node_modules/"+folder && npmIsDirect(doc, folder),
			Dev:    pkg.Dev || pkg.DevOpt,
			Path:   paths[path],
		})
	}
	return out, notes, nil
}

// npmName turns a lockfile path into a package name. The name is everything
// after the *last* "node_modules/", which is what makes a nested copy of
// lodash under webpack read as lodash rather than as webpack/node_modules.
func npmName(path string) string {
	i := strings.LastIndex(path, "node_modules/")
	if i < 0 {
		return ""
	}
	return path[i+len("node_modules/"):]
}

// npmIsDirect reports whether the root package.json names this dependency.
func npmIsDirect(doc npmLock, name string) bool {
	root, ok := doc.Packages[""]
	if !ok {
		return false
	}
	for _, set := range []map[string]string{root.Deps, root.DevDeps, root.OptDeps, root.PeerDeps} {
		if _, ok := set[name]; ok {
			return true
		}
	}
	return false
}

// npmPaths walks the tree from the root, breadth-first, and records one
// representative chain per package.
//
// Breadth-first rather than depth-first on purpose: the shortest chain is the
// most actionable one, because it names the dependency closest to the app's
// own package.json — which is the one its owner can actually change.
func npmPaths(doc npmLock) map[string]string {
	// resolve implements npm's lookup: from the dependent's directory, try
	// "<dir>/node_modules/<name>", then the same from each parent directory,
	// up to the root.
	resolve := func(from, name string) (string, bool) {
		for dir := from; ; {
			candidate := "node_modules/" + name
			if dir != "" {
				candidate = dir + "/node_modules/" + name
			}
			if _, ok := doc.Packages[candidate]; ok {
				return candidate, true
			}
			if dir == "" {
				// The root scope was the last one to try.
				return "", false
			}
			// Step out to the enclosing scope. A path like
			// "node_modules/webpack" has no "/node_modules/" to cut at — its
			// enclosing scope is the root — and stopping there instead was a
			// bug that left every second-level dependency looking unreachable.
			if i := strings.LastIndex(dir, "/node_modules/"); i >= 0 {
				dir = dir[:i]
			} else {
				dir = ""
			}
		}
	}

	paths := map[string]string{"": "(this app)"}
	queue := []string{""}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		pkg := doc.Packages[cur]
		for _, set := range []map[string]string{pkg.Deps, pkg.DevDeps, pkg.OptDeps} {
			for name := range set {
				target, ok := resolve(cur, name)
				if !ok {
					continue
				}
				if _, seen := paths[target]; seen {
					continue
				}
				paths[target] = paths[cur] + " > " + name
				queue = append(queue, target)
			}
		}
	}
	// A package nothing reachable depends on still gets a name rather than an
	// empty chain: "unreachable" is a fact about the lockfile, and a blank
	// would read as "direct".
	for path := range doc.Packages {
		if path == "" {
			continue
		}
		if _, ok := paths[path]; !ok {
			paths[path] = "(not reachable from this app's manifest)"
		}
	}
	delete(paths, "")
	return paths
}

// ── PyPI ─────────────────────────────────────────────────────────────────

// uvLock is the subset of uv.lock this platform reads.
type uvLock struct {
	Package []struct {
		Name         string `toml:"name"`
		Version      string `toml:"version"`
		Dependencies []struct {
			Name string `toml:"name"`
		} `toml:"dependencies"`
	} `toml:"package"`
}

func resolvePyPI(ctx context.Context, dir, python string, earlier []string, logf Logf, gaps *[]Gap, hashed *bool) ([]Dependency, []string, error) {
	var notes []string
	if err := outsideRequirementPath(dir, "requirements.txt"); err != nil {
		return nil, notes, err
	}

	// **The judged set must be the installed set.** The build installs with
	// `uv pip install -r requirements.txt` or `uv pip install .`, and uv's pip
	// interface ignores uv.lock and resolves the declared ranges afresh. So the
	// lockfile describes what the build installs only when there is nothing
	// else to install from; otherwise it is a different answer from the one
	// that will run, and judging it would pass a release the build then
	// replaces with an unjudged one.
	hasInstallInput := exists(dir, "requirements.txt") || exists(dir, "pyproject.toml")
	if exists(dir, "uv.lock") && !hasInstallInput {
		deps, err := readUVLock(filepath.Join(dir, "uv.lock"))
		return deps, notes, err
	}

	// **A requirements.txt that pins and hashes every package is the lock**,
	// read as it stands: no resolution, no wheel check, no GapUnlocked. That is
	// safe only because the build installs exactly this file with
	// --require-hashes, which refuses anything it does not list
	// (toolchain.HashedRequirements is the one rule both ask). Without the
	// flag uv would install an unlisted dependency unhashed, and this inventory
	// would claim a closed set the build does not install.
	if exists(dir, "requirements.txt") {
		r, err := toolchain.ReadRequirements(filepath.Join(dir, "requirements.txt"))
		if err != nil {
			return nil, notes, err
		}
		if r.Hashed() {
			logf("    requirements.txt pins and hashes every package, so it is read as the lockfile")
			if hashed != nil {
				*hashed = true
			}
			return hashedRows(r), notes, nil
		}
	}

	// A requirements.txt that pins every line but is not hashed is **not** a
	// closed set: the installer does not pass --no-deps, so the transitive dependencies of what
	// it lists are resolved fresh at install time. It is resolved like any other
	// file, and read directly only as the fallback when resolution cannot run.
	var pinnedFallback []Dependency
	if exists(dir, "requirements.txt") {
		deps, pinned, err := readPinnedRequirements(filepath.Join(dir, "requirements.txt"))
		if err != nil {
			return nil, notes, err
		}
		if pinned {
			pinnedFallback = deps
		}
	}

	// No interpreter to resolve for — an install with no Python at all — is
	// not uv's to choose one: its discovery executes whatever the release
	// points it at (DG-2). What is pinned is still judged by name; anything
	// else is unresolved, like a resolution that could not run.
	if python == "" {
		logf("    This install has no Python interpreter to resolve the Python dependencies for, so they were not resolved")
		notes = append(notes, "Python dependencies could not be resolved: this install has no Python interpreter to resolve them for.")
		if pinnedFallback != nil {
			gap(gaps, GapDirectOnly, advisory.EcosystemPyPI, "requirements.txt")
			return pinnedFallback, notes, nil
		}
		if gaps != nil {
			*gaps = append(*gaps, Gap{Kind: GapUnresolved, Ecosystem: advisory.EcosystemPyPI,
				Reason: "this install has no Python interpreter to resolve them for"})
		}
		return nil, notes, nil
	}

	// Otherwise resolve. --no-build is what keeps setup.py from running; when
	// a package cannot be resolved without it, the log names the package
	// rather than the platform silently doing it.
	logf("    Resolving Python dependency metadata (no package code is run)")
	out := filepath.Join(dir, resolvedFile)
	// Removed first as well as after: an existing output file is uv's
	// preference list (LB-9). The tree is still the platform's here.
	if err := os.RemoveAll(out); err != nil {
		return nil, notes, fmt.Errorf("%s is in this repository and could not be removed "+
			"before the scan: remove it from the repository", resolvedFile)
	}
	defer os.Remove(out)

	input := "pyproject.toml"
	if exists(dir, "requirements.txt") {
		input = "requirements.txt"
	}
	// The output named relative to the release, which is uv's working
	// directory: the refusal quotes what uv said, and a server path in it
	// was the install's directory layout told to a developer (BB-039).
	args := uvCompile(python, "--no-build", "--quiet", "--upgrade", "--output-file", resolvedFile, input)
	if err := run(ctx, dir, "uv", args...); err != nil {
		err = resolverSaid(err, dir, args)
		// The honest treatment: say that resolution needed to build a package,
		// name what the resolver said, and let the operator decide. Retrying
		// without --no-build would be the platform executing unjudged package
		// code on the strength of its own convenience.
		logf("    Could not resolve without building a source distribution: %v", err)
		// **A wheel missing for this Python is refused, not passed over** —
		// the backtrack's rule (ADR §39), for the case where uv has nothing
		// to walk back to: pyyaml==5.3 publishes only an sdist, so listing
		// fails outright, and the build builds it and installs a set nobody
		// checked. Under `block` that deployed with a warning while the same
		// missing wheel one step removed was refused (BB-004, third
		// 2026-11-03 list). Any other failure stays the soft gap below — a
		// network error is the gate's own, and fails open.
		if noWheel, ok := missingWheel(err.Error(), nil); ok {
			return nil, notes, &NoWheelError{Package: noWheel, Cause: oneLine(err.Error()),
				EarlierLine: earlierLineWithWheel(ctx, dir, input, python, noWheel, earlier)}
		}
		// Only the network's failure is the gate's own. A failure the app's
		// files produce — a source distribution that needs building, a VCS
		// reference, a constraint nothing satisfies — is the manifest not
		// read, which `block` refuses like any other (code review 2026-11-11,
		// G-4); it used to be a soft gap that deployed the whole file unjudged
		// while `uv pip install` built and installed all of it.
		if !networkFailure(err.Error()) {
			return nil, notes, &UnreadError{Problems: []UnreadProblem{{Ecosystem: advisory.EcosystemPyPI,
				File: input, Err: fmt.Errorf("uv could not resolve it without building a package: %s", oneLine(err.Error())),
				Remedy: "pin the package to a version that publishes a wheel for this Python, or remove the URL or path reference; the resolver builds nothing"}}}
		}
		checked := "so they were not checked"
		if pinnedFallback != nil {
			checked = "so only the packages requirements.txt names were checked"
		}
		notes = append(notes, "Python dependencies could not be listed without building a "+
			"source distribution, "+checked+". The resolver said: "+oneLine(err.Error()))
		if pinnedFallback != nil {
			// Only what the file names: its transitive dependencies were not
			// judged, and the note above says so.
			gap(gaps, GapDirectOnly, advisory.EcosystemPyPI, "requirements.txt")
			return pinnedFallback, notes, nil
		}
		if gaps != nil {
			*gaps = append(*gaps, Gap{Kind: GapUnresolved, Ecosystem: advisory.EcosystemPyPI,
				Reason: "listing them needs a source distribution built, and the scan does not " +
					"build packages, because building runs their code"})
		}
		return nil, notes, nil
	}
	compiled, err := toolchain.ReadRequirements(out)
	if err != nil {
		return nil, notes, err
	}
	deps := pinnedRows(compiled)
	// A requirement written as a direct reference — `name @ https://…/x.whl`,
	// a local path — compiles to the same line, which the parser files under
	// Options and no row records. It is installed all the same, so it is a
	// gap by name, not a silent omission (code review 2026-11-11, E-5).
	for _, o := range compiled.Options {
		if strings.HasPrefix(o, "-") {
			continue
		}
		logf("    %s is a direct reference, which names no index release: it is installed but cannot be checked", oneLine(o))
		if gaps != nil {
			*gaps = append(*gaps, Gap{Kind: GapUnresolved, Ecosystem: advisory.EcosystemPyPI,
				File: "requirements.txt", Reason: "it installs " + oneLine(o) + " from a URL or path, which names no index release"})
		}
	}
	// Nothing resolved is nothing a later build can resolve differently: an
	// empty requirements.txt is not an unlocked dependency set.
	if len(deps) == 0 {
		return deps, notes, nil
	}
	// A resolution that could not build backtracks rather than failing, so
	// the set above can be one the build will never install. Judging it would
	// be judging the wrong thing; the error says so instead.
	if err := wheelBacktrack(ctx, dir, input, python, deps); err != nil {
		logf("    %v", err)
		return nil, notes, err
	}
	gap(gaps, GapUnlocked, advisory.EcosystemPyPI, "")
	notes = append(notes, "This app's Python dependencies are resolved when it is built, so the versions "+
		"recorded here were resolved now and can differ on a later build.")
	// Mark the ones the app names itself, and each one's chain, from the
	// resolver's own `# via` — so a finding says whose it is and which of the
	// app's choices to bump (D-09). A resolution without annotations falls
	// back to the names requirements.txt pins.
	if r, err := toolchain.ReadRequirements(out); err == nil && r.Annotated() {
		direct, paths := pypiChains(r)
		for i := range deps {
			deps[i].Direct = direct[deps[i].Package]
			deps[i].Path = paths[deps[i].Package]
		}
		return deps, notes, nil
	}
	if exists(dir, "requirements.txt") {
		direct, _, err := readPinnedRequirements(filepath.Join(dir, "requirements.txt"))
		if err == nil {
			named := map[string]bool{}
			for _, d := range direct {
				named[d.Package] = true
			}
			for i := range deps {
				deps[i].Direct = named[deps[i].Package]
			}
		}
	}
	return deps, notes, nil
}

// resolvedFile is the --no-build resolution's output, inside the release.
//
// **An --output-file that already exists is a preference**: uv keeps the
// versions it names unless --upgrade is given, so a `.toft-resolved.txt`
// committed to the repository steered every ranged requirement to an older
// version than the build's `uv pip install` takes. So the file is removed
// before the compile, and the compile carries --upgrade whatever the removal
// did (security review 2026-11-15, LB-9).
const resolvedFile = ".toft-resolved.txt"

// resolverSaid is uv's refusal without the command line in front of it: the
// arguments name the interpreter's path and the release directory, which are
// the install's and say nothing a developer can act on.
func resolverSaid(err error, dir string, args []string) error {
	msg := strings.TrimPrefix(err.Error(), "uv "+strings.Join(args, " ")+": ")
	msg = strings.ReplaceAll(msg, dir+string(filepath.Separator), "")
	return errors.New(msg)
}

// earlierLineWithWheel is the first of the earlier lines a --no-build
// resolution succeeds on for Linux, or "" when none does — when the package
// publishes no wheel this install could use on any line it offers. Asked
// only on the refusal's path, one resolution per line, none running package
// code.
func earlierLineWithWheel(ctx context.Context, dir, input, python, pkg string, earlier []string) string {
	for _, line := range earlier {
		_, err := output(ctx, dir, "uv", uvCompile(python, "--no-build", "--quiet", input,
			"--python-version", line, "--python-platform", "linux")...)
		if err == nil {
			return line
		}
		if name, ok := missingWheel(err.Error(), nil); !ok || name != pkg {
			// Some other failure: not evidence against the line.
			return line
		}
	}
	return ""
}

// NoWheelError is a --no-build resolution that failed outright because a
// package it needs publishes no wheel for this Python — a pin on an sdist-only
// release, or a package with no release this Python has a wheel for.
type NoWheelError struct {
	// Package is the one uv named, or "" when it named none; Cause is uv's
	// refusal on one line.
	Package, Cause string
	// EarlierLine is an earlier Python line this install offers that the
	// package publishes a wheel for, or "" when there is none to suggest.
	EarlierLine string
}

func (e *NoWheelError) who() string {
	if e.Package != "" {
		return e.Package
	}
	return "a package it needs"
}

func (e *NoWheelError) Error() string {
	return "the Python packages could not be listed without building: " + e.who() +
		" publishes no wheel for this Python, and the scan does not build packages to read " +
		"their metadata, because building runs their code. The resolver said: " + e.Cause
}

// GapReason is the summary's account of it.
func (e *NoWheelError) GapReason() string {
	return e.who() + " publishes no wheel for this Python, so the packages the build installs could not be listed"
}

// Remedy is the way forward, for the refusal.
func (e *NoWheelError) Remedy() string {
	who := "the package without one"
	if e.Package != "" {
		who = e.Package
	}
	if e.EarlierLine == "" {
		// No line this install offers has a wheel for it, so a different
		// Python is no way out (BB-039).
		return "pin a release of " + who + " that publishes a wheel for Linux, or wait for " +
			who + " to publish one"
	}
	return "pin a release of " + who + " that publishes a wheel for this Python, or declare an earlier " +
		"Python line under [runtime] in toft.toml — python = \"" + e.EarlierLine + "\" has a wheel for " + who
}

// WheelBacktrackError is a --no-build resolution that chose an older version
// of a package than the build will install, because something the newer one
// needs publishes no wheel for this Python. The build builds that package from
// source and installs the newer set, so the set listed here is not the set that
// runs.
type WheelBacktrackError struct {
	// Package is what --no-build chose lower, at Listed; Available is the
	// version the build would reach for instead.
	Package, Listed, Available string
	// NoWheel is the package uv named as having no usable wheel, when it named
	// one.
	NoWheel string
}

func (e *WheelBacktrackError) Error() string {
	why := "a package it needs publishes no wheel for this Python"
	if e.NoWheel != "" {
		why = e.NoWheel + " publishes no wheel for this Python"
	}
	return fmt.Sprintf("the Python packages could not be listed as the build installs them: %s, "+
		"so listing without building chose %s %s where the build installs %s %s or later — "+
		"the listed set is not the one that runs, and was not checked",
		why, e.Package, e.Listed, e.Package, e.Available)
}

// GapReason is the summary's account of it: the file was read, and the set
// that could be listed is not the one the build installs.
func (e *WheelBacktrackError) GapReason() string {
	why := "a package it needs publishes no wheel for this Python"
	if e.NoWheel != "" {
		why = e.NoWheel + " publishes no wheel for this Python"
	}
	return why + ", so the packages that could be listed without building (" + e.Package + " " +
		e.Listed + ") are not the ones the build installs (" + e.Package + " " + e.Available + " or later)"
}

// Remedy is the way forward, for the refusal. Pinning the listed version makes
// the build install what the scan listed; declaring the Python line before
// the newest is the other way, because the scan resolves against the
// release's declared interpreter (Builder.scanPython) and a new Python is
// exactly when wheels are missing — pyyaml on 3.15, which refused every
// marimo app on the default (BB-012); waiting is the third.
func (e *WheelBacktrackError) Remedy() string {
	who := "the package without one"
	if e.NoWheel != "" {
		who = e.NoWheel
	}
	return "pin " + e.Package + "==" + e.Listed + " (the newest the scan can list without building — an " +
		"older release, checked against the advisories like any other), " +
		"declare an earlier Python line under [runtime] in toft.toml, one " + who + " publishes wheels for, " +
		"or wait for " + who + " to publish a wheel for this Python — the scan does not build " +
		"packages to read their metadata, because building runs their code"
}

// wheelBacktrack finds a --no-build resolution that judged a different set from
// the one the build installs.
//
// **uv backtracks rather than failing.** When marimo 0.25.2 needs pyyaml and
// pyyaml has no wheel for this Python, `uv pip compile --no-build` quietly
// walks back to marimo 0.6.0, whose dependencies do have wheels — and
// `uv pip install`, which may build, installs 0.25.2. Nothing in the resolver's
// output says it happened, so it is asked in two more steps, neither of which
// runs package code:
//
//  1. The highest version of every listed package, alone: `--no-deps`, which
//     reads the index and fetches no distribution, so it sees sdist-only
//     releases without building them. A package listed below that is one the
//     resolver walked back from.
//  2. For each of those, the same --no-build resolution with the package held
//     at that version or later. A failure that names a missing wheel is the
//     case above; a plain conflict is a real upper bound the build would meet
//     too, and the listed version stands.
func wheelBacktrack(ctx context.Context, dir, input, python string, listed []Dependency) error {
	tmp, err := os.MkdirTemp("", "toft-wheels-")
	if err != nil {
		return fmt.Errorf("the Python package list could not be confirmed: %w", err)
	}
	defer os.RemoveAll(tmp)

	names := make([]string, 0, len(listed))
	for _, d := range listed {
		names = append(names, d.Package)
	}
	namesFile := filepath.Join(tmp, "names.txt")
	highestFile := filepath.Join(tmp, "highest.txt")
	if err := os.WriteFile(namesFile, []byte(strings.Join(names, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("the Python package list could not be confirmed: %w", err)
	}
	if err := run(ctx, dir, "uv", uvCompile(python, "--no-deps", "--quiet",
		"--output-file", highestFile, namesFile)...); err != nil {
		// Unconfirmed is not confirmed: the listed set may be the wrong one,
		// and judging it anyway is the failure this function exists to stop.
		return fmt.Errorf("the Python package list could not be confirmed against the newest "+
			"versions available, so it was not checked: %s", oneLine(err.Error()))
	}
	highest, _, err := readPinnedRequirements(highestFile)
	if err != nil {
		return fmt.Errorf("the Python package list could not be confirmed: %w", err)
	}
	top := map[string]string{}
	for _, d := range highest {
		top[d.Package] = d.Version
	}
	// newest answers a package the listed set does not hold — pyyaml, which
	// the backtracked marimo 0.6.0 never needed — with one more --no-deps
	// read of the index, which fetches no distribution.
	newest := func(name string) string {
		if v, ok := top[name]; ok {
			return v
		}
		f := filepath.Join(tmp, "one.txt")
		if os.WriteFile(f, []byte(name+"\n"), 0o600) != nil {
			return ""
		}
		out := filepath.Join(tmp, "one-out.txt")
		if run(ctx, dir, "uv", uvCompile(python, "--no-deps", "--quiet",
			"--output-file", out, f)...) != nil {
			return ""
		}
		got, _, err := readPinnedRequirements(out)
		if err != nil {
			return ""
		}
		for _, d := range got {
			if d.Package == name {
				top[name] = d.Version
				return d.Version
			}
		}
		return ""
	}

	for _, d := range listed {
		v, ok := top[d.Package]
		if !ok || v == d.Version {
			continue
		}
		constraint := filepath.Join(tmp, "constraint.txt")
		if err := os.WriteFile(constraint, []byte(d.Package+">="+v+"\n"), 0o600); err != nil {
			return fmt.Errorf("the Python package list could not be confirmed: %w", err)
		}
		err := run(ctx, dir, "uv", uvCompile(python, "--no-build", "--quiet",
			"--output-file", filepath.Join(tmp, "held.txt"), "--constraint", constraint, input)...)
		if err == nil {
			// Held higher, it still resolves without building: the lower pick
			// was the resolver's preference, not a missing wheel. Nothing here
			// prefers lower, so this is not expected, and it is not the case
			// this function refuses.
			continue
		}
		if noWheel, ok := missingWheel(err.Error(), newest); ok {
			return &WheelBacktrackError{Package: d.Package, Listed: d.Version, Available: v, NoWheel: noWheel}
		}
	}
	return nil
}

// missingWheel reads uv's refusal for whether a package lacked a usable wheel,
// and which one it named.
//
// **Only a missing wheel on a package's newest release decides.** uv appends
// "hint: Wheels are required for `marimo` …" to *every* --no-build refusal, a
// plain version conflict included, and its derivation names every old release
// without a wheel on the way to the conflict: marimo 0.25.2 caps
// pymdown-extensions below 12, so holding it at 12.3 is refused — and the
// cause says "marimo==0.13.12 has no usable wheels" in passing, about a
// release nobody would install. Read as a missing wheel, that refused every
// marimo app on Python 3.13, the pin the remedy itself recommends (BB-011 and
// BB-005, 2026-11-03). What a new Python actually does is leave a package's
// *current* release without a wheel — "pyyaml>=6.0.1 has no usable wheels" —
// so a clause counts only when the range it names holds that package's newest
// version, which newest answers ("" when unknown, which counts: unconfirmed
// is not confirmed).
func missingWheel(msg string, newest func(string) string) (string, bool) {
	var cause []string
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "hint:") {
			continue
		}
		cause = append(cause, line)
	}
	clauses, found := wheellessClauses(cause)
	if !found {
		return "", false
	}
	if len(clauses) == 0 {
		// uv said a wheel was missing in a shape this does not parse: the
		// refusal stands, under the hint's name.
		return hintedWheel(msg), true
	}
	for _, c := range clauses {
		v := ""
		if newest != nil {
			v = newest(c.name)
		}
		if v == "" {
			return c.name, true
		}
		if allowed, ok := toolchain.PEP440Allows(c.spec, v); !ok || allowed {
			return c.name, true
		}
	}
	return "", false
}

// wheelless is one "<package><range> has no usable wheels" in uv's cause.
type wheelless struct{ name, spec string }

// wheellessClauses lists the ranges uv's cause says have no wheels, and
// whether it said so at all. Two shapes: "Because pyyaml>=6.0.1 has no usable
// wheels and …", the range being the word before "has", and an "all of:" list
// whose lines are ranges, closed by a line that opens "have no usable wheels".
func wheellessClauses(cause []string) ([]wheelless, bool) {
	var out []wheelless
	found := false
	for i, line := range cause {
		at := -1
		for _, marker := range []string{" no usable wheels", " no wheels"} {
			if j := strings.Index(" "+line, marker); j >= 0 && (at < 0 || j < at) {
				at = j
			}
		}
		if at < 0 {
			continue
		}
		found = true
		pre := strings.Fields(line[:max(at-1, 0)])
		if n := len(pre); n > 0 && (pre[n-1] == "has" || pre[n-1] == "have") {
			pre = pre[:n-1]
		}
		if len(pre) > 0 {
			if c, ok := parseWheelless(pre[len(pre)-1]); ok {
				out = append(out, c)
			}
			continue
		}
		for k := i - 1; k >= 0; k-- {
			l := strings.TrimSpace(cause[k])
			if strings.HasSuffix(l, "all of:") {
				break
			}
			if c, ok := parseWheelless(l); ok {
				out = append(out, c)
			}
		}
	}
	return out, found
}

// parseWheelless splits "marimo>=0.1.82,<=0.1.83" into its name and range.
func parseWheelless(word string) (wheelless, bool) {
	word = strings.TrimRight(word, ",.;:")
	if i := strings.IndexByte(word, '{'); i >= 0 {
		word = word[:i] // an environment marker
	}
	i := strings.IndexAny(word, "<>=!~")
	name, spec := word, ""
	if i >= 0 {
		name, spec = word[:i], word[i:]
	}
	if name == "" || strings.ContainsAny(name, " `'\"()") {
		return wheelless{}, false
	}
	return wheelless{name: advisory.NormalizePackage(advisory.EcosystemPyPI, name), spec: spec}, true
}

// hintedWheel is the package uv's hint names, or "".
func hintedWheel(msg string) string {
	const marker = "Wheels are required for `"
	if i := strings.Index(msg, marker); i >= 0 {
		rest := msg[i+len(marker):]
		if j := strings.IndexByte(rest, '`'); j > 0 {
			return advisory.NormalizePackage(advisory.EcosystemPyPI, rest[:j])
		}
	}
	return ""
}

func readUVLock(path string) ([]Dependency, error) {
	var doc uvLock
	if err := decodeTOML(path, &doc); err != nil {
		return nil, fmt.Errorf("uv.lock could not be read: %w", err)
	}
	// The lock names every package and what each depends on, so one
	// representative chain is a walk over the same graph npm's is.
	depsOf := map[string][]string{}
	version := map[string]string{}
	for _, p := range doc.Package {
		name := advisory.NormalizePackage(advisory.EcosystemPyPI, p.Name)
		if name == "" {
			continue
		}
		version[name] = p.Version
		for _, d := range p.Dependencies {
			depsOf[name] = append(depsOf[name],
				advisory.NormalizePackage(advisory.EcosystemPyPI, d.Name))
		}
	}
	// A root is anything nothing else depends on, which for a uv lock is the
	// project itself and its direct requirements.
	depended := map[string]bool{}
	for _, list := range depsOf {
		for _, d := range list {
			depended[d] = true
		}
	}

	paths := map[string]string{}
	var queue []string
	for name := range version {
		if !depended[name] {
			paths[name] = "(this app) > " + name
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)
	direct := map[string]bool{}
	for _, name := range queue {
		direct[name] = true
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range depsOf[cur] {
			if _, seen := paths[d]; seen || version[d] == "" {
				continue
			}
			paths[d] = paths[cur] + " > " + d
			queue = append(queue, d)
		}
	}

	out := make([]Dependency, 0, len(version))
	for name, v := range version {
		if v == "" {
			continue
		}
		out = append(out, Dependency{
			Ecosystem: advisory.EcosystemPyPI,
			Package:   name,
			Version:   v,
			Direct:    direct[name],
			Path:      paths[name],
		})
	}
	return out, nil
}

// readPinnedRequirements reads `name==version` lines, and reports whether the
// file was *fully* pinned — an unpinned line means the file is a request
// rather than an answer, and the answer has to be resolved. It reads through
// toolchain's reader, the one the build's lock rule uses, so a `--hash` on a
// continuation line is decoration on its pin and not a directive.
func readPinnedRequirements(path string) ([]Dependency, bool, error) {
	r, err := toolchain.ReadRequirements(path)
	if err != nil {
		return nil, false, err
	}
	out := pinnedRows(r)
	return out, r.Pinned() && len(out) > 0, nil
}

// hashedRows is a hashed requirements.txt as the inventory. **Direct** follows
// the compile tools' `# via`: a row is the app's own choice when its via names
// a file — `-r requirements.in`, a bare `requirements.in`, `app
// (pyproject.toml)` — and a dependency of something else otherwise. A file
// with no annotations at all is hand-written, and lists what its author chose.
func hashedRows(r toolchain.Requirements) []Dependency {
	out := pinnedRows(r)
	if !r.Annotated() {
		return out
	}
	direct, paths := pypiChains(r)
	for i := range out {
		out[i].Direct, out[i].Path = direct[out[i].Package], paths[out[i].Package]
	}
	return out
}

// pypiChains reads a compiled requirements file's `# via` graph: which
// packages the app chose (a via naming a file), and the shortest chain from
// the app to each package, in npm's spelling ("(this app) > flask > jinja2").
func pypiChains(r toolchain.Requirements) (map[string]bool, map[string]string) {
	direct := map[string]bool{}
	children := map[string][]string{}
	for _, req := range r.Requirements {
		name := advisory.NormalizePackage(advisory.EcosystemPyPI, req.Name)
		for _, v := range req.Via {
			if viaNamesFile(v) {
				direct[name] = true
				continue
			}
			parent := advisory.NormalizePackage(advisory.EcosystemPyPI, strings.TrimSpace(v))
			children[parent] = append(children[parent], name)
		}
	}
	paths := map[string]string{}
	var queue []string
	for _, req := range r.Requirements {
		name := advisory.NormalizePackage(advisory.EcosystemPyPI, req.Name)
		if direct[name] && paths[name] == "" {
			paths[name] = "(this app) > " + name
			queue = append(queue, name)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		kids := children[cur]
		sort.Strings(kids)
		for _, k := range kids {
			if paths[k] == "" {
				paths[k] = paths[cur] + " > " + k
				queue = append(queue, k)
			}
		}
	}
	return direct, paths
}

// viaNamesFile is a `# via` entry naming a file rather than a package. A
// constraint file (`-c`) limits versions and chooses nothing.
func viaNamesFile(v string) bool {
	if strings.HasPrefix(v, "-c ") {
		return false
	}
	if rest, ok := strings.CutPrefix(v, "-r "); ok {
		return strings.TrimSpace(rest) != ""
	}
	if strings.HasSuffix(v, ")") && strings.Contains(v, "(") {
		return true // `app (pyproject.toml)`, uv's spelling for a project
	}
	for _, ext := range []string{".in", ".txt", ".toml", ".cfg", ".py"} {
		if strings.HasSuffix(v, ext) {
			return true
		}
	}
	return false
}

// pinnedRows is a requirements file's exactly pinned requirements as rows,
// each direct; a caller that knows better says so.
func pinnedRows(r toolchain.Requirements) []Dependency {
	var out []Dependency
	for _, req := range r.Requirements {
		name := advisory.NormalizePackage(advisory.EcosystemPyPI, req.Name)
		if name == "" || req.Version == "" {
			continue
		}
		out = append(out, Dependency{
			Ecosystem: advisory.EcosystemPyPI,
			Package:   name,
			Version:   req.Version,
			Direct:    true,
		})
	}
	return out
}

// ── Go ───────────────────────────────────────────────────────────────────

type goModule struct {
	Path     string    `json:"Path"`
	Version  string    `json:"Version"`
	Main     bool      `json:"Main"`
	Indirect bool      `json:"Indirect"`
	Replace  *goModule `json:"Replace"`
}

func resolveGo(ctx context.Context, dir string, logf Logf, gaps *[]Gap) ([]Dependency, []string, error) {
	var notes []string
	logf("    Resolving the Go build list (no package code is run)")

	// The build list as the compiler will actually see it, which is minimal
	// version selection's answer and not the same as reading go.mod's require
	// blocks. `-mod=readonly` keeps this from editing the app's go.mod as a
	// side effect of scanning it.
	out, err := output(ctx, dir, "go", "list", "-m", "-mod=readonly", "-json", "all")
	if err != nil {
		// The module's own fault — a go.sum missing entries the build does
		// not need, a go.mod that will not parse — is the manifest not read,
		// refused under `block` (G-4); `go build` succeeds on a go.sum that
		// `go list -m all` refuses, so a soft gap here was a way past the gate.
		// A proxy or network failure stays the gate's own.
		if !networkFailure(err.Error()) {
			return nil, notes, &UnreadError{Problems: []UnreadProblem{{Ecosystem: advisory.EcosystemGo,
				File: "go.mod", Err: fmt.Errorf("the Go build list could not be resolved: %s", oneLine(err.Error())),
				Remedy: "run `go mod tidy` and commit go.mod and go.sum"}}}
		}
		notes = append(notes, "The Go build list could not be resolved, so Go dependencies were "+
			"not checked: "+oneLine(err.Error()))
		gap(gaps, GapUnresolved, advisory.EcosystemGo, "")
		return nil, notes, nil
	}

	var deps []Dependency
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var m goModule
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m.Main {
			// The app's own module.
			continue
		}
		// A replace directive is what actually gets built, so it is what gets
		// judged. Judging the replaced module would report a finding against
		// code this build does not contain.
		effective := m
		if m.Replace != nil {
			effective = *m.Replace
		}
		if effective.Path == "" || effective.Version == "" {
			// A filesystem replace has no version. Nothing published, nothing
			// an advisory can be about.
			continue
		}
		deps = append(deps, Dependency{
			Ecosystem: advisory.EcosystemGo,
			Package:   effective.Path,
			Version:   effective.Version,
			Direct:    !m.Indirect,
		})
	}
	notes = append(notes, "Go findings are module-level: a module with an advisory is reported "+
		"whether or not this app calls the affected code.")
	return deps, notes, nil
}

// ── running commands ─────────────────────────────────────────────────────

// run executes a resolution command with a clean environment.
//
// The environment is deliberately *not* the app's: resolution reads public
// package metadata and has no business holding the app's secrets, and
// inheriting os.Environ() is the thing every child process on this platform is
// forbidden from doing.
func run(ctx context.Context, dir, name string, args ...string) error {
	_, err := output(ctx, dir, name, args...)
	return err
}

func output(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = resolveEnv()
	// The timeout ends the whole tree, and a grandchild that still holds the
	// pipes after it cannot keep Run waiting past waitDelay either.
	ownGroup(cmd)
	cmd.WaitDelay = waitDelay
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// resolveEnv is the allow-list a resolution command runs under. PATH and HOME
// because the tools need them; the caches so a resolve is not a cold download
// every time; nothing else, and specifically none of the app's own
// environment.
//
// The toolchain and proxy half of it is not written here. It comes from
// toolchain.PassthroughSlice, which is the same list internal/builder passes to
// the commands that *build* the tree these commands are reading — see that
// package for why the two used to differ and what it cost.
func resolveEnv() []string {
	// UV_NO_CONFIG as well as --no-config on every argv: no uv.toml — the
	// app's, the user's or /etc/uv's — shapes what the scan judges (DG-1).
	// GOENV=off: no go environment file — on an install with no accounts the
	// builds share the platform's HOME, and a build could shape a later scan
	// (LB-10).
	env := []string{"PATH=" + os.Getenv("PATH"), "UV_NO_CONFIG=1", "GOENV=off"}
	// No npm_config_cache: the scan's npm is given its own cache on the argv,
	// and the environment must not disagree with it (LB-3).
	for _, key := range []string{"HOME", "TMPDIR", "UV_CACHE_DIR"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return toolchain.PassthroughSlice(env)
}

// networkFailure reads a resolver's message for the signs of a failure to
// reach an index, proxy or registry — the one kind of failure that is this
// install's rather than the app's, and so stays a soft gap (G-4).
//
// **It never reads a name the release supplied** (security review
// 2026-11-15, LB-2). The message quotes the app: Go names the module
// (`go: github.com/gin-contrib/timeout@v1.1.0: missing go.sum entry…`), uv
// quotes the requirement in backticks, and either can carry a URL the app
// wrote. So those spans are removed first, and every sign left is a phrase
// with a space in it — something no module path, package name or URL can
// spell. A single word ("proxy", "timeout", "eof") turned an unreadable
// manifest into an allowed, unscanned release under `block`.
func networkFailure(msg string) bool {
	m := strings.ToLower(withoutAppNames(msg))
	for _, sign := range networkSigns {
		if strings.Contains(m, sign) {
			return true
		}
	}
	return false
}

var networkSigns = []string{
	"dial tcp", "connection refused", "connection reset", "no such host", "timed out",
	"network is unreachable", "temporary failure in name resolution", "could not connect",
	"error sending request", "failed to fetch",
	"i/o timeout", "tls handshake timeout", "operation timed out", "unexpected eof",
	"proxyconnect tcp", "certificate signed by", "certificate verify failed",
	"invalid peer certificate", "503 service unavailable", "502 bad gateway",
	"504 gateway timeout", "429 too many requests",
}

var (
	// `…` — uv's quoting of the requirement it could not build; "…" — Go's
	// quoting of a path or URL.
	backtickSpan = regexp.MustCompile("`[^`]*`")
	quotedSpan   = regexp.MustCompile(`"[^"\n]*"`)
	// Go's suggestion line names the module again.
	goModDownload = regexp.MustCompile(`go mod download\s+\S+`)
	// Any URL, parenthesized or bare.
	urlToken = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s)]*`)
	// path@version — Go's module errors, npm's package specs.
	atVersion = regexp.MustCompile(`\S+@\S+`)
)

// withoutAppNames is msg with every span the release could have chosen
// replaced by a placeholder, so the network test reads only the tool's words.
func withoutAppNames(msg string) string {
	// A parser echoes the line it could not read and underlines it with
	// carets: the echoed line is the app's text, whatever it says.
	lines := strings.Split(msg, "\n")
	for i := 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t != "" && strings.Trim(t, "^~") == "" {
			lines[i-1] = "<name>"
		}
	}
	msg = strings.Join(lines, "\n")
	for _, re := range []*regexp.Regexp{backtickSpan, quotedSpan, goModDownload, urlToken, atVersion} {
		msg = re.ReplaceAllString(msg, "<name>")
	}
	return msg
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
