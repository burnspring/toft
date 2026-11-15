// Package toolchain answers which language versions this install has, which
// ones its isolated runner images are declared to carry, and which of them
// satisfies an app's declared range.
//
// **The set is discovered, never compiled in** (ADR §64 D7). Five things about
// this platform are compiled into the binary and must never be templated — the
// pod label, the workload UID, the unauthenticated config probe, the workload
// ServiceAccount and its token audience — and the supported language versions
// are emphatically not among them. The published image is a *base*: an
// enterprise takes it and builds their final image, and that is where Node,
// Python and Go get installed. What versions exist is therefore a property of a
// machine this project did not build.
//
// So there are two halves, and the split is the design:
//
//   - **The operator declares where.** TOFT_PYTHON_PATH,
//     TOFT_NODE_PATH and TOFT_GO_PATH are colon-separated lists of
//     interpreter paths in the image they built, stated in the environment
//     alongside every other install-level fact this product takes that way.
//   - **The probe says what.** Every path is confirmed by exec, and the answer
//     is believed over anything a variable name or a directory name claims.
//     That is why no version string appears in a variable name: it would be a
//     second place for the truth to live, free to disagree with the binary.
//
// A path that does not exist, or will not exec, stops the platform from
// starting (D11). It is the operator's own statement about their own image, and
// a typo should fail on the first boot rather than become a confusing deploy
// refusal three weeks later.
//
// The environment is read once, at process start, so **restart-to-notice** is
// the behaviour for everything that *resolves*. There is no TTL and nothing to
// explain: a probe is an exec, and two deploys a second apart must not be
// refused differently because an interpreter appeared between them.
//
// The one exception reports and decides nothing. AvailableNow re-probes a
// language whose cached answer is empty, because "Not installed" on the system
// checks page is otherwise a fact about boot being read by the one person most
// likely to have just installed the thing. It never writes the cache, so the
// set a build resolves against is still the set this process started with.
//
// This package has four readers and only one of them refuses anything:
// internal/api renders it, checkToolchains grades it, internal/runner probes
// itself with it, and internal/builder is the one that turns a range into a
// refused deploy. That is why it is a package rather than a helper in
// internal/api — a fact a deploy is refused on cannot live where only a page
// can reach it (D10).
package toolchain

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The languages this package knows. These are *language families*, the same
// unit internal/config's runner images use — not frameworks, and never per-app.
const (
	Python = "python"
	Node   = "node"
	Go     = "go"
	R      = "r"
)

// Languages is every language, in the order a person would read them. Exported
// because the admin surface and the health check both enumerate.
var Languages = []string{Python, Node, Go, R}

// Label is the language's name as a person writes it.
func Label(lang string) string {
	switch lang {
	case Python:
		return "Python"
	case Node:
		return "Node.js"
	case Go:
		return "Go"
	case R:
		return "R"
	}
	return lang
}

// Version is one interpreter or toolchain that exists somewhere.
//
// Path is empty for a *declared* version — a runner image's, which the platform
// has been told about and cannot see. Default is true for the one found on
// PATH, which is what an app with no declared range gets.
type Version struct {
	Language string `json:"language"`
	Version  string `json:"version"`
	Path     string `json:"path,omitempty"`
	Default  bool   `json:"default"`
}

// Series is the part of a version that matching happens on: the major for Node,
// major.minor for Python and Go. **Patch never matters** (D8) — nobody pins a
// Node patch, and a Python patch is a bugfix by policy.
func (v Version) Series() string { return series(v.Language, v.Version) }

func (v Version) String() string {
	if v.Path != "" {
		return v.Version + " (" + v.Path + ")"
	}
	return v.Version
}

// pathEnv names the variable that declares where an operator put a language's
// interpreters. TOFT_ because this is this product's own idea; GOPROXY and
// GOTOOLCHAIN keep Go's names because they are Go's ideas (D12).
var pathEnv = map[string]string{
	Python: "TOFT_PYTHON_PATH",
	Node:   "TOFT_NODE_PATH",
	Go:     "TOFT_GO_PATH",
	R:      "TOFT_R_PATH",
}

// DeclarationEnv is the operator's declarations of where the interpreters are,
// as KEY=VALUE for those set: paths, never secrets. A process that resolves a
// release's interpreter on this machine — the runner the shared-mode agent
// places — needs them, and is started from an allow-listed environment that
// does not carry them; without them a release pinned below the default runs
// the default.
func DeclarationEnv() []string {
	var out []string
	for _, lang := range Languages {
		if k := pathEnv[lang]; k != "" {
			if v, ok := os.LookupEnv(k); ok {
				out = append(out, k+"="+v)
			}
		}
	}
	return out
}

// runnerVersionEnv names the variable that declares what an isolated runner
// image carries. Platform-side, beside the RUNNER_IMAGE_* an operator already
// sets — because the deploy decision is made before any pod exists and cannot
// wait to be told by one (§3g).
//
// **Go is deliberately absent.** `go build` produces a binary and the workload
// execs that; Node and Python ship their interpreter into the running workload,
// and Go leaves it behind on the build host. So Go's version matters on the
// platform and is absent from the runner.
//
// R is present for the reason that is Go's inverted, and the absence would be
// silent rather than loud: an renv library holds .so files compiled against one
// R and one libc, and the interpreter ships into the running workload. Without
// the entry Configured("r") returns nil, effectiveSet falls back to the build
// host, and an isolated R app is judged against an interpreter it will never
// run under — the one case ADR §64 leaves un-refused on purpose, reached by
// omission rather than by an operator's choice.
//
// **R is also why every one of these reads a list** (D19). A binary repository
// publishes a package version only for the R series it was current in, so a
// lockfile pinned to an older R resolved against a newer one is served source
// and compiled against headers it predates — which is not a hypothetical: five
// of eight real Connect bundles failed exactly there, on `Calloc`, `Free` and
// `PI`, all removed in R 4.5. A single declared version made that unavoidable
// in isolated mode however many interpreters the image actually carried. The
// list is not R-only because the shape is not R-specific; R is just the family
// where one version is provably not enough.
var runnerVersionEnv = map[string]string{
	Python: "RUNNER_PYTHON_VERSION",
	Node:   "RUNNER_NODE_VERSION",
	R:      "RUNNER_R_VERSION",
}

// defaultBinary is what a language is called when nobody has said where it is.
var defaultBinary = map[string]string{
	Python: "python3",
	Node:   "node",
	Go:     "go",
	// Rscript, not R, and for the reason python3 rather than python: it is the
	// non-interactive front end, and it is what every launcher and build step
	// execs. A declared TOFT_R_PATH may still name either, so both
	// greetings are parsed.
	R: "Rscript",
}

// probeTimeout bounds one `--version` exec. Generous, because the first `node`
// on a cold container image can be slow, and short enough that a wedged binary
// fails the boot rather than hanging it.
var probeTimeout = 20 * time.Second // a variable only so a test can shorten it

type probeResult struct {
	versions []Version
	err      error
}

var (
	probeOnce  sync.Map // language -> *sync.Once
	probeCache sync.Map // language -> probeResult
)

// Available is every version of a language this host actually has: whatever is
// on PATH, plus every path the operator declared, each confirmed by exec and
// deduplicated on the matching unit.
//
// Highest first, so a caller wanting the best match takes the first that fits.
// An empty result is a real configuration, not an error — an isolated-only
// install has no Node, and only the deploy that wanted one can say it was
// required.
func Available(lang string) []Version {
	return probe(lang).versions
}

// AvailableNow is Available with one correction, for a caller that reports
// rather than resolves: when the cached answer is empty the probe is run again,
// now.
//
// Only the empty answer, and in one direction. A language the platform found at
// boot is not re-probed — the cost is an exec per press, and more importantly
// the cached set is what every build resolves against, so a page that could
// disagree with it in the *present* direction would be a worse lie than the one
// this fixes. A toolchain removed since boot therefore still reads as present,
// and the checks page says when the reading was taken.
//
// The result is deliberately not stored. Discovering Node here must not change
// what a deploy is refused on: that set is fixed for the life of the process by
// design, and this function exists so an operator can see the host without
// having to restart to see it.
func AvailableNow(lang string) []Version {
	if v := Available(lang); len(v) > 0 {
		return v
	}
	return probeLanguage(lang).versions
}

// Err reports why a language's declared paths could not be probed, or nil.
// Separate from Available because the two have different audiences: Available's
// caller wants the set, and Err's caller is Validate, refusing to start.
func Err(lang string) error { return probe(lang).err }

func probe(lang string) probeResult {
	o, _ := probeOnce.LoadOrStore(lang, &sync.Once{})
	o.(*sync.Once).Do(func() {
		probeCache.Store(lang, probeLanguage(lang))
	})
	r, _ := probeCache.Load(lang)
	return r.(probeResult)
}

func probeLanguage(lang string) probeResult {
	var out []Version
	seen := map[string]bool{}

	// The default first, so it keeps the Default flag when a declared path
	// turns out to be the same interpreter under another name. A missing
	// default is not an error: an install may declare every path explicitly.
	if bin, err := exec.LookPath(defaultBinary[lang]); err == nil {
		if v, err := probeBinary(lang, bin); err == nil {
			v.Default = true
			out = append(out, v)
			seen[v.Series()] = true
		}
	}

	var bad []string
	for _, p := range declaredPaths(lang) {
		v, err := probeBinary(lang, p)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		// Deduplicated on the matching unit rather than on the path: two paths
		// to the same series are one choice, and offering both would let a
		// resolution depend on which symlink an operator listed first.
		if seen[v.Series()] {
			continue
		}
		seen[v.Series()] = true
		out = append(out, v)
	}

	sortDescending(out)

	if len(bad) > 0 {
		return probeResult{versions: out, err: fmt.Errorf(
			"%s declares %s that %s not be run: %s. This is a statement about the image "+
				"this platform is running in, so it is refused at startup rather than at "+
				"the deploy that would have needed it",
			pathEnv[lang], pluralWord(len(bad), "a path", "paths"),
			pluralWord(len(bad), "could", "could"), strings.Join(bad, "; "))}
	}
	return probeResult{versions: out}
}

// declaredPaths splits a TOFT_*_PATH list. Colon-separated, like PATH
// itself, because that is the separator every operator already knows means "a
// list of places a program might be".
func declaredPaths(lang string) []string {
	raw := os.Getenv(pathEnv[lang])
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, p := range filepath.SplitList(raw) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// probeBinary runs one interpreter and believes what it says.
func probeBinary(lang, path string) (Version, error) {
	args := []string{"--version"}
	if lang == Go {
		args = []string{"version"}
	}
	out, err := runVersion(path, args)
	if err != nil {
		return Version{}, err
	}
	v := parseVersionOutput(lang, out)
	if v == "" {
		return Version{}, fmt.Errorf("could not read a version out of %q", oneLine(out))
	}
	return Version{Language: lang, Version: v, Path: path}, nil
}

// parseVersionOutput turns each tool's greeting into a bare version.
//
//	python3 --version  ->  "Python 3.13.7"
//	node --version     ->  "v24.13.0"
//	go version         ->  "go version go1.26.5 linux/amd64"
//	R --version        ->  `R version 4.5.3 (2026-03-11) -- "Reassured Reassurer"`
//	Rscript --version  ->  "Rscript (R) version 4.5.3 (2026-03-11)"
//
// R is the one that needs a rule rather than a prefix trim: both of its
// greetings begin with a letter, so the leading-numeric-run trim below reduces
// them to "" and probeBinary fails with *could not read a version out of …*.
// That is not a failed deploy — Validate refuses to start the platform, because
// a declared path is a statement about the image the platform is running in. So
// the map entries and this case are one commit and not two.
func parseVersionOutput(lang, out string) string {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	switch lang {
	case Python:
		line = strings.TrimSpace(strings.TrimPrefix(line, "Python"))
	case Node:
		line = strings.TrimPrefix(line, "v")
	case Go:
		line = GoStamp(line)
	case R:
		// Both spellings put the number immediately after a literal "version",
		// which is what lets one rule read either without knowing which binary
		// answered.
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "version" && i+1 < len(fields) {
				line = fields[i+1]
				break
			}
		}
	}
	// Anything after the number is the tool's own decoration — uv's platform
	// triple, a distribution's build suffix. Keep the leading numeric run.
	if i := strings.IndexFunc(line, func(r rune) bool {
		return !(r >= '0' && r <= '9') && r != '.'
	}); i >= 0 {
		line = line[:i]
	}
	if _, err := parseVersion(line); err != nil {
		return ""
	}
	return line
}

// GoStamp reads a Go version out of any line the go command writes one on, and
// there are two such lines rather than one.
//
//	go version go1.27.0 linux/amd64          the command's own greeting
//	/app/toft-app: go1.25.14            the stamp inside a compiled binary
//
// The second is why this is a function rather than four lines inside
// parseVersionOutput. **A Go binary carries the toolchain that compiled it**,
// and that is not always the toolchain the platform resolved: the go command
// re-execs into another when `GOTOOLCHAIN` names one, after the platform has
// finished deciding. So the builder reads the binary back, and it must read it
// with the same rule that reads everything else — a second parser would be a
// second answer about the same string.
//
// Empty when the line carries no version, which is the honest outcome for
// output this did not recognise.
func GoStamp(line string) string {
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, "go1") || strings.HasPrefix(f, "go2") {
			return strings.TrimPrefix(f, "go")
		}
	}
	return ""
}

// DefaultSeries is the matching series of the version an app with no pin gets
// on this host, or "" when the language is absent.
//
// One definition, because both launchers need it and a second one would be a
// second answer. An R release's package library is stamped with the series it
// was built for (runtime.RenvLibraryDir), and the launcher has to name the same
// directory the build wrote or the workload starts with an empty library and
// fails at its first library() call — with an R error about a missing package
// rather than anything about paths.
func DefaultSeries(lang string) string {
	for _, v := range Available(lang) {
		if v.Default {
			return v.Series()
		}
	}
	return ""
}

// DefaultPath is the filesystem path of the version an app with no pin gets, or
// "" when the language is absent or was found by name on PATH alone.
func DefaultPath(lang string) string {
	for _, v := range Available(lang) {
		if v.Default {
			return v.Path
		}
	}
	return ""
}

// Configured is what this install's isolated runner images are *declared* to
// carry, from RUNNER_PYTHON_VERSION / RUNNER_NODE_VERSION / RUNNER_R_VERSION.
//
// Declared rather than probed, and that is the whole of §3g's split: the
// runner's self-knowledge arrives at pod start, and the deploy decision is made
// before any pod exists, because the build produces the venv on the platform
// host. Decide from the declaration; confirm in the runner.
//
// The residual is stated rather than closed: between a wrong declaration and
// the first pod start, the platform will accept a pin it cannot honour (D17).
//
// **A list, because an image may carry more than one** (D19). This is the
// declared half of what TOFT_*_PATH already says about the build host: an
// operator who installs three Rs in their image has to be able to say so, and
// for as long as this read a single version the platform judged every isolated
// R app against one series no matter what the image held. Comma-separated
// rather than colon: these are versions and not paths, and a comma is what a
// person writes when they mean a list of numbers.
//
// **The first entry is the image's default** — the interpreter on its PATH,
// which is what an app with no pin and no declaration gets. It cannot be probed
// from here, so position is the convention that carries it, and the runner
// confirms the whole set against its own filesystem at bootstrap.
//
// Deduplicated on the matching unit, for the reason declared paths are: two
// spellings of one series are one choice, and offering both would let a
// resolution depend on which the operator wrote first. A malformed entry voids
// the whole declaration rather than being skipped — ConfiguredError reports it
// and Validate refuses to start, because a list that silently dropped a name
// would be a set the operator does not think they configured.
func Configured(lang string) []Version {
	env, ok := runnerVersionEnv[lang]
	if !ok {
		return nil
	}
	fields, err := declaredVersions(os.Getenv(env))
	if err != nil {
		return nil
	}
	var out []Version
	seen := map[string]bool{}
	for _, f := range fields {
		v := Version{Language: lang, Version: f, Default: len(out) == 0}
		if seen[v.Series()] {
			continue
		}
		seen[v.Series()] = true
		out = append(out, v)
	}
	return out
}

// declaredVersions splits and validates a RUNNER_*_VERSION list. A nil result
// with a nil error is an empty declaration, which is a real configuration —
// absent is not a claim (D17).
func declaredVersions(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			return nil, fmt.Errorf("has an empty entry; comma separates versions and each " +
				"side of it has to name one")
		}
		if _, err := parseVersion(f); err != nil {
			return nil, fmt.Errorf("names %q, which is not a version number", f)
		}
		out = append(out, f)
	}
	return out, nil
}

// ConfiguredError reports a RUNNER_*_VERSION that is not a version at all.
// A separate call for the same reason as Err: refusing to start is Validate's
// job, and every other caller wants the set.
func ConfiguredError(lang string) error {
	env, ok := runnerVersionEnv[lang]
	if !ok {
		return nil
	}
	if _, err := declaredVersions(os.Getenv(env)); err != nil {
		return fmt.Errorf("%s=%q %v", env, strings.TrimSpace(os.Getenv(env)), err)
	}
	return nil
}

// Validate is the platform's startup check: every declared path execs, and
// every declared runner version is a version.
//
// It is also what forces the probe, so the first exec happens during boot
// rather than inside the first deploy that needed it.
func Validate() error {
	for _, lang := range Languages {
		if err := Err(lang); err != nil {
			return err
		}
		if err := ConfiguredError(lang); err != nil {
			return err
		}
	}
	return nil
}

// GoToolchainPosture is what GOTOOLCHAIN is set to, and it is reported rather
// than defaulted.
//
// An install with module-proxy egress — direct, or an internal mirror such as
// Artifactory or Athens, which serve toolchains as ordinary modules — sets
// `auto` and a go.mod requiring a newer toolchain fetches it. An air-gapped
// install sets `local` and gets Go's own refusal, which already names both
// versions: *go.mod requires go >= 1.27.0; running go 1.26.5*. That is a better
// message than one we would write (D5).
//
// Unset is reported as such rather than as "auto", even though auto is what Go
// does: an administrator debugging a build needs to know whether anybody
// decided, and "auto (Go's default — not configured on this install)" is a
// different fact from "auto".
func GoToolchainPosture() string {
	if v := strings.TrimSpace(os.Getenv("GOTOOLCHAIN")); v != "" {
		return v
	}
	return ""
}

// series truncates a version to the unit its language is matched on.
// SeriesOf is the matching unit of a bare version string, or "" when the string
// is not a version at all.
//
// Stricter than the unexported series() it wraps, and deliberately: that one
// hands back whatever it could not parse, which is right for a value this
// package produced itself and wrong for one read out of a file somebody else
// wrote. A renv.lock whose R.Version is missing, empty or not a number
// *declares nothing*, and "" is how the caller hears that — where "abc" coming
// back would become a series that matches no interpreter and reads as a refusal.
func SeriesOf(lang, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if _, err := parseVersion(v); err != nil {
		return ""
	}
	return series(lang, v)
}

func series(lang, v string) string {
	parts, err := parseVersion(v)
	if err != nil {
		return v
	}
	n := MatchUnit(lang)
	if len(parts) < n {
		n = len(parts)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = strconv.Itoa(parts[i])
	}
	return strings.Join(out, ".")
}

// MatchUnit is how many version components a language is matched on: one for
// Node, two for Python and Go.
//
// Three release lines look like 20/22/24 for Node and 3.11/3.12/3.13 for
// Python, and a pin means the line rather than the point release in both cases.
func MatchUnit(lang string) int {
	if lang == Node {
		return 1
	}
	return 2
}

// parseVersion reads a dotted numeric version. Nothing else: a version with a
// pre-release suffix or a distribution's local marker is not something this
// resolves against, and saying so is better than guessing at an ordering.
func parseVersion(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty version")
	}
	fields := strings.Split(s, ".")
	if len(fields) > 3 {
		return nil, fmt.Errorf("%q has more than three components", s)
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%q is not a version number", s)
		}
		out = append(out, n)
	}
	return out, nil
}

// compare orders two versions at n components, padding what is missing with
// zero. -1, 0, +1.
func compare(a, b []int, n int) int {
	at := func(v []int, i int) int {
		if i < len(v) {
			return v[i]
		}
		return 0
	}
	for i := 0; i < n; i++ {
		switch {
		case at(a, i) < at(b, i):
			return -1
		case at(a, i) > at(b, i):
			return 1
		}
	}
	return 0
}

func sortDescending(vs []Version) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, errA := parseVersion(vs[i].Version)
		b, errB := parseVersion(vs[j].Version)
		if errA != nil || errB != nil {
			return false
		}
		return compare(a, b, 3) > 0
	})
}

func runVersion(path string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	// A wrapper that hands an inherited pipe to a child must not keep the
	// probe waiting past the timeout either.
	cmd.WaitDelay = 2 * time.Second
	// The same clean environment every other child exec in this product gets.
	// A probe is a child process like any other and has no business inheriting
	// JWT_SECRET.
	cmd.Env = baseEnv()
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s %s: did not answer within %s", path, strings.Join(args, " "), probeTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %v", filepath.Base(path), strings.Join(args, " "), err)
	}
	return string(out), nil
}

// baseEnv is runtime.BaseEnv's allow-list, restated here rather than imported.
//
// internal/runtime is the workload seam and imports the world below it; this
// package is imported *by* the builder and by the runner binary, and must stay
// a leaf. Six variable names is a cheaper duplication than an import cycle, and
// TestBaseEnvMatchesRuntime holds the two in step.
func baseEnv() []string {
	out := make([]string, 0, 8)
	for _, k := range []string{"PATH", "HOME", "LANG", "TMPDIR", "USER", "SHELL"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// resetForTest drops the probe cache. Tests set TOFT_*_PATH and need the
// next Available to mean it; nothing in production ever re-probes.
func resetForTest() {
	probeOnce.Range(func(k, _ any) bool { probeOnce.Delete(k); return true })
	probeCache.Range(func(k, _ any) bool { probeCache.Delete(k); return true })
}

// ResetForTest is resetForTest for a test in another package, and it exists
// because the probe cache is process-wide by design.
//
// A test that declares an interpreter and then asks what this install has is
// only correct if nothing in its package has asked first — and "nothing has
// asked yet" is not a property a test can hold on to. It was true of
// internal/runner's until a repair started resolving the interpreter it rebuilds
// a venv with, at which point an unrelated notebook test primed Python and the
// version assertions in another file started reading this host's.
//
// Exported rather than duplicated: the alternative is every package that probes
// growing its own way to un-probe, which is a second cache in all but name.
// Nothing in production calls it, and the probe is deliberately not resettable
// by anything else — a set that could change under a running platform is what
// restart-to-notice exists to prevent.
func ResetForTest() { resetForTest() }
