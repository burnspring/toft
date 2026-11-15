package main

// Guards on the two Dockerfiles, in the spirit of deploy/compose/compose_test.go
// and deploy/charts/chart_test.go: a *stated* fact in one file that another file
// no longer agrees with.
//
// The subject here is narrower than "the images build". It is the seam between
// the two execution modes. `Dockerfile` builds the platform image, which runs
// shared-mode apps as OS processes beside the platform; `docker/runner/
// Dockerfile` builds the family images, which run isolated-mode apps in pods.
// The same app can be moved between the two without a rebuild, and the promise
// that this changes nothing about how it runs is only as good as the two files
// naming the same toolchain. They are edited months apart, for unrelated
// reasons, and a drift between them does not fail either build — it produces an
// app that works in one mode and fails in the other, reported by whoever
// happened to switch.
//
// The platform image's *Python* used to be the thing none of this could catch: it
// was whatever `python3` the runtime stage's Debian carried, so the only way to
// read it was to build the image. It is a path in a file now — the image installs
// a stated version with uv and declares it at TOFT_PYTHON_PATH, because 3.14
// does not exist in a Debian stable and family-python is on 3.14 — so the pair is
// finally checkable from two files. What still cannot be checked here is whether
// the binary at that path *is* that version, which is why the RUN that puts it
// there ends in `python3 -V` and why the platform refuses to start on a declared
// path that will not exec (ADR §64).

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

const (
	platformDockerfile = "Dockerfile"
	runnerDockerfile   = "docker/runner/Dockerfile"
)

func readDockerfile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if os.IsNotExist(err) && (strings.HasPrefix(name, "scripts/") || strings.HasPrefix(name, "tests/")) {
		// The public source snapshot carries no private scripts or suites; what
		// these checks hold them to is this repository's concern.
		t.Skipf("%s is not in this tree", name)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// findAll returns every capture of the first group, and fails if there are
// none — a pattern that silently stops matching is how a guard like this turns
// into a test that passes because it checks nothing.
func findAll(t *testing.T, file, body string, re *regexp.Regexp) []string {
	t.Helper()
	ms := re.FindAllStringSubmatch(body, -1)
	if len(ms) == 0 {
		t.Fatalf("%s: no match for %s — the file's shape changed and this guard "+
			"is no longer reading anything", file, re)
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m[1])
	}
	return out
}

// uv resolves and installs a Python app's dependencies in both modes, and a
// resolver version is not neutral: it decides which dependency versions the app
// actually gets. The runner pinned 0.5.11 against the platform's floating
// `curl … | sh` for twenty months, so the same requirements.txt could resolve
// two ways depending on where the app ran.
func TestUvIsPinnedToTheSameVersionInBothImages(t *testing.T) {
	re := regexp.MustCompile(`ghcr\.io/astral-sh/uv:([^\s]+)`)

	platform := findAll(t, platformDockerfile, readDockerfile(t, platformDockerfile), re)
	runner := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile), re)

	for _, got := range append(append([]string{}, platform...), runner...) {
		if got == "latest" || got == "" {
			t.Errorf("uv is pinned to %q; a moving tag makes the two modes agree "+
				"only until one of the images is rebuilt", got)
		}
	}
	if platform[0] != runner[0] {
		t.Errorf("uv is %s in %s and %s in %s — the same app would be resolved by "+
			"two different resolvers depending on execution mode",
			platform[0], platformDockerfile, runner[0], runnerDockerfile)
	}
}

// Node is the runtime a deployed Node app executes on, so the two modes have to
// agree on the major or an app that works in one fails in the other. Checked as
// the whole tag rather than just the major: the Debian suffix decides the
// *Python* the platform image resolves to, and letting the tags differ there is
// how that pair silently comes apart again.
func TestNodeIsTheSameTagInBothImages(t *testing.T) {
	re := regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?node:(\S+)`)

	tags := findAll(t, platformDockerfile, readDockerfile(t, platformDockerfile), re)
	tags = append(tags, findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile), re)...)

	for _, tag := range tags[1:] {
		if tag != tags[0] {
			t.Fatalf("node bases disagree: %v — shared mode and isolated mode must "+
				"not hand an app two different Node runtimes", tags)
		}
	}
	if tags[0] == "latest" || tags[0] == "lts" {
		t.Errorf("node is pinned to the moving tag %q; the point of naming a major "+
			"is that leaving LTS is a decision someone makes, not something that "+
			"happens on a Tuesday", tags[0])
	}
}

// The declaration and the image it describes.
//
// `family-python`'s `TOFT_PYTHON_SERIES` is the set an isolated Python app
// can actually run under, and `RUNNER_PYTHON_VERSION` is what the *platform* is
// told that set is — the thing a `[runtime]` pin is judged against for an
// isolated app, because the build happens on the platform host before any pod
// exists (ADR §64). The runner checks the two at bootstrap and refuses a
// workload the declaration lied about, which is the enforcement that matters;
// this is the cheaper half, and it fires in `scripts/check.sh` rather than in a
// cluster.
//
// It reads the stage's own declaration rather than its `FROM` tag, and that is
// the change a second interpreter forced: a tag names one version and the stage
// now installs two more beside it, so the tag stopped being the whole claim in
// the same way it never was for family-r, whose R arrives from apt.
//
// The chart is deliberately not checked. `runnerVersions.python` ships empty,
// because a customer's image is not this repository's to describe, and an empty
// value means nothing is claimed rather than something wrong is.
func TestTheDeclaredRunnerPythonMatchesTheImage(t *testing.T) {
	assertDeclarationMatches(t, "python", "RUNNER_PYTHON_VERSION", "TOFT_PYTHON_SERIES")

	// The base image's own interpreter is the first entry by convention — the
	// one on the stage's PATH, which is what an app with no pin gets — so the
	// tag and the head of the list are two spellings of one fact.
	tag := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?python:(\d+\.\d+)`))[0]
	if head := declaredSeries(t, "TOFT_PYTHON_SERIES")[0]; head != tag {
		t.Errorf("family-python is python:%s and declares %s first. The first entry is the "+
			"image's default, so these two disagreeing means an app with no pin at all "+
			"resolves to something other than what it runs on", tag, head)
	}
}

// The same for Node, where the tag is already held across the two Dockerfiles
// by TestNodeIsTheSameTagInBothImages — this adds the third statement of it,
// which is the one an isolated deploy is decided by.
func TestTheDeclaredRunnerNodeMatchesTheImage(t *testing.T) {
	assertDeclarationMatches(t, "node", "RUNNER_NODE_VERSION", "TOFT_NODE_SERIES")

	tag := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?node:(\d+)`))[0]
	if head := declaredSeries(t, "TOFT_NODE_SERIES")[0]; head != tag {
		t.Errorf("family-node is node:%s and declares %s first. The first entry is the "+
			"image's default, so these two disagreeing means an app with no pin at all "+
			"resolves to something other than what it runs on", tag, head)
	}
}

// declaredSeries is one family stage's `TOFT_*_SERIES`, split.
func declaredSeries(t *testing.T, envVar string) []string {
	t.Helper()
	raw := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^ENV `+envVar+`=(\S+)`))[0]
	out := strings.Split(raw, ",")
	for _, s := range out {
		if strings.TrimSpace(s) == "" {
			t.Fatalf("%s=%q has an empty entry; toolchain.Configured refuses the whole "+
				"declaration for one, so this image would be judged against nothing", envVar, raw)
		}
	}
	return out
}

// assertDeclarationMatches holds one family's three statements of its own
// version set together: what scripts/k3s.sh puts in the configmap, what the
// stage declares about itself, and — for the entries that are not the base
// image's — where the stage says each interpreter is.
//
// The last of those is what a list made possible and what the single-version
// check could not see: a declared series with no interpreter behind it is an app
// that deploys and then will not start.
func assertDeclarationMatches(t *testing.T, family, scriptVar, imageVar string) {
	t.Helper()
	script := []byte(readDockerfile(t, "scripts/k3s.sh"))
	stated := findAll(t, "scripts/k3s.sh", string(script),
		regexp.MustCompile(`(?m)^`+scriptVar+`=\$\{`+scriptVar+`:-(\S+)\}`))[0]
	declared := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^ENV `+imageVar+`=(\S+)`))[0]

	if stated != declared {
		t.Fatalf("scripts/k3s.sh declares %s=%s and docker/runner/Dockerfile's family-%s "+
			"declares %s=%s — the declaration is what a [runtime] pin is judged against, so "+
			"an app pinning a series only one of them knows about would deploy and then be "+
			"refused by the runner at start. Move both in one commit",
			scriptVar, stated, family, imageVar, declared)
	}

	paths := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^ENV TOFT_`+strings.ToUpper(family)+`_PATH=(\S+)`))[0]
	for i, s := range declaredSeries(t, imageVar) {
		if i == 0 {
			// The base image's own, found by name on PATH and deliberately not
			// repeated as a path.
			continue
		}
		if !strings.Contains(paths, "/"+s+"/") {
			t.Errorf("family-%s declares %s %s and TOFT_%s_PATH=%s names no interpreter "+
				"for it. The declaration is what the platform resolves an isolated deploy "+
				"against and this is where the runner's own probe looks; a series in one and "+
				"not the other deploys an app that cannot start",
				family, imageVar, s, strings.ToUpper(family), paths)
		}
	}
}

// The two images have to carry the *same* set, for Python and for Node, and the
// reason is different in each and equally invisible.
//
// A Python venv records the absolute path of the interpreter it was built
// against, and every release is built on the platform host in both execution
// modes — so a path the platform writes and this image does not have is a tree
// the pod discards and rebuilds, and a *version* it does not have at all is one
// the platform will resolve an app onto and no pod can run. A Node release
// carries no path, and carries something worse: a native module compiled
// against one major's ABI, which fails at `require` naming a `.node` file.
//
// Neither image's build fails when they drift, because each is individually
// correct and only an app that moves between modes notices — the same shape as
// the R system-library list, and held the same way.
func TestBothImagesCarryTheSamePythonAndNodeSeries(t *testing.T) {
	platform := readDockerfile(t, platformDockerfile)
	for _, c := range []struct {
		family, imageVar string
		platformPath     *regexp.Regexp
	}{
		{"python", "TOFT_PYTHON_SERIES",
			regexp.MustCompile(`(?m)^ENV TOFT_PYTHON_PATH=(\S+)`)},
		{"node", "TOFT_NODE_SERIES",
			regexp.MustCompile(`(?m)^ENV TOFT_NODE_PATH=(\S+)`)},
	} {
		runner := declaredSeries(t, c.imageVar)
		// The platform image has no SERIES declaration to read — it states
		// paths, because its interpreters are what its own probe finds and
		// nothing is told about them from outside. So the series come out of
		// the one line in that file a probe will actually walk.
		var host []string
		for _, p := range strings.Split(findAll(t, platformDockerfile, platform, c.platformPath)[0], ":") {
			host = append(host, seriesInPath(t, c.family, p))
		}
		sort.Strings(host)
		want := append([]string(nil), runner...)
		sort.Strings(want)
		if !slices.Equal(host, want) {
			t.Errorf("the platform image carries %s %v and family-%s carries %v.\n"+
				"A release is built on the platform and run in the pod, so a version in one "+
				"image and not the other is an app that deploys and then runs on something "+
				"else. Move both in one commit", c.family, host, c.family, want)
		}
		if len(want) < 3 {
			t.Errorf("only %v — this guard exists because a pin is refused and never "+
				"approximated, so an install needs more than the current version, and a list "+
				"this short means either the regex stopped reading or ADR §64 D21 was "+
				"reverted in one image", want)
		}
	}
}

// seriesInPath reads the matching series out of one entry of a
// TOFT_*_PATH list — /opt/python/3.12/bin/python3, /opt/node/20/bin/node,
// and the base image's own, whose version is not in its path and which is by
// convention the first entry of the peer declaration.
func seriesInPath(t *testing.T, family, p string) string {
	t.Helper()
	if rest, ok := strings.CutPrefix(p, "/opt/"+family+"/"); ok {
		return strings.SplitN(rest, "/", 2)[0]
	}
	head := declaredSeries(t, "TOFT_"+strings.ToUpper(family)+"_SERIES")[0]
	if !strings.HasPrefix(p, "/usr/") {
		t.Fatalf("%s: cannot read a version out of the %s path %q — this guard is no longer "+
			"reading anything", platformDockerfile, family, p)
	}
	return head
}

// The two images' Pythons, read out of the two files.
//
// This is the guard the Python row of the toolchain table had no way to have
// until 2026-08-18. Before that the platform's interpreter was a property of its
// base image's Debian — not a string anywhere — so "do the two modes agree about
// Python" could only be answered by building the image and running it. Now the
// platform installs a stated version with uv at a versioned path and declares it,
// and the runner's is a FROM tag, so the disagreement that matters is two regexes
// apart.
//
// The failure it prevents is the *easy half* of a Python move: editing
// family-python's tag alone, which reopens exactly the defect the trixie move
// closed — the same app getting a different interpreter depending on which
// execution mode it landed in, chosen by nothing the developer can see. That one
// is invisible in a way the declaration mismatch is not, because nothing refuses
// it: both images work, and only an app that moves between modes notices.
//
// Compared on the *minor*, because that is Python's matching unit
// (internal/toolchain.MatchUnit): 3.14.7 and 3.14.9 are the same answer to every
// question this platform asks, and a patch pin is refused rather than truncated.
func TestThePlatformImageDeclaresTheSamePythonAsTheRunner(t *testing.T) {
	declared := findAll(t, platformDockerfile, readDockerfile(t, platformDockerfile),
		regexp.MustCompile(`(?m)^ENV TOFT_PYTHON_PATH=\S*?/(\d+\.\d+)/`))

	family := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?python:(\d+\.\d+)`))

	if declared[0] != family[0] {
		t.Fatalf("the platform image declares Python %s at TOFT_PYTHON_PATH and "+
			"docker/runner/Dockerfile's family-python is python:%s — the same app would get a "+
			"different interpreter depending on which execution mode it landed in, which is "+
			"chosen by nothing its developer can see. Both images move in one commit, or "+
			"neither does", declared[0], family[0])
	}
}

// Where a Go app's binary is built, and where it is run.
//
// Go's version does not reach an isolated workload the way Node's and Python's
// do: `go build` produces a binary on the platform host and the pod execs that,
// so `family-go`'s own toolchain is used only in a build mode the platform has
// never invoked. What reaches the pod is the binary — and a binary that imports
// "C" is linked against the glibc of the image that built it.
//
// So the comparison is not between the two `golang:` bases, which is the
// tempting one and the wrong one: the platform's `gotoolchain` stage exists to
// have `/usr/local/go` copied out of it, and nothing else from that stage
// survives into the image. The pair that decides whether a binary runs is the
// platform's **runtime** base — where `go build` actually executes — and
// `family-go`, where the result is exec'd.
//
// They are trixie and bookworm — measured, glibc 2.41-12+deb13u3 against
// 2.36-9+deb12u14. A Go app importing "C" would build against the newer and die
// in the pod with a missing symbol.
//
// *Would*, because today it cannot get that far: the platform image carries no
// C compiler at all (`command -v gcc cc x86_64-linux-gnu-gcc` finds nothing in
// node:24-trixie-slim, and the runtime stage's apt-get install adds none), so
// such a package fails during `go build` and no binary is produced. This is a
// trap that arms itself rather than a live bug, and the inversion is the part
// to remember: family-go has gcc and the older glibc, and the image that does
// the compiling has neither. Adding build-essential for a native npm module
// would turn it into a pod failure in a different language family than the
// change that caused it.
//
// ADR §44 records that shape from one direction and §64's GOOS/GOARCH refusal
// from another; this is the third, and until family-go moved, the only one
// nothing looked at.
//
// It skipped for exactly as long as the answer was decided and unedited: the
// decision was that family-go moves, since the build end is a Node image chosen
// for trixie's Python and has no freedom to move, and failing ahead of that edit
// would only have turned check.sh red for every session on this box. Both landed
// in one commit and this fails now. The two readings it replaced are recorded
// above rather than deleted, because "same Go, different glibc" is what a future
// bookworm pin would look like on the way back in.
func TestAGoBinaryIsBuiltAndRunOnTheSameDebian(t *testing.T) {
	suffix := regexp.MustCompile(`^\S+?-(\w+?)(?:-slim)?$`)
	debian := func(t *testing.T, file, tag string) string {
		t.Helper()
		m := suffix.FindStringSubmatch(tag)
		if m == nil {
			t.Fatalf("%s: cannot read a Debian release out of the tag %q — this guard is no "+
				"longer reading anything", file, tag)
		}
		return m[1]
	}

	// The platform's runtime stage, by name. It used to be "the last FROM in
	// the file", which stopped being true the day the image grew a Quarto
	// variant built on top of it — and a positional read of a Dockerfile is a
	// guard that goes quiet exactly when somebody adds a stage.
	runtimeTags := findAll(t, platformDockerfile, readDockerfile(t, platformDockerfile),
		regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?node:(\S+)\s+AS\s+runtime\s*$`))
	buildsOn := debian(t, platformDockerfile, runtimeTags[0])

	familyGo := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?golang:(\S+)\s+AS\s+family-go`))
	runsOn := debian(t, runnerDockerfile, familyGo[0])

	if buildsOn != runsOn {
		t.Fatalf("a Go app's binary is built on %s (the platform image's runtime stage, where "+
			"`go build` actually runs) and exec'd on %s (family-go). Same Go, different glibc: "+
			"a binary importing \"C\" links against the build end's and dies at exec in the pod "+
			"with a missing symbol and nothing about Debian in the message. The build end is "+
			"not free to move — it is a Node image, on trixie for that Debian's Python — so "+
			"family-go is the end that follows: put it on a %s base",
			buildsOn, runsOn, buildsOn)
	}
}

// The runner's first act in run mode is an HTTPS GET of its release from the
// artifact store, and Go reads the system trust store. Three of the four family
// bases carry a CA bundle; node:24-trixie-slim carries none at all — an empty
// /etc/ssl/certs and no ca-certificates package — so a Node workload died at
// artifact_fetch with `x509: certificate signed by unknown authority` against a
// presigned S3 URL, which reads as a bad signature rather than an empty trust
// store.
//
// Read out of the Dockerfile rather than out of a built image, like every other
// guard in this file: the point is that nobody deletes the line, and a test that
// needed docker would be skipped in exactly the CI where that happens.
func TestEveryRunnerFamilyTrustsPublicTLS(t *testing.T) {
	content := readDockerfile(t, runnerDockerfile)

	// The families whose bases carry no bundle of their own. python-slim,
	// golang and distroless-static all ship one; node:*-slim and debian:*-slim
	// do not, and each has to install it or every workload of that family dies
	// at artifact_fetch.
	for _, family := range []string{"family-node", "family-r"} {
		start := strings.Index(content, "AS "+family)
		if start < 0 {
			t.Fatalf("%s: no %s stage — this guard is no longer reading anything",
				runnerDockerfile, family)
		}
		stage := content[start:]
		if next := strings.Index(stage, "\nFROM "); next > 0 {
			stage = stage[:next]
		}

		if !strings.Contains(stage, "ca-certificates") {
			t.Errorf("%s: %s installs no ca-certificates. Its base ships an empty "+
				"/etc/ssl/certs, so the runner cannot verify the artifact store's certificate "+
				"and every workload of that family fails at artifact_fetch with x509: "+
				"certificate signed by unknown authority — an error that reads as a broken "+
				"signature or a clock skew rather than an empty trust store.",
				runnerDockerfile, family)
		}
	}
}

// R's half of the declaration guard, and it is shaped differently from Python's
// and Node's because R's version is not in a FROM tag to read: family-r is
// debian:trixie-slim and R arrives from apt. So the stage states the series
// itself, its own RUN holds that string against the interpreter apt actually
// installed — the build fails rather than the cluster, the day trixie moves to
// 4.6 — and this test holds scripts/k3s.sh against the same string.
//
// Compared on the *series*, because that is R's matching unit: MatchUnit returns
// 2 for anything that is not Node, so 4.5.0 and 4.5.1 are the same answer to
// every question this platform asks, and Debian moving the patch under a
// security update invalidates nothing.
func TestTheDeclaredRunnerRMatchesTheImage(t *testing.T) {
	declared := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^ENV TOFT_R_SERIES=(\S+)`))

	script := []byte(readDockerfile(t, "scripts/k3s.sh"))
	stated := findAll(t, "scripts/k3s.sh", string(script),
		regexp.MustCompile(`(?m)^RUNNER_R_VERSION=\$\{RUNNER_R_VERSION:-(\S+)\}`))

	if stated[0] != declared[0] {
		t.Fatalf("scripts/k3s.sh declares RUNNER_R_VERSION=%s and docker/runner/Dockerfile's "+
			"family-r declares TOFT_R_SERIES=%s — the declaration is what a [runtime] pin "+
			"is judged against, so an app pinning the declared version would deploy and then "+
			"be refused by the runner at start. Move both in one commit",
			stated[0], declared[0])
	}

	// Both are lists now (ADR §64 D19), so equal strings are no longer the
	// whole question: every series named has to have an interpreter in the
	// stage, and TOFT_R_PATH has to say where it is. A declared series
	// with nothing behind it is an app that deploys and then will not start.
	paths := findAll(t, runnerDockerfile, readDockerfile(t, runnerDockerfile),
		regexp.MustCompile(`(?m)^ENV TOFT_R_PATH=(\S+)`))
	for _, s := range strings.Split(declared[0], ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			t.Fatalf("TOFT_R_SERIES=%q has an empty entry; toolchain.Configured refuses "+
				"the whole declaration for one, so this image would be judged against nothing",
				declared[0])
		}
		// apt's R is on PATH as plain `Rscript` and is the first entry by
		// convention; the rest have to be named by path.
		if s == strings.Split(declared[0], ",")[0] {
			continue
		}
		if !strings.Contains(paths[0], "/opt/R/"+s+".") {
			t.Errorf("family-r declares R %s and TOFT_R_PATH=%s names no interpreter "+
				"for it. The declaration is what the platform resolves an isolated deploy "+
				"against and this is where the runner's own probe looks; a series in one and "+
				"not the other deploys an app that cannot start", s, paths[0])
		}
	}
}

// The two images have to carry the *same* R series, for the reason they have to
// carry the same system libraries and the same Debian.
//
// An renv library is restored on the platform host in both execution modes and
// loaded in the pod, and it is stamped with the series it was built for —
// `renv/library/R-4.4`. So a series the platform can build against and family-r
// cannot load is a release that packs cleanly, ships, and then either has its
// library discarded whole and rebuilt in the pod or refuses to start naming a
// version. Neither image's build fails when they drift, because each is
// individually correct and only an R app notices: the same shape as the system
// library list, and held the same way.
//
// Read off TOFT_R_PATH in both, because that is the one line in each file
// that says what a probe will actually find — the platform has no
// TOFT_R_SERIES to compare against, and a version named only in a `for`
// loop is a string the probe never sees.
func TestBothImagesCarryTheSameRSeries(t *testing.T) {
	series := func(file string) []string {
		paths := findAll(t, file, readDockerfile(t, file),
			regexp.MustCompile(`(?m)^ENV TOFT_R_PATH=(\S+)`))
		var out []string
		for _, p := range strings.Split(paths[0], ":") {
			// /opt/R/4.4.3/bin/Rscript -> 4.4; /usr/bin/Rscript is apt's and
			// its series is not in the path, so it is named by the constant
			// both files agree on rather than guessed at.
			if rest, ok := strings.CutPrefix(p, "/opt/R/"); ok {
				v := strings.SplitN(rest, "/", 2)[0]
				parts := strings.Split(v, ".")
				if len(parts) < 2 {
					t.Fatalf("%s: TOFT_R_PATH names /opt/R/%s, which is not a version", file, v)
				}
				out = append(out, parts[0]+"."+parts[1])
				continue
			}
			out = append(out, "apt")
		}
		sort.Strings(out)
		return out
	}

	platform, runner := series(platformDockerfile), series(runnerDockerfile)
	if !slices.Equal(platform, runner) {
		t.Fatalf("the platform image carries R %v and family-r carries %v.\n"+
			"An renv library is restored on the platform and loaded in the pod, stamped with "+
			"the series it was built for — so a series in one image and not the other is a "+
			"release that ships and then cannot start. Move both in one commit", platform, runner)
	}
	if len(platform) < 2 {
		t.Fatalf("only %v — this guard exists because an install needs more than one R, and a "+
			"list this short means either the regex stopped reading or the multi-version "+
			"decision (ADR §64 D19) was reverted in one image", platform)
	}
}

// The two images' Debians, which for R is a stronger coupling than the two
// images' interpreters.
//
// A binary R package is compiled against a distribution's system libraries, and
// an renv library is restored on the *platform* host in both execution modes —
// the build never happens in the pod. So the release that ships carries objects
// built against this image's Debian and loaded against family-r's. If those
// differ the release unpacks cleanly and then fails at dyn.load, one package at
// a time, in a pod log, with nothing about Debian in the message.
//
// Neither build fails when they drift, which is what makes this worth a test:
// both images are individually correct and only an R app notices. It is the
// same class of defect TestAGoBinaryIsBuiltAndRunOnTheSameDebian exists for,
// and it arrives the same way — somebody moves one base for an unrelated reason.
func TestBothImagesTakeRFromTheSameDebian(t *testing.T) {
	debian := regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?(?:\S+:)?(?:.*?-)?(bookworm|trixie|forky)(?:-slim)?\s+AS\s+(runtime|family-r)\s*$`)

	platform := debian.FindStringSubmatch(readDockerfile(t, platformDockerfile))
	runner := debian.FindStringSubmatch(readDockerfile(t, runnerDockerfile))
	if platform == nil {
		t.Fatalf("%s: could not read the runtime stage's Debian — this guard is no longer "+
			"reading anything", platformDockerfile)
	}
	if runner == nil {
		t.Fatalf("%s: could not read family-r's Debian — this guard is no longer reading "+
			"anything", runnerDockerfile)
	}

	if platform[1] != runner[1] {
		t.Errorf("the platform image's runtime stage is Debian %s and family-r is Debian %s. "+
			"An renv library is restored on the platform host in both execution modes and "+
			"loaded in the pod, so a binary package built against one and loaded against the "+
			"other fails at dyn.load — after a green deploy, in a pod log, naming a shared "+
			"object rather than a distribution", platform[1], runner[1])
	}

	// Both ends install R the same way, which is what makes the versions agree
	// by construction rather than by a pinned string in two files. Read from the
	// instructions rather than the whole file — the prose above and in both
	// Dockerfiles names the packages it is refusing, and a guard that matched
	// its own explanation would fail on the comment that justifies it.
	// Every R in both images is Posit's build for debian-13. A download for
	// another distribution is the mismatch the FROM check above exists to
	// prevent, arriving through a URL instead of a tag; and r-base-core —
	// directly, or through r-base-dev — would put trixie's own R at
	// /usr/bin/Rscript as an interpreter neither image declares.
	for _, f := range []struct{ name, body string }{
		{platformDockerfile, rInstall(readDockerfile(t, platformDockerfile), "")},
		{runnerDockerfile, rInstall(readDockerfile(t, runnerDockerfile), "")},
	} {
		if !strings.Contains(f.body, "cdn.posit.co/r/debian-13/") {
			t.Errorf("%s no longer installs Posit's R for debian-13 — if one end starts "+
				"taking R from somewhere else, an renv library restored on the platform "+
				"stops loading in the pod", f.name)
		}
		for _, apt := range []string{"r-base-core", "r-base-dev"} {
			if strings.Contains(f.body, apt) {
				t.Errorf("%s installs %s, which puts trixie's R 4.5 on PATH beside the "+
					"declared set — and the probe takes the Rscript on PATH as the default", f.name, apt)
			}
		}
	}

	platformBody := rInstall(readDockerfile(t, platformDockerfile), "")
	if !strings.Contains(platformBody, "build-essential") || !strings.Contains(platformBody, "gfortran") {
		t.Errorf("%s no longer installs build-essential and gfortran. Without a compiler every "+
			"R package pinned to a version older than the TOFT_R_REPOS snapshot fails at "+
			"`make: not found`, several minutes into a restore, at the bottom of an renv "+
			"traceback — and a Posit Connect manifest pins exactly that way", platformDockerfile)
	}
	runnerBody := rInstall(readDockerfile(t, runnerDockerfile), "AS family-r")
	if strings.Contains(runnerBody, "r-base-dev") || strings.Contains(runnerBody, "build-essential") {
		t.Errorf("family-r installs an R build toolchain. Nothing is ever compiled in a pod: " +
			"the renv library is restored on the platform host in both execution modes and " +
			"arrives in the release already built. This is only image size, in every pod")
	}
}

// rInstall returns a Dockerfile's instruction lines with every comment dropped,
// optionally narrowed to the single stage beginning at marker.
func rInstall(body, marker string) string {
	if marker != "" {
		start := strings.Index(body, marker)
		if start < 0 {
			return ""
		}
		body = body[start:]
		if next := strings.Index(body, "\nFROM "); next > 0 {
			body = body[:next]
		}
	}
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// Quarto is in the one image, and the guard changed shape when the second tag
// went away (ADR §73, reversed 2026-09-01).
//
// It used to check that `quarto` built on `runtime` and that `default` was the
// last stage — both about keeping two tags honest. What matters now is the
// opposite: that there is **no** variant stage to fall out of. A `--target`
// nobody passes is a layer nobody gets, and the failure is silent in the worst
// way — an install that runs, serves, and refuses a `.qmd` by name, which is
// indistinguishable from an operator who built their own image without it.
func TestQuartoIsInTheImageEveryInstallGets(t *testing.T) {
	body := readDockerfile(t, platformDockerfile)

	// One published artifact. A stage named for a variant is the shape this
	// decision removed, and re-adding one silently splits the tag again.
	for _, stage := range []string{"quarto", "default"} {
		if regexp.MustCompile(`(?m)^FROM\s+.*?\s+AS\s+` + stage + `\s*$`).MatchString(body) {
			t.Errorf("the Dockerfile has a %q stage again. Quarto is part of the runtime "+
				"image now, so a variant stage means a `docker build .` and a published "+
				"tag can differ by a toolchain — which is exactly what an operator "+
				"cannot see", stage)
		}
	}

	// The install has to actually be in the stage a bare build produces, which
	// is the last one.
	stages := regexp.MustCompile(`(?m)^FROM\s+.*?\s+AS\s+(\S+)\s*$`).FindAllStringSubmatch(body, -1)
	if len(stages) == 0 {
		t.Fatal("no named stages in the Dockerfile — this guard is no longer reading anything")
	}
	if last := stages[len(stages)-1][1]; last != "runtime" {
		t.Errorf("the last stage is %q, want \"runtime\" — a bare `docker build .` builds "+
			"the last stage, and everything downstream of this file assumes that is the "+
			"whole image", last)
	}
	if !strings.Contains(body, "quarto-cli/releases/download") {
		t.Error("the runtime image no longer installs Quarto, so every .qmd deploy on a " +
			"stock install is refused by name")
	}

	// The version is pinned for the reason uv's is: a renderer version is not
	// neutral, it decides what a document actually looks like.
	if !regexp.MustCompile(`(?m)^ARG QUARTO_VERSION=\d+\.\d+\.\d+\s*$`).MatchString(body) {
		t.Error("QUARTO_VERSION is not pinned to an exact version")
	}

	// The image states the path, so an operator does not have to. Absent, the
	// binary would still be found on PATH — but it is not on PATH, and an
	// install whose engine is refused by name with nothing wrong is the worst
	// version of this.
	if !regexp.MustCompile(`(?m)^ENV QUARTO_BIN=/opt/quarto/bin/quarto\s*$`).MatchString(body) {
		t.Error("the image does not set QUARTO_BIN")
	}
}

// The R system libraries have to be the same at both ends, for the reason the
// Debian has to be.
//
// A binary R package links the distribution's shared libraries rather than
// vendoring them, and an renv library is restored on the *platform* host in
// both execution modes. So a library named in one image and not the other
// produces a release that installs cleanly — renv's own load test passes on the
// platform, because the library is there — and then fails at dyn.load inside
// the pod, naming a shared object rather than an image.
//
// Neither build fails when they drift, which is what makes this worth a test:
// each image is individually correct and only an R app notices. The list is the
// specification, and moving it is a two-file commit by construction.
//
// Read off `ARG R_SYSTEM_LIBRARIES`, the one place in each file where R's own
// libraries live — a name added to some other RUN in either image is not on
// this list and is not held to it, which is deliberate: the coupling is R's,
// not the image's in general. It was the words after `r-base-core` on an apt
// line until R stopped coming from apt.
func TestBothImagesInstallTheSameRSystemLibraries(t *testing.T) {
	re := regexp.MustCompile(`(?s)\nARG R_SYSTEM_LIBRARIES="(.*?)"`)

	libs := func(file string) []string {
		m := re.FindStringSubmatch(readDockerfile(t, file))
		if m == nil {
			t.Fatalf("%s: no `ARG R_SYSTEM_LIBRARIES=\"…\"` — the file's shape changed and "+
				"this guard is no longer reading anything", file)
		}
		var out []string
		for _, f := range strings.Fields(m[1]) {
			if f != `\` {
				out = append(out, f)
			}
		}
		sort.Strings(out)
		return out
	}

	platform, runner := libs(platformDockerfile), libs(runnerDockerfile)
	if !slices.Equal(platform, runner) {
		t.Fatalf("the platform image installs R system libraries %v and family-r installs %v.\n"+
			"A library in one and not the other is a release that restores cleanly on the "+
			"platform and fails at dyn.load in the pod, naming a shared object and nothing "+
			"about the image. Move both in one commit", platform, runner)
	}
	if len(platform) < 2 {
		t.Fatalf("only %v — this guard exists because the list grows, and a list this short "+
			"means the regex stopped reading the install", platform)
	}
}

// --- the build context -------------------------------------------------------

// contextBudget is what the daemon may be handed. The measured source tree is
// ~20 MB; the number here is generous on purpose, because the failure this
// closes is not "the context is big" — it is that nobody would ever notice it
// becoming big again. It was 3.3 GB, of which 3.0 GB was a scratch directory
// written *while a build was running*, and nothing errored.
const contextBudget = 64 << 20

// TestTheBuildContextIsSource walks the tree under .dockerignore and fails over
// contextBudget, naming the largest things it counted.
//
// Both Dockerfiles copy narrowly, and that is not the same guarantee: a narrow
// COPY narrows what lands in a layer, while the *context* is tarred and shipped
// to the daemon before any instruction runs. scripts/k3s.sh runners pays it once
// per family.
func TestTheBuildContextIsSource(t *testing.T) {
	pats := parseDockerignore(t, ".dockerignore")
	if len(pats) < 5 {
		t.Fatalf(".dockerignore parsed to %d patterns — the file's shape changed and this "+
			"guard is no longer reading anything", len(pats))
	}

	var total int64
	sizes := map[string]int64{} // top-level entry -> bytes counted under it
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		rel := filepath.ToSlash(p)
		if dockerignored(rel, d.IsDir(), pats) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// A file that vanished under the walk is scratch by definition and
			// is not what this test is measuring.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		sizes[strings.SplitN(rel, "/", 2)[0]] += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	if total > contextBudget {
		type entry struct {
			name string
			n    int64
		}
		var top []entry
		for k, v := range sizes {
			top = append(top, entry{k, v})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
		var b strings.Builder
		for i, e := range top {
			if i == 8 {
				break
			}
			fmt.Fprintf(&b, "\n  %8.1f MB  %s", float64(e.n)/(1<<20), e.name)
		}
		t.Fatalf("the build context is %.1f MB, over the %d MB budget. Every build of either "+
			"image tars and ships this to the daemon before the first instruction runs, and "+
			"scripts/k3s.sh runners does it once per family. Add the offender to "+
			".dockerignore, or raise contextBudget in this test and say why:%s",
			float64(total)/(1<<20), contextBudget>>20, b.String())
	}
}

// dockerignorePattern is one line of .dockerignore, split into segments.
type dockerignorePattern struct {
	segs    []string
	dirOnly bool
	negate  bool
}

// parseDockerignore reads the file into patterns. It implements the subset of
// BuildKit's matcher this repository's ignore file uses — a pattern is matched
// against the *whole* relative path (so `node_modules/` is the root one and
// `web/node_modules/` has to be written out), `*` does not cross a slash, and
// `**` matches any number of segments. Deliberately not the full syntax and
// deliberately not a new dependency: the thing being guarded is a number, and a
// matcher that silently disagrees with Docker on an exotic pattern would be
// worse than one that cannot read it at all — so an unsupported form fails the
// test rather than being skipped.
func parseDockerignore(t *testing.T, name string) []dockerignorePattern {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var out []dockerignorePattern
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := dockerignorePattern{}
		if strings.HasPrefix(line, "!") {
			p.negate, line = true, strings.TrimPrefix(line, "!")
		}
		if strings.HasSuffix(line, "/") {
			p.dirOnly, line = true, strings.TrimSuffix(line, "/")
		}
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			t.Fatalf("%s: empty pattern", name)
		}
		p.segs = strings.Split(line, "/")
		out = append(out, p)
	}
	return out
}

// dockerignored reports whether a path is excluded, which is true when the path
// itself matches. Callers skip whole directories, which is what makes a parent's
// match cover its children.
func dockerignored(rel string, isDir bool, pats []dockerignorePattern) bool {
	segs := strings.Split(rel, "/")
	excluded := false
	for _, p := range pats {
		if p.dirOnly && !isDir {
			continue
		}
		if matchSegments(p.segs, segs) {
			excluded = !p.negate
		}
	}
	return excluded
}

// matchSegments matches a segmented pattern against a segmented path, with `**`
// standing for any number of segments and path.Match for everything else.
func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], segs[1:])
}

// --- the build lock ----------------------------------------------------------

// TestTheImageBuildLockIsOneLock holds scripts/k3s.sh and
// scripts/publish-image.sh to the same lock path.
//
// Two files naming a lock is the whole mechanism, and a lock that is two locks
// is indistinguishable from no lock: both builds start, both succeed, and the
// only symptom is that each took twice as long on a box that had already run
// out of memory. Nothing errors, which is why this is a test and not a comment
// in one of them.
func TestTheImageBuildLockIsOneLock(t *testing.T) {
	re := regexp.MustCompile(`(?m)^IMAGE_LOCK=\$\{IMAGE_LOCK:-([^}]+)\}`)
	paths := map[string]string{}
	for _, f := range []string{"scripts/k3s.sh", "scripts/publish-image.sh"} {
		m := re.FindStringSubmatch(readDockerfile(t, f))
		if m == nil {
			t.Fatalf("%s: no `IMAGE_LOCK=${IMAGE_LOCK:-…}` line — the lock is gone, or it "+
				"moved and this guard is no longer reading anything", f)
		}
		paths[f] = m[1]
	}
	var first, firstFile string
	for f, p := range paths {
		if first == "" {
			first, firstFile = p, f
			continue
		}
		if p != first {
			t.Fatalf("%s locks on %q and %s locks on %q. Two locks is not a lock: both "+
				"builds start, both succeed, and the only symptom is that each took twice "+
				"as long on a box that had already run out of memory", firstFile, first, f, p)
		}
	}
}

// TestTheReleaseGateNamesAUrlStageAsAFile: a --url target's stage name is a
// log file name, and a slash after the host (a base path, a trailing /) made
// it a path in a directory that does not exist — the stage was recorded FAIL
// without running (security review 2026-11-15, SC-4). Both scripts sanitize the
// URL the same way.
func TestTheReleaseGateNamesAUrlStageAsAFile(t *testing.T) {
	const sanitize = `s|[^A-Za-z0-9.-]|-|g`
	for _, f := range []string{"scripts/release-gate.sh", "tests/e2e/run.sh"} {
		if !strings.Contains(readDockerfile(t, f), sanitize) {
			t.Errorf("%s no longer sanitizes a --url target with %s", f, sanitize)
		}
	}
	if strings.Contains(readDockerfile(t, "scripts/release-gate.sh"), `stage "url-${u#*://}"`) {
		t.Error("scripts/release-gate.sh names a --url stage from the raw URL again")
	}
}

// TestTheReleaseGateChecksTheSpaTheBinaryEmbeds: the gate rebuilds bin/toft,
// which embeds internal/spa/dist; comparing web/src with web/dist let an
// `npm run build` without the copy silence the warning while the binary served
// the old SPA (security review 2026-11-15, SC-5).
func TestTheReleaseGateChecksTheSpaTheBinaryEmbeds(t *testing.T) {
	gate := readDockerfile(t, "scripts/release-gate.sh")
	if !regexp.MustCompile(`find web/src -newer internal/spa/dist/index\.html`).MatchString(gate) {
		t.Error("scripts/release-gate.sh's staleness check does not compare web/src with internal/spa/dist/index.html")
	}
	if strings.Contains(gate, "web/dist/index.html") {
		t.Error("scripts/release-gate.sh still names web/dist/index.html, which the binary does not embed")
	}
	if !strings.Contains(readDockerfile(t, "internal/spa/embed.go"), "//go:embed dist/*") {
		t.Error("internal/spa no longer embeds dist/*; this guard and the gate name the wrong directory")
	}
}

// --- the runner bake ---------------------------------------------------------

// TestTheBakeBuildsTheFamiliesTheScriptDeclares holds docker/runner/bake.hcl
// against scripts/k3s.sh.
//
// A family the script names and the bake file does not is loud — `docker buildx
// bake` refuses an unknown target. The silent half is the assembly: the static
// family's base has no shell, so it is finished by a different stage, and a
// target built with ASSEMBLY=posix instead produces an image that builds
// cleanly and cannot run its own account setup. That is one word in one line of
// a file nobody reads while debugging a pod.
func TestTheBakeBuildsTheFamiliesTheScriptDeclares(t *testing.T) {
	script := readDockerfile(t, "scripts/k3s.sh")
	bake := readDockerfile(t, "docker/runner/bake.hcl")

	m := regexp.MustCompile(`(?m)^RUNNER_FAMILIES=\$\{RUNNER_FAMILIES:-([^}]+)\}`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("scripts/k3s.sh: no `RUNNER_FAMILIES=${RUNNER_FAMILIES:-…}` line — the " +
			"file's shape changed and this guard is no longer reading anything")
	}
	declared := strings.Fields(m[1])
	sort.Strings(declared)

	targets := findAll(t, "docker/runner/bake.hcl", bake,
		regexp.MustCompile(`(?m)^target "([a-z]+)"`))
	var families []string
	for _, tg := range targets {
		if strings.HasPrefix(tg, "_") {
			continue
		}
		families = append(families, tg)
	}
	sort.Strings(families)

	if !slices.Equal(declared, families) {
		t.Fatalf("scripts/k3s.sh builds %v and docker/runner/bake.hcl declares targets %v.\n"+
			"The script hands bake the families it decided were stale, so one it names and "+
			"the bake file does not is a runner pass that dies naming a target instead of "+
			"building an image", declared, families)
	}

	// The assembly of each target, read out of its args block.
	assembly := map[string]string{}
	for _, m := range regexp.MustCompile(
		`(?s)target "([a-z]+)" \{.*?ASSEMBLY = "([a-z]+)"`).FindAllStringSubmatch(bake, -1) {
		assembly[m[1]] = m[2]
	}
	for _, f := range families {
		want := "posix"
		if f == "static" {
			want = "static"
		}
		got, ok := assembly[f]
		if !ok {
			t.Fatalf("docker/runner/bake.hcl: target %q states no ASSEMBLY, so it takes the "+
				"Dockerfile's default. Say it, because the one family that needs the other "+
				"value is the one whose base has no shell to report the mistake", f)
		}
		if got != want {
			t.Fatalf("docker/runner/bake.hcl: target %q is built with ASSEMBLY=%q, want %q. "+
				"family-static's base is distroless — an image finished by assembly-posix "+
				"builds cleanly and cannot run the account setup inside it", f, got, want)
		}
	}
}

// Every toolchain the images download — Posit's R debs, nodejs.org's and
// go.dev's tarballs, Quarto's release — is checked against a SHA-256 written in
// the Dockerfile before it is installed, so a build runs the bytes this file
// names rather than whatever a CDN edge or a release page serves that day. The
// guard is per download: a fetch of one of those hosts must be followed, in the
// same RUN, by `sha256sum -c`. And where both images fetch the same version,
// they name the same hash, because they are the same file.
func TestBothImagesVerifyTheSameToolchainDownloads(t *testing.T) {
	download := regexp.MustCompile(`https://(?:cdn\.posit\.co/r/|nodejs\.org/dist/|go\.dev/dl/|github\.com/quarto-dev/)`)
	entry := regexp.MustCompile(`\b(\d+\.\d+\.\d+):(?:[^:\s]+:)?([0-9a-f]{64}):([0-9a-f]{64})\b`)
	seen := map[string]string{}
	for _, file := range []string{platformDockerfile, runnerDockerfile} {
		body := readDockerfile(t, file)
		lines := strings.Split(body, "\n")
		for i, l := range lines {
			if !download.MatchString(l) || strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			verified := false
			for j := i; j < len(lines) && j < i+4; j++ {
				if strings.Contains(lines[j], "sha256sum -c") {
					verified = true
				}
			}
			if !verified {
				t.Errorf("%s:%d downloads a toolchain with no sha256sum -c after it: %s", file, i+1, strings.TrimSpace(l))
			}
		}
		for _, m := range entry.FindAllStringSubmatch(body, -1) {
			key, sums := m[1], m[2]+":"+m[3]
			if prev, ok := seen[key]; ok && prev != sums {
				t.Errorf("%s names different SHA-256s for %s than the other image — the same file has one hash", file, key)
			}
			seen[key] = sums
		}
	}
	for _, v := range []string{"4.6.2", "4.5.3", "4.4.3", "24.22.2", "22.23.4", "1.26.9"} {
		if _, ok := seen[v]; !ok {
			t.Errorf("no pinned SHA-256 found for %s; the guard is no longer reading the download tables", v)
		}
	}
}
