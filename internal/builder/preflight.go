package builder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/deploysteps"
	"toft/internal/jobs"
	"toft/internal/runtime"
	"toft/internal/schedule"
	"toft/internal/staticserve"
	"toft/internal/toolchain"
)

// Preflight is what a deploy of a directory would decide before it builds
// anything — the half of the deploy that is decisions rather than work.
//
// **Every answer here is the build's own function, called on the same
// directory.** Detection, the manifest and the runtime version are each refused
// by the deploy at a step of their own, and a check that restated any of them
// would be a second rule that drifts from the first: the check says yes, the
// deploy says no, and the reader learns to distrust the check. So this calls
// DetectFramework, ResolveNotebook, ParseManifestWithWarnings and
// resolveRuntime and reports what they returned, in the deploy's own failure
// vocabulary.
//
// **What it cannot answer is said, not implied.** The dependency gate reads a
// resolved inventory and the build is the build; neither runs here, so a clean
// preflight is "these decisions would pass", never "this will deploy".
type Preflight struct {
	Framework  string `json:"framework"`
	Language   string `json:"language,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`
	// Runtime is the interpreter the build would settle on, and the set it was
	// judged against. Absent for a static app, and when resolution refused.
	Runtime *PreflightRuntime `json:"runtime,omitempty"`
	// Jobs are the names toft.toml declares.
	Jobs []string `json:"jobs,omitempty"`
	// JobLimits is each declared job's effective timeout and memory with the
	// source of each figure, when the install's settings were in hand: what a
	// developer needs to know before a run is stopped by a limit they did not
	// know applied.
	JobLimits []PreflightJob `json:"job_limits,omitempty"`
	// Restart and Render are toft.toml's `[restart]` and `[render]` as the
	// deploy would record them; absent when the table is, or when it is
	// refused (the refusal is in Problems).
	Restart *PreflightSchedule `json:"restart,omitempty"`
	Render  *PreflightSchedule `json:"render,omitempty"`
	// Storage is toft.toml's [[storage]] planned against the app's table: the
	// four lists a deploy would write at its flip. RunPreflight has no
	// principal and no database, so it parses the entries (DeclaredStorage)
	// and the check handler plans them for the caller and the owner.
	Storage *StoragePlan `json:"storage,omitempty"`
	// Warnings are what the manifest carries that nothing reads — the same
	// sentences a deploy's result carries, one per WarningDetails entry.
	Warnings []string `json:"warnings,omitempty"`
	// WarningDetails is Warnings with a code and, for toft.toml, the table.
	WarningDetails []Warning `json:"warning_details,omitempty"`
	// Problems are the refusals a deploy would make, each with the reason key
	// the deploy would fail with.
	Problems []PreflightProblem `json:"problems,omitempty"`
	// Notes are the lines the build log would carry for these decisions.
	Notes []string `json:"notes,omitempty"`
	// NotChecked names what only a real deploy can answer.
	NotChecked []string `json:"not_checked"`

	declaredStorage []StorageEntry
	storageParsed   bool
}

// DeclaredStorage is toft.toml's [[storage]] entries, and whether the file
// parsed far enough to say — a manifest refused before the table is read has
// no plan to make.
func (p *Preflight) DeclaredStorage() ([]StorageEntry, bool) {
	return p.declaredStorage, p.storageParsed
}

// AddStoragePlan records a plan the check handler judged, its refusals as
// problems in the deploy's own vocabulary.
func (p *Preflight) AddStoragePlan(plan *StoragePlan) {
	if len(plan.Keep)+len(plan.Add)+len(plan.Change)+len(plan.Remove)+len(plan.Console)+len(plan.Problems) == 0 {
		return
	}
	p.Storage = plan
	for _, sp := range plan.Problems {
		p.Problems = append(p.Problems, PreflightProblem{
			Reason: string(deploysteps.ReasonStorageRefused), Code: sp.Code, Hint: sp.Hint, Message: sp.Message})
	}
	for _, l := range plan.Lines() {
		p.Notes = append(p.Notes, "Storage: "+l)
	}
	p.completeProblems()
}

type PreflightRuntime struct {
	Version string `json:"version,omitempty"`
	Pin     string `json:"pin,omitempty"`
	// RequiresPython is pyproject.toml's, when it chose the version because it
	// excluded the install default.
	RequiresPython string `json:"requires_python,omitempty"`
	Where          string `json:"where"`
}

// PreflightSchedule is a declared `[restart]` or `[render]`: the expression,
// its zone (empty for UTC), the stored form and the first firing.
type PreflightSchedule struct {
	Schedule  string     `json:"schedule"`
	Timezone  string     `json:"timezone,omitempty"`
	Stored    string     `json:"stored"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

func preflightSchedule(s *database.ManifestSchedule) *PreflightSchedule {
	if s == nil {
		return nil
	}
	zone, expr := schedule.Split(s.Schedule)
	return &PreflightSchedule{Schedule: expr, Timezone: zone, Stored: s.Schedule, NextRunAt: s.NextRunAt}
}

// PreflightJob is one declared job's effective limits.
type PreflightJob struct {
	Name   string      `json:"name"`
	Limits jobs.Limits `json:"limits"`
}

type PreflightProblem struct {
	Reason string `json:"reason"`
	// Code and Hint are a refusal's machine code and pasteable fix, where the
	// refusal has them (removed_key, limit_exceeds_ceiling).
	Code string `json:"code,omitempty"`
	Hint string `json:"hint,omitempty"`
	// Field is the toft.toml key the problem is about, as a dotted path —
	// storage[1].path, jobs.nightly.timeout, runtime.node — where the
	// refusal names one (D-22, fourth 2026-11-03 black-box list).
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// OK reports whether every decision a preflight can make would pass.
func (p *Preflight) OK() bool { return len(p.Problems) == 0 }

// RunPreflight runs the deploy's decisions against srcDir for a release that
// would run in mode. None of them reads a builder's state — the versions a pin
// is judged against are the host's and the declared images' — so it needs no
// Builder, and a zero one carries the method. settings is the install's, for
// the job timeout's ceiling and default; nil checks the release alone. env is
// the install's envelope, which the dispatcher and the job list resolve a
// job's memory and CPU against too, so the three cannot name a different
// figure or source.
func RunPreflight(srcDir string, mode runtime.Mode, settings *database.PlatformSettings, env config.Envelope, deployed ...database.JobDefinition) *Preflight {
	var b Builder
	p := &Preflight{NotChecked: []string{"dependencies", "build", "start"}}
	defer p.completeProblems()
	note := func(line string) { p.Notes = append(p.Notes, line) }

	warn := func(w Warning) {
		p.Warnings = append(p.Warnings, w.Message)
		p.WarningDetails = append(p.WarningDetails, w)
	}
	// Each problem the manifest has, not the first (BB-010): the deploy
	// refuses on the first, and a check that hid the rest sent its reader
	// round the loop once per key.
	manifestProblem := func(err error) {
	next:
		for _, one := range manifestProblems(err) {
			// Once each: a syntax error is met by every reader of the file —
			// the manifest's and [runtime]'s — and `node = 22` was listed
			// twice (BB-012).
			for _, have := range p.Problems {
				if have.Message == one.Error() {
					continue next
				}
			}
			pp := PreflightProblem{Reason: string(deploysteps.ReasonManifestInvalid), Message: one.Error()}
			var me ManifestError
			if errors.As(one, &me) {
				pp.Code, pp.Hint = me.CodeAndHint()
				pp.Field = me.FieldPath()
				// The deploy's sentence carries its fix, because it is all a
				// deploy carries; a check problem has the hint beside it, and
				// saying it twice read as a hint repeating the message.
				pp.Message = strings.TrimSuffix(pp.Message, " — "+pp.Hint)
			}
			p.Problems = append(p.Problems, pp)
		}
	}
	decl, unread, err := ParseManifestAll(srcDir)
	defs := decl.Jobs
	for _, key := range unread {
		warn(UnreadKeyWarning(key))
	}
	if err != nil {
		manifestProblem(err)
	} else {
		p.declaredStorage, p.storageParsed = decl.Storage, true
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	for _, j := range defs {
		p.Jobs = append(p.Jobs, j.Name)
	}

	// A malformed `[static]` table is dropped by detection, which must not
	// turn a typo into "unsupported framework" — so it is said here, or a
	// `root = "../site"` in a tree that detects as Python builds as Python
	// and the declaration vanishes without a word (BB-014).
	staticBad := false
	if _, err := staticserve.ReadDeclaration(srcDir); err != nil {
		var de staticserve.DeclarationError
		if errors.As(err, &de) {
			staticBad = true
			manifestProblem(staticDeclarationProblem(srcDir, de))
		}
	}

	result := DetectFramework(srcDir)
	p.Framework = string(result.Framework)
	p.Entrypoint = result.Entrypoint
	// A declaration that parses can still name what this tree does not hold —
	// `root = "dist"` with no dist/ committed. The deploy resolves it and
	// refuses, so the check resolves it too (M4, 2026-11-04 black-box list).
	if result.Framework == FrameworkStatic && !staticBad {
		if _, err := staticserve.Resolve(srcDir); err != nil {
			pp := PreflightProblem{Reason: string(deploysteps.ReasonManifestInvalid), Message: err.Error(),
				Code: "invalid_static"}
			var de staticserve.DeclarationError
			if errors.As(err, &de) && de.Field != "" {
				pp.Field = "static." + de.Field
				if de.Field == "root" {
					pp.Hint = "commit that directory, or point root at one this tree contains — " +
						"a static app is served as it is uploaded, with no build to produce it"
				}
			}
			p.Problems = append(p.Problems, pp)
		}
	}
	// The deploy's own refusals (checkManifest), made here too: a check
	// that passed what the deploy then refused is a check nobody trusts.
	if err := checkJobsFit(string(result.Framework), defs); err != nil {
		manifestProblem(err)
	}
	if err := checkSchedulesFit(string(result.Framework), decl); err != nil {
		manifestProblem(err)
	} else {
		p.Restart, p.Render = preflightSchedule(decl.Restart), preflightSchedule(decl.Render)
	}
	// A static app runs no jobs, so its refused [jobs] gets no limits beside
	// the refusal — install defaults for jobs that will never run read as
	// though they would (drills 2026-10-30).
	runsJobs := result.Framework != FrameworkStatic
	if settings != nil && runsJobs {
		// A declaration above a ceiling is refused, and only refused: before,
		// the same job also read `clamped` in job_limits and drew a
		// limit_clamped warning, which is the word for a ceiling lowered under
		// a job already deployed (jobs-regression #16).
		refused := map[string]*jobs.CeilingError{}
		for _, ce := range jobs.CeilingRefusals(defs, deployed, *settings) {
			refused[ce.Job] = ce
			manifestProblem(CeilingManifestError(ce))
		}
		var clampable []database.JobDefinition
		for i := range defs {
			if refused[defs[i].Name] == nil {
				clampable = append(clampable, defs[i])
			}
		}
		// One job at a time, so each warning names its table and a program
		// need not parse the job back out of the sentence (drills 2026-10-28
		// evening #4).
		for i := range clampable {
			for _, c := range jobs.ClampWarnings(clampable[i:i+1], *settings) {
				warn(Warning{Code: WarningLimitClamped, Table: "jobs." + clampable[i].Name, Message: c})
			}
		}
		for i := range defs {
			l := jobs.EffectiveLimits(&defs[i], *settings, env)
			if ce := refused[defs[i].Name]; ce != nil {
				l.TimeoutSec = jobs.Limit{Value: ce.Declared, Source: jobs.SourceDeclared}
			}
			p.JobLimits = append(p.JobLimits, PreflightJob{Name: defs[i].Name, Limits: l})
		}
	}
	unsupported := func(msg string) {
		p.Problems = append(p.Problems, PreflightProblem{Reason: string(deploysteps.ReasonUnsupportedFramework),
			Code: string(deploysteps.ReasonUnsupportedFramework), Hint: unsupportedHint(srcDir), Message: msg})
	}
	if result.Framework == FrameworkUnsupported {
		// A [static] table detection dropped for its typo is the problem
		// already said; "unsupported framework" beside it is the same typo
		// misnamed (BB-012).
		if !staticBad {
			unsupported(UnsupportedMessage(srcDir))
		}
		// [runtime] is the manifest's, whatever the tree holds: a library-only
		// Go tree with `go = "1.26"` was told only that it has no framework,
		// and invalid_runtime came after the fix (BB-058, fifth 2026-11-03
		// black-box list).
		if _, err := ParseRuntimePins(srcDir); err != nil {
			manifestProblem(err)
		}
		return p
	}
	if _, err := ResolveNotebook(srcDir); err != nil {
		// A [notebook] key with a wrong value is the manifest's problem, as
		// the deploy reports it; the rest (two notebooks, none) is what the
		// tree holds.
		notebookProblem := func(err error) {
			p.Problems = append(p.Problems, PreflightProblem{Reason: string(deploysteps.ReasonManifestInvalid),
				Code: "invalid_notebook_" + err.(NotebookError).Field, Hint: notebookHint(err), Message: err.Error()})
		}
		var ne NotebookError
		if errors.As(err, &ne) && ne.Field != "" {
			notebookProblem(ne)
			// The resolve stops at the file, so engine = "magic" beside a
			// file that is not there was never said (BB-009).
			if ne.Field != "engine" {
				if eng := unknownNotebookEngine(srcDir); eng != nil {
					notebookProblem(*eng)
				}
			}
		} else {
			unsupported(err.Error())
			// The engine is the manifest's whatever the tree holds: engine =
			// "latex" beside a missing notebook was told only the latter
			// (BB-065).
			if eng := unknownNotebookEngine(srcDir); eng != nil {
				notebookProblem(*eng)
			}
		}
		// And so is [runtime], as on the unsupported path above (BB-058,
		// BB-065) — a malformed pin, and one no interpreter here has: a
		// missing notebook's tree still names its language in [runtime], so
		// python = "3.99" was refused only once the notebook came back
		// (2026-11-03 evening drills).
		if _, err := ParseRuntimePins(srcDir); err != nil {
			manifestProblem(err)
		} else {
			for _, miss := range b.unavailablePins(srcDir, mode, note) {
				p.Problems = append(p.Problems, PreflightProblem{
					Reason: string(deploysteps.ReasonRuntimeVersion), Code: string(deploysteps.ReasonRuntimeVersion),
					Hint:    runtimeVersionHint(srcDir, miss.lang, mode),
					Message: miss.err.Error()})
			}
		}
		return p
	}
	p.Language = LanguageOf(result.Framework)

	rt, err := b.resolveRuntime(result.Framework, srcDir, mode, note)
	if err != nil {
		if _, manifest := err.(ManifestError); manifest {
			manifestProblem(err)
			return p
		}
		// No interpreter this install has satisfies the pin: runtime_version,
		// the code the CLI documents, now with a hint (BB-038).
		p.Problems = append(p.Problems, PreflightProblem{
			Reason: string(deploysteps.ReasonRuntimeVersion), Code: string(deploysteps.ReasonRuntimeVersion),
			Hint:    runtimeVersionHint(srcDir, p.Language, mode),
			Message: err.Error()})
		return p
	}
	if w, ok := rPackagesUnlocked(srcDir, p.Language, result.Framework); ok {
		warn(w)
	}
	if w, ok := pinExcludedByRequiresPython(srcDir, rt); ok {
		warn(w)
	}
	if rt != nil {
		p.Runtime = &PreflightRuntime{Version: rt.Chosen.Version, Pin: rt.Pin,
			RequiresPython: rt.RequiresPython, Where: rt.Where}
		rt.report(note)
		if w, ok := nodeVersionUnread(srcDir, rt); ok {
			warn(w)
		}
		// The check said nothing at all here, and the deploy then spent a
		// build failing to download the toolchain (docs/bugs
		// 2026-10-22-cli-sdk-drills-go #1). Still not a problem -- D5 leaves
		// the version to the go command, and GOTOOLCHAIN=auto may fetch it --
		// but said before the build rather than discovered by it.
		if w, ok := goToolchainFetched(rt); ok {
			warn(w)
		}
	}
	if result.Framework == FrameworkGo || result.Framework == FrameworkBatchGo {
		for _, w := range goWorkDirsMissing(srcDir) {
			warn(w)
		}
	}
	return p
}

// WarningGoWorkDirExcluded is a go.work `use` directory the tree does not
// hold — a git-ignored .toft/sdk-go is the usual one — which the build fails
// to load. The CLI's own check says it of a directory its ignore rules left
// out, under the same code; this is the install's, for any client (BB-028).
const WarningGoWorkDirExcluded = "go_work_dir_excluded"

// goWorkDirsMissing is one warning per directory go.work uses that the tree
// does not contain. A line scan of `use` directives, single or in a block,
// never a parse — the go command is what reads the file, and this only asks
// whether what it names arrived.
func goWorkDirsMissing(srcDir string) []Warning {
	data, err := readInRelease(srcDir, "go.work")
	if err != nil {
		return nil
	}
	var out []Warning
	inBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := raw
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		var dir string
		switch {
		case inBlock && line == ")":
			inBlock = false
			continue
		case inBlock:
			dir = line
		case line == "use (" || line == "use(":
			inBlock = true
			continue
		case strings.HasPrefix(line, "use ") || strings.HasPrefix(line, "use\t"):
			dir = strings.TrimSpace(line[len("use"):])
		default:
			continue
		}
		dir = strings.Trim(dir, "\"`")
		if dir == "" {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(dir))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			continue
		}
		if info, err := os.Stat(filepath.Join(srcDir, clean)); err == nil && info.IsDir() {
			continue
		}
		rel := filepath.ToSlash(clean)
		out = append(out, Warning{Code: WarningGoWorkDirExcluded, Table: "go.work", Path: rel, Message: "go.work uses " +
			rel + ", which this tree does not contain — the build will fail to load it. Ship the " +
			"directory (stop ignoring it), or drop it from go.work (go work edit -dropuse=./" + rel + ")"})
	}
	return out
}

// goToolchainFetched is the warning for a go.mod asking for a Go this install
// does not have: the build leaves it to the go command (D5), which fetches it
// or fails. One sentence for the check and the deploy's result, which carried
// none of it (BB-011, third 2026-11-03 list).
func goToolchainFetched(rt *resolvedRuntime) (Warning, bool) {
	if rt == nil || rt.Unsatisfied == "" {
		return Warning{}, false
	}
	return Warning{Code: WarningGoToolchainFetched, Message: fmt.Sprintf(
		"go.mod %s — the build will ask the go command to fetch the toolchain go.mod "+
			"asks for, and fails if it cannot (lower the go line in go.mod to "+
			"build with what this install has)", rt.Unsatisfied)}, true
}

// staticDeclarationProblem is a [static] refusal as a manifest problem: a value
// of the wrong type said as the manifest's own are (`fallback = "yes"` must be
// true or false), not in the decoder's Go terms under "could not be parsed".
func staticDeclarationProblem(srcDir string, de staticserve.DeclarationError) error {
	rest, ok := strings.CutPrefix(de.Msg, manifestUnparsed+": ")
	if !ok {
		rest, ok = strings.CutPrefix(de.Msg, manifestWrongType+": ")
	}
	if ok {
		data, _ := readInRelease(srcDir, ManifestFile)
		return tomlDecodeError(string(data), errors.New(rest))
	}
	return de
}

// pinMiss is one [runtime] pin no interpreter on this install satisfies.
type pinMiss struct {
	lang string
	err  error
}

// unavailablePins judges each [runtime] pin against the versions this install
// carries, without a framework to say which language the tree is: the pin
// itself names it. Go is go.mod's and never here.
func (b *Builder) unavailablePins(srcDir string, mode runtime.Mode, note func(string)) []pinMiss {
	pins, err := ParseRuntimePins(srcDir)
	if err != nil || len(pins) == 0 {
		return nil
	}
	langs := make([]string, 0, len(pins))
	for l := range pins {
		langs = append(langs, l)
	}
	var out []pinMiss
	for _, lang := range sortedStrings(langs) {
		if lang == toolchain.Go {
			continue
		}
		set, where := b.effectiveSet(lang, mode, note)
		if _, err := toolchain.Resolve(lang, pins[lang], set, where); err != nil {
			out = append(out, pinMiss{lang: lang, err: err})
		}
	}
	return out
}
