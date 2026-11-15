package sdk_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"toft/sdk"
)

// **Every key an SDK reads out of `/api/v1/app/self` is a key the handler
// sends.**
//
// The sibling guard in contract_test.go pins the *routes* an SDK calls; this
// pins the fields it reads off one of them, and it exists because the field
// that went missing was the one an app is least able to survive being wrong
// about. `toft.app.base_path` was documented as where the app is mounted, was
// read from this payload, and the payload never carried the key — so all four
// SDKs fell back to "" and reported it as fact. An app that built its links
// from the accessor emitted root-absolute URLs, which the proxy answers with a
// 404 on every link, with nothing in any log saying why. And "" is also the
// legitimate answer on an install that gives each app its own origin, so no app
// could tell "not mounted" from "the platform did not tell me".
//
// Nothing compared the two halves. Both were individually reasonable — a
// handler that sends what it means to, and an accessor with a default — and the
// defect lived entirely in the gap between them.
//
// Textual on purpose, for the reason the route guard is: reaching for the
// router would mean standing up a server, which is heavier than the failure
// warrants, and the failure is not subtle.
//
// The comparison runs one way only. A key the platform sends and no SDK reads
// is not a defect — it is a field waiting for a reader, or one only the HTTP
// caller wants.
func TestEveryAppSelfKeyAnSDKReadsIsOneThePlatformSends(t *testing.T) {
	sent := appSelfKeys(t)
	for file, keys := range appSelfReaders(t) {
		for _, key := range keys {
			if sent[key] {
				continue
			}
			t.Errorf("%s reads %q out of GET /api/v1/app/self, which handleAppSelf does "+
				"not send.\n    sent: %s", file, key, strings.Join(sortedSet(sent), " "))
		}
	}
}

// appSelfKeys is what handleAppSelf actually writes, read out of the one
// writeJSON literal that is its whole answer.
func appSelfKeys(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "internal", "api", "app_surface_handlers.go"))
	if err != nil {
		t.Fatalf("reading app_surface_handlers.go: %v", err)
	}
	body := regexp.MustCompile(`(?s)func \(s \*Server\) handleAppSelf\(.*?\n\}`).Find(raw)
	if body == nil {
		t.Fatal("could not find handleAppSelf; it was renamed and this test now compares " +
			"against nothing")
	}
	payload := regexp.MustCompile(`(?s)writeJSON\(w, http\.StatusOK, map\[string\]interface\{\}\{.*`).
		Find(body)
	if payload == nil {
		t.Fatal("handleAppSelf no longer answers with a map literal; this guard reads one")
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z_]+)":`).FindAllStringSubmatch(string(payload), -1) {
		out[m[1]] = true
	}
	if len(out) < 4 {
		t.Fatalf("only %d keys found in handleAppSelf's answer; the regex has stopped "+
			"matching", len(out))
	}
	return out
}

// appSelfReadKey is how each language spells "the key I want out of the app's
// own view": Python and Node index the cached dict, Go declares a json tag on
// AppInfo, and R reaches into the list app_info() returns.
var appSelfReadKey = map[string]*regexp.Regexp{
	filepath.Join("python", "toft", "appinfo.py"): regexp.MustCompile(`_load\(\)\.get\("([a-z_]+)"`),
	filepath.Join("node", "app.js"):                    regexp.MustCompile(`#load\(\)\)\.([a-z_]+)`),
	filepath.Join("go", "app.go"):                      regexp.MustCompile("`json:\"([a-z_]+)\"`"),
	filepath.Join("r", "R", "app.R"):                   regexp.MustCompile(`app_info\(\)\$([a-z_]+)`),
}

func appSelfReaders(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for file, pattern := range appSelfReadKey {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		source := stripComments(filepath.Ext(file), string(raw))
		matches := pattern.FindAllStringSubmatch(source, -1)
		if matches == nil {
			t.Fatalf("%s: nothing matched %s — the accessor was rewritten and this half "+
				"of the comparison is now empty", file, pattern)
		}
		for _, m := range matches {
			out[file] = append(out[file], m[1])
		}
	}
	return out
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// **The three SDKs on one version line carry the same number.**
//
// They are one product in three languages: a change to the wire, or to what an
// accessor promises, lands in all of them in one commit. A version that moves
// in one and not the others is how an app author reads a changelog that does
// not describe the package they installed — and the 0.6.0 audit hit the shape
// of this from the other side, reporting the SDK as "byte-identical at 0.2.0"
// while the platform underneath it had moved twice.
//
// R is deliberately not here. It carries a CRAN-style `Version:` on a scheme of
// its own, so pinning it to this line would be asserting a relationship that
// does not exist.
func TestTheSDKsOnOneVersionLineAgree(t *testing.T) {
	spellings := map[string]*regexp.Regexp{
		filepath.Join("python", "toft", "__init__.py"): regexp.MustCompile(`__version__ = "([^"]*)"`),
		filepath.Join("node", "package.json"):               regexp.MustCompile(`"version":\s*"([^"]*)"`),
		filepath.Join("go", "toft.go"):                        regexp.MustCompile(`const Version = "([^"]*)"`),
	}
	seen := map[string]string{}
	for file, pattern := range spellings {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		m := pattern.FindSubmatch(raw)
		if m == nil {
			t.Fatalf("%s: no version matching %s — it was renamed, and this guard now "+
				"compares two of three", file, pattern)
		}
		seen[file] = string(m[1])
	}
	var first, at string
	for file, v := range seen {
		if first == "" {
			first, at = v, file
			continue
		}
		if v != first {
			t.Errorf("%s is %s and %s is %s — one SDK moved and the others did not, so an "+
				"app author reads a changelog that does not describe what they installed",
				at, first, file, v)
		}
	}
}

// **Within one language, every place that states a version states the same
// one.**
//
// Python says it twice — `__version__` for anything that imports the package,
// and `pyproject.toml` for the distribution `uv pip install` builds — and the
// two disagreed: the module said 0.3.0 while `pip show toft-sdk` said
// 0.2.0, so the build log printed one number and the installed wheel recorded
// another. That is the same defect this whole file exists for, one scope down,
// and it was found by reading a real build log rather than by anything here.
//
// The other three languages state it once, which is why they are not in the
// table: a rule can only be broken where there are two spellings to disagree.
func TestOneLanguageStatesOneVersion(t *testing.T) {
	spellings := map[string]map[string]*regexp.Regexp{
		"python": {
			filepath.Join("python", "toft", "__init__.py"): regexp.MustCompile(`__version__ = "([^"]*)"`),
			filepath.Join("python", "pyproject.toml"):           regexp.MustCompile(`(?m)^version = "([^"]*)"`),
		},
	}
	for lang, files := range spellings {
		seen := map[string]string{}
		for file, pattern := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("reading %s: %v", file, err)
			}
			m := pattern.FindSubmatch(raw)
			if m == nil {
				t.Fatalf("%s: nothing matched %s — a spelling was renamed and this guard "+
					"now compares one thing against itself", file, pattern)
			}
			seen[file] = string(m[1])
		}
		var first, at string
		for file, v := range seen {
			if first == "" {
				first, at = v, file
				continue
			}
			if v != first {
				t.Errorf("%s states two versions: %s is %s and %s is %s. The build log "+
					"prints one and the installed distribution records the other",
					lang, at, first, file, v)
			}
		}
	}
}

// **The SDK's identity is derived from its bytes, not declared beside them.**
//
// The SDK reaches an app by two independent build paths — `internal/builder` on
// the platform host, and `internal/runner` inside the family image — and the two
// binaries are built and deployed separately, which is the property the family
// image exists to have. An install upgraded on one side and not the other
// serves one SDK from `GET /api/v1/sdk/{lang}`, which is what `toft sdk vendor`
// hands a developer, and installs a different one into every app's venv.
//
// Both call themselves the same version, because a version is a declaration and
// both trees declared it. That is what made the skew unfindable: it was caught
// by reading `inspect.getsource` inside a running process, and nothing in the
// build log, the deploy result or `toft status` mentioned an SDK at all.
func TestTheSDKBuildIdentityChangesWithTheBytes(t *testing.T) {
	for _, lang := range []string{"python", "node", "go", "r"} {
		first, err := sdk.Build(lang)
		if err != nil {
			t.Fatalf("Build(%q): %v", lang, err)
		}
		if len(first) != 12 {
			t.Errorf("Build(%q) = %q, want twelve characters — it is read in a build "+
				"log and compared by eye", lang, first)
		}
		// Stable: two calls on one tree agree, which is what makes a
		// disagreement between two binaries mean what it says.
		if again, _ := sdk.Build(lang); again != first {
			t.Errorf("Build(%q) is not stable: %q then %q", lang, first, again)
		}
		if got := sdk.Describe(lang); !strings.Contains(got, first) {
			t.Errorf("Describe(%q) = %q, which does not carry the build %q — the log "+
				"line is the only place the two copies can be compared", lang, got, first)
		}
		if v := sdk.Version(lang); v == "" || !strings.HasPrefix(sdk.Describe(lang), v) {
			t.Errorf("Describe(%q) = %q, want it to lead with the declared version %q",
				lang, sdk.Describe(lang), v)
		}
	}

	// No two languages share an identity, or a skew in one would read as
	// agreement in another.
	seen := map[string]string{}
	for _, lang := range []string{"python", "node", "go", "r"} {
		id, _ := sdk.Build(lang)
		if other, clash := seen[id]; clash {
			t.Errorf("%s and %s have the same build identity %q", lang, other, id)
		}
		seen[id] = lang
	}

	// A payload this binary does not carry has no identity, and describing it
	// must not panic or invent one: BuildOrEmpty is for the callers that are
	// describing rather than deciding.
	if got := sdk.BuildOrEmpty("cobol"); got != "" {
		t.Errorf("BuildOrEmpty of a language this binary has no SDK for = %q", got)
	}
}

// **Both build paths say which SDK they installed, in the same words.**
//
// Two binaries, one sentence: a reader comparing a build log against what `toft
// sdk vendor` printed has to be comparing the same thing. A path that installs
// the SDK and does not say so is the state this whole finding lived in.
func TestBothBuildPathsNameTheSDKTheyInstall(t *testing.T) {
	for _, file := range []string{
		filepath.Join("..", "internal", "builder", "dependencies.go"),
		filepath.Join("..", "internal", "builder", "builder.go"),
		filepath.Join("..", "internal", "runner", "run.go"),
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		source := string(raw)
		installs := strings.Count(source, "Toft SDK")
		if installs == 0 {
			continue
		}
		if !strings.Contains(source, "sdk.Describe(") {
			t.Errorf("%s installs the Toft SDK and never names which copy — "+
				"sdk.Describe is the one spelling, so two build logs can be compared",
				file)
		}
	}
}
