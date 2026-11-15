package builder

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectNextBasePathWritesConfigWhenAbsent(t *testing.T) {
	dir := t.TempDir()

	if err := injectNextBasePath(dir, "/apps/my-app"); err != nil {
		t.Fatalf("injectNextBasePath: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "next.config.js"))
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	if !strings.Contains(string(data), `basePath: "/apps/my-app"`) {
		t.Errorf("generated config missing basePath:\n%s", data)
	}
}

func TestInjectNextBasePathWrapsExistingCommonJSConfig(t *testing.T) {
	dir := t.TempDir()
	original := "module.exports = { reactStrictMode: true }\n"
	write(t, dir, "next.config.js", original)

	if err := injectNextBasePath(dir, "/apps/my-app"); err != nil {
		t.Fatalf("injectNextBasePath: %v", err)
	}

	// The user's config is preserved verbatim, moved aside.
	moved := read(t, dir, "next.config.toft-user.js")
	if moved != original {
		t.Errorf("user config was altered:\n%s", moved)
	}

	wrapper := read(t, dir, "next.config.js")
	if !strings.Contains(wrapper, "require('./next.config.toft-user.js')") {
		t.Errorf("wrapper does not load the user config:\n%s", wrapper)
	}
	if !strings.Contains(wrapper, `basePath = "/apps/my-app"`) {
		t.Errorf("wrapper missing basePath:\n%s", wrapper)
	}
	if strings.Contains(wrapper, "import ") {
		t.Errorf("CommonJS wrapper must not use ESM syntax:\n%s", wrapper)
	}
}

func TestInjectNextBasePathUsesESMForModulePackages(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"type":"module"}`)
	write(t, dir, "next.config.js", "export default { reactStrictMode: true }\n")

	if err := injectNextBasePath(dir, "/apps/my-app"); err != nil {
		t.Fatalf("injectNextBasePath: %v", err)
	}

	wrapper := read(t, dir, "next.config.js")
	if !strings.Contains(wrapper, "import userConfig from './next.config.toft-user.js'") {
		t.Errorf("expected ESM import in wrapper:\n%s", wrapper)
	}
	if strings.Contains(wrapper, "require(") {
		t.Errorf("ESM wrapper must not use require:\n%s", wrapper)
	}
}

func TestInjectNextBasePathHandlesMJS(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "next.config.mjs", "export default {}\n")

	if err := injectNextBasePath(dir, "/apps/x"); err != nil {
		t.Fatalf("injectNextBasePath: %v", err)
	}

	wrapper := read(t, dir, "next.config.mjs")
	if !strings.Contains(wrapper, "export default") {
		t.Errorf("mjs wrapper must be ESM:\n%s", wrapper)
	}
	if !fileExists(filepath.Join(dir, "next.config.toft-user.mjs")) {
		t.Error("user config was not moved aside")
	}
}

func TestInjectNextBasePathDropsExtensionForTypeScript(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "next.config.ts", "export default {}\n")

	if err := injectNextBasePath(dir, "/apps/x"); err != nil {
		t.Fatalf("injectNextBasePath: %v", err)
	}

	wrapper := read(t, dir, "next.config.ts")
	// A .ts extension in the import specifier is not resolvable.
	if strings.Contains(wrapper, "toft-user.ts'") {
		t.Errorf("TypeScript import must omit the extension:\n%s", wrapper)
	}
	if !strings.Contains(wrapper, "from './next.config.toft-user'") {
		t.Errorf("unexpected import specifier:\n%s", wrapper)
	}
}

func TestInjectNextBasePathIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "next.config.js", "module.exports = { reactStrictMode: true }\n")

	if err := injectNextBasePath(dir, "/apps/x"); err != nil {
		t.Fatalf("first inject: %v", err)
	}
	first := read(t, dir, "next.config.js")

	if err := injectNextBasePath(dir, "/apps/x"); err != nil {
		t.Fatalf("second inject: %v", err)
	}

	// A second pass must not wrap the wrapper.
	if got := read(t, dir, "next.config.js"); got != first {
		t.Errorf("config changed on re-run:\n%s", got)
	}
	if got := read(t, dir, "next.config.toft-user.js"); !strings.Contains(got, "reactStrictMode") {
		t.Errorf("user config lost on re-run:\n%s", got)
	}
}

// A rebuild copies the live release as its source, wrapper included. The
// wrapper carries the path it was built for, so a tree wrapped for the app's
// first slug must be rewrapped for the current one — the early return that used
// to stand here is how a renamed Next app stayed built for its first address.
func TestInjectNextBasePathRewrapsForANewPath(t *testing.T) {
	for _, tc := range []struct{ name, file, source string }{
		{"user config", "next.config.js", "module.exports = { reactStrictMode: true }\n"},
		{"user mjs", "next.config.mjs", "export default {}\n"},
		{"no config", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "" {
				write(t, dir, tc.file, tc.source)
			}
			if err := injectNextBasePath(dir, "/apps/old"); err != nil {
				t.Fatalf("first inject: %v", err)
			}
			if err := injectNextBasePath(dir, "/apps/new"); err != nil {
				t.Fatalf("second inject: %v", err)
			}
			cfg := "next.config.js"
			if tc.file != "" {
				cfg = tc.file
			}
			got := read(t, dir, cfg)
			if strings.Contains(got, "/apps/old") || !strings.Contains(got, `"/apps/new"`) {
				t.Errorf("config not rewritten for the new path:\n%s", got)
			}
			if tc.file != "" {
				ext := filepath.Ext(tc.file)
				if moved := read(t, dir, "next.config.toft-user"+ext); moved != tc.source {
					t.Errorf("user config altered on rewrap:\n%s", moved)
				}
			} else if fileExists(filepath.Join(dir, "next.config.toft-user.js")) {
				t.Error("a generated standalone config was wrapped as though the author wrote it")
			}
		})
	}
}

// At "" the app is served at / on a hostname of its own, and the tree goes
// back to exactly what its author wrote.
func TestInjectNextBasePathUnwrapsAtRoot(t *testing.T) {
	t.Run("user config", func(t *testing.T) {
		dir := t.TempDir()
		original := "module.exports = { reactStrictMode: true }\n"
		write(t, dir, "next.config.js", original)
		if err := injectNextBasePath(dir, "/apps/x"); err != nil {
			t.Fatal(err)
		}
		if err := injectNextBasePath(dir, ""); err != nil {
			t.Fatal(err)
		}
		if got := read(t, dir, "next.config.js"); got != original {
			t.Errorf("user config not restored byte-for-byte:\n%s", got)
		}
		if fileExists(filepath.Join(dir, "next.config.toft-user.js")) {
			t.Error("the moved-aside copy was left behind")
		}
		// And back again: a move off the hostname wraps it afresh.
		if err := injectNextBasePath(dir, "/apps/y"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(read(t, dir, "next.config.js"), `"/apps/y"`) {
			t.Error("rewrap after unwrap lost the path")
		}
	})
	t.Run("generated config", func(t *testing.T) {
		dir := t.TempDir()
		if err := injectNextBasePath(dir, "/apps/x"); err != nil {
			t.Fatal(err)
		}
		if err := injectNextBasePath(dir, ""); err != nil {
			t.Fatal(err)
		}
		if fileExists(filepath.Join(dir, "next.config.js")) {
			t.Error("the generated standalone config was not removed")
		}
	})
	t.Run("never wrapped", func(t *testing.T) {
		dir := t.TempDir()
		original := "module.exports = {}\n"
		write(t, dir, "next.config.js", original)
		if err := injectNextBasePath(dir, ""); err != nil {
			t.Fatal(err)
		}
		if got := read(t, dir, "next.config.js"); got != original {
			t.Errorf("an unwrapped config was touched:\n%s", got)
		}
	})
}

// A config may be an object, a function, or a function returning a promise.
// The last one is the regression this guards: `{...somePromise}` is `{}`, so a
// wrapper that spreads without resolving deploys the app with basePath and none
// of its own settings, silently.
func TestInjectNextBasePathPreservesEveryConfigShape(t *testing.T) {
	cases := []struct {
		name, file, source string
	}{
		{"cjs object", "next.config.js", `module.exports = { env: { MARKER: 'kept' } }`},
		{"cjs function", "next.config.js", `module.exports = () => ({ env: { MARKER: 'kept' } })`},
		{"cjs async function", "next.config.js", `module.exports = async () => ({ env: { MARKER: 'kept' } })`},
		{"esm object", "next.config.mjs", `export default { env: { MARKER: 'kept' } }`},
		{"esm function", "next.config.mjs", `export default () => ({ env: { MARKER: 'kept' } })`},
		{"esm async function", "next.config.mjs", `export default async () => ({ env: { MARKER: 'kept' } })`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.file, tc.source+"\n")

			if err := injectNextBasePath(dir, "/apps/my-app"); err != nil {
				t.Fatalf("injectNextBasePath: %v", err)
			}

			cfg, _ := evalNextConfig(t, filepath.Join(dir, tc.file))
			if got := cfg["basePath"]; got != "/apps/my-app" {
				t.Errorf("basePath = %v, want /apps/my-app", got)
			}
			env, _ := cfg["env"].(map[string]any)
			if env["MARKER"] != "kept" {
				t.Errorf("user config was discarded: %v", cfg)
			}
		})
	}
}

// `output` cannot be honored here — the app is started with `next start` — so
// the wrapper says so in the build log rather than letting the developer read
// a Next.js error that names none of this.
func TestInjectNextBasePathWarnsAboutOutput(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "next.config.js", "module.exports = { output: 'export' }\n")

	if err := injectNextBasePath(dir, "/apps/x"); err != nil {
		t.Fatalf("injectNextBasePath: %v", err)
	}

	cfg, stderr := evalNextConfig(t, filepath.Join(dir, "next.config.js"))
	if !strings.Contains(stderr, "output") {
		t.Errorf("no warning about output:\n%s", stderr)
	}
	// Warned about, not overridden: silently rewriting an app's config is worse
	// than a build that fails for a stated reason.
	if cfg["output"] != "export" {
		t.Errorf("output was rewritten: %v", cfg["output"])
	}
}

// Belt and braces for installs without node in PATH, where the tests above
// skip: a refactor that drops the promise branch still fails here.
func TestNextConfigWrappersResolvePromises(t *testing.T) {
	for name, wrapper := range map[string]string{
		"commonjs": commonJSWrapper("next.config.toft-user.js", "/apps/x"),
		"esm":      esmWrapper("next.config.toft-user.mjs", "/apps/x"),
	} {
		if !strings.Contains(wrapper, "typeof resolved.then === 'function'") {
			t.Errorf("%s wrapper does not resolve a promise-returning config:\n%s", name, wrapper)
		}
	}
}

// evalNextConfig loads a generated wrapper the way Next.js does — calling it
// when it is a function and awaiting whatever comes back — and returns the
// resolved config plus anything written to stderr.
func evalNextConfig(t *testing.T, path string) (map[string]any, string) {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; wrapper behaviour not exercised")
	}

	const script = `const p = process.argv[1]
const load = p.endsWith('.mjs') ? import(p).then((m) => m.default) : Promise.resolve(require(p))
load
  .then((cfg) => (typeof cfg === 'function' ? cfg('phase-production-build', {}) : cfg))
  .then((resolved) => process.stdout.write(JSON.stringify(resolved)))
  .catch((err) => { process.stderr.write(String(err)); process.exit(1) })`

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(node, "-e", script, path)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("evaluating %s: %v\n%s", path, err, stderr.String())
	}

	var cfg map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &cfg); err != nil {
		t.Fatalf("parsing resolved config %q: %v", stdout.String(), err)
	}
	return cfg, stderr.String()
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

// Which frameworks keep the prefix is a claim about what the *upstream* expects
// to receive, and getting it backwards is a 404 on every page of the app.
//
// The list is asserted whole rather than one entry at a time because that is
// how it went wrong: Shiny was added to it on the strength of a correct
// observation — that Shiny emits prefixed URLs — which is a fact about what the
// browser is *sent*, not about what the app can *serve*. Shiny mounts at / and
// has no baseUrlPath option, so it answered 404 for /apps/{slug}/ while its pod
// logged a clean `Listening on http://0.0.0.0:8080`. Nothing in a unit test, a
// build log or an app log said anything was wrong.
//
// A framework belongs here only if it was *told* its mount point — Next.js via
// basePath, Streamlit via --server.baseUrlPath, marimo via --base-url. If the
// answer to "what tells it?" is "nothing", it does not keep the prefix.
func TestOnlyFrameworksToldTheirMountPointKeepThePrefix(t *testing.T) {
	want := map[string]bool{
		string(FrameworkNextJS):    true,
		string(FrameworkStreamlit): true,
		string(FrameworkMarimo):    true,
	}
	for _, framework := range []string{
		string(FrameworkNextJS), string(FrameworkStreamlit), string(FrameworkMarimo),
		string(FrameworkShiny), string(FrameworkPlumber), string(FrameworkRMarkdown),
		string(FrameworkQuarto), string(FrameworkStatic), string(FrameworkNotebook),
		string(FrameworkFlask), string(FrameworkFastAPI),
	} {
		if got := KeepsPrefix(framework); got != want[framework] {
			t.Errorf("KeepsPrefix(%q) = %v, want %v — a framework keeps the prefix only if "+
				"something told it its mount point", framework, got, want[framework])
		}
	}
}
