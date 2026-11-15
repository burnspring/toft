package builder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"toft/internal/config"
	"toft/internal/database"
)

// Toft serves every app under /apps/{slug}. Rather than rewriting response
// bodies at the proxy, each framework is told its mount point using whatever
// native mechanism it provides:
//
//	Next.js    basePath in next.config.*        (build time)
//	Streamlit  --server.baseUrlPath             (start time)
//	Shiny      nothing — it reads its own URL    (start time, prefix kept)
//	marimo     --base-url                       (start time)
//	Notebook   nothing — a file server, and the document is one page
//	FastAPI    uvicorn --root-path              (start time, proxy strips path)
//	Flask      X-Forwarded-Prefix + ProxyFix    (start time, proxy strips path)
//	Go         BASE_PATH env var                (start time, proxy strips path)
//	Node/other BASE_PATH env var                (start time, proxy strips path)
//
// Frameworks in keepPrefixFrameworks generate fully-prefixed URLs and therefore
// expect to receive the prefixed path too. Everything else is mounted at / and
// the proxy strips the prefix before forwarding.
var keepPrefixFrameworks = map[string]bool{
	string(FrameworkNextJS):    true,
	string(FrameworkStreamlit): true,
	// marimo is given --base-url and generates fully-prefixed URLs from it,
	// exactly as Streamlit does with --server.baseUrlPath.
	string(FrameworkMarimo): true,
}

// **Shiny is deliberately not in that map, and it was.** `runApp` has no
// baseUrlPath option — Shiny Server solves the problem by rewriting at its own
// proxy — so a Shiny app is told nothing and mounts at /. Forwarding it
// /apps/{slug}/ gets a 404 from the app itself, which is what the entry
// produced: every Shiny app on the install answered "not found" while its pod
// logged `Listening on http://0.0.0.0:8080` and nothing else.
//
// What made the entry look right is that Shiny *does* emit prefixed URLs — but
// it emits them by inheriting the document's own path rather than by being told
// a mount point, which is the opposite property:
//
//   - every asset URL is *relative with no leading slash*
//     (href="bootstrap-3.4.1/css/bootstrap.min.css"), so the browser resolves
//     it against the document's base; and
//   - shiny.min.js builds the WebSocket URL from window.location.pathname,
//     forcing a trailing slash and appending "websocket/".
//
// So both halves land back on /apps/{slug}/… — and they need the proxy to
// strip that before it reaches an app mounted at /. The map is about what the
// *upstream* expects to receive, never about what the browser is sent.
//
// This is also exactly why Shiny is in needsTrailingSlash below: relative assets
// are only correct when the document's own URL ends in a slash.

// needsTrailingSlash lists the frameworks whose mount path must end in a slash
// before anything is served at it.
//
// A static site's HTML resolves `href="styles.css"` against the page's own URL,
// so at /apps/{slug} the browser asks for /apps/styles.css — one directory too
// high, and off this app entirely. The redirect is issued *here* rather than by
// the file server because the proxy collapses /apps/{slug} and /apps/{slug}/
// into the same upstream path ("/"), so by the time the workload sees the
// request the distinction is gone. Every deeper directory redirect does come
// from the file server, and rewriteLocation puts the prefix back on it.
//
// Scoped to the frameworks that need it rather than applied to all of them: for
// a Flask or Node app both forms already reach the app's own root route, and a
// redirect nobody asked for would change behaviour for every app on the install.
var needsTrailingSlash = map[string]bool{
	string(FrameworkStatic): true,
	// A rendered notebook is a folder of HTML served by the same file server,
	// so it has the same relative-URL problem for the same reason.
	string(FrameworkNotebook): true,
	// marimo is here for a different reason, and it is a measurement rather
	// than a guess. Asked for the bare mount path it issues a 307 of its own —
	// and it builds that Location as an *absolute* URL from the Host header,
	// which this proxy rewrites to the upstream address. Since marimo keeps its
	// prefix, ModifyResponse leaves Location alone, so the browser would be
	// sent to 127.0.0.1:<upstream port>. Redirecting here, before anything is
	// proxied, means marimo never gets the chance to answer that request.
	string(FrameworkMarimo): true,
	// The two R document frameworks, for the reason the notebook above is
	// here: a rendered document is a folder of HTML served by the same file
	// server, with the same relative-URL problem.
	string(FrameworkRMarkdown): true,
	string(FrameworkQuarto):    true,
	// Shiny, and this is the half a document could not have settled. Its assets
	// are relative with no leading slash, so at /apps/{slug} the browser
	// resolves href="bootstrap-3.4.1/…" against /apps/ and asks for
	// /apps/bootstrap-3.4.1/… — one directory too high and off this app
	// entirely. The page still arrives, which is what makes this worth a
	// redirect rather than a comment: a Shiny app served without the slash
	// renders as unstyled HTML with no working inputs, and the WebSocket
	// connects, so nothing in the app's log says anything is wrong.
	string(FrameworkShiny): true,
}

// BasePath is the URL prefix an app is served under on the gateway.
func BasePath(slug string) string {
	return "/apps/" + slug
}

// BasePathFor is where an app *should* live: "" at the root of its own
// hostname when one is switched on, /apps/{slug} otherwise.
//
// **Only the build calls this**, and records the answer on the deployment row
// and — at the flip — on the app. Everything that serves, starts or describes
// the running code reads ServingBasePath instead. The two differ for the length
// of every move, and a reader that took this one would start a woken app at an
// address its tree was not built for: for Next.js, a blank page; for every
// keep-prefix framework, how a rename used to leave an app answering 404.
func BasePathFor(app *database.App) string {
	if app.HostnameEnabled && app.Hostname != "" {
		return ""
	}
	return BasePath(app.Slug)
}

// ServingBasePath is the base path the serving release was built for — the
// fact, where BasePathFor is the wish. An app whose row predates the column,
// or that has no serving release, answers its gateway path, which is what
// every such release was built for.
func ServingBasePath(app *database.App) string {
	if app.ReleaseBasePath != nil {
		return *app.ReleaseBasePath
	}
	return BasePath(app.Slug)
}

// PublicURL is the whole address a visitor is sent to for an app — the origin
// deployed apps are served from, plus the path this file already owns.
//
// **One construction, because the origin is not derivable.** Apps live on
// their own origin on a split-origin install and on the platform's on a
// single-origin one, and the difference is configuration rather than anything
// a caller can work out: `deriveAppsBaseURL` makes the two collapse when
// APPS_PORT is the platform's port, and an administrator behind a proxy may
// have named a third address entirely. So the base is always passed in — it is
// `config.AppsBaseURL`, which is the value the proxy itself redirects to — and
// what this adds is the half that belongs here.
//
// Every caller that composes an app's address goes through this: the estate's
// scan targets, the reachability payload the vendor probes, and the link in an
// app's outbound mail. A second copy of `base + "/apps/" + slug` is how one of
// them ends up pointing at a 404 on an install shaped differently from the one
// it was written on, and reporting that 404 as the app's own answer.
//
// Empty when either half is missing, which every caller reads as "this app has
// no address to give" rather than as a URL with a hole in it.
//
// **An app whose release lives at the root of its own hostname answers that
// origin**, by the same rule the gateway's 308 is keyed on: the serving
// release, not the switch. Mid-move the old address is still the one that
// serves, so it is still the one given out.
func PublicURL(appsBaseURL string, app *database.App) string {
	base := strings.TrimSuffix(appsBaseURL, "/")
	if base == "" || app == nil || app.Slug == "" {
		return ""
	}
	if app.Hostname != "" && ServingBasePath(app) == "" {
		return config.AppHostOriginFor(appsBaseURL, app.Hostname)
	}
	return base + BasePath(app.Slug)
}

// NeedsTrailingSlash reports whether a request for the bare mount path must be
// redirected to the same path with a slash before it is proxied.
func NeedsTrailingSlash(framework string) bool {
	return needsTrailingSlash[framework]
}

// KeepsPrefix reports whether the framework expects the proxy to forward the
// full /apps/{slug} path rather than stripping it.
func KeepsPrefix(framework string) bool {
	return keepPrefixFrameworks[framework]
}

const (
	userConfigMarker = "next.config.toft-user"
	generatedHeader  = "// Generated by Toft — do not edit. Wraps the app's own Next.js config to set basePath.\n"
)

// injectNextBasePath makes a Next.js app aware that it is served under a
// sub-path. Next resolves basePath at build time, so this must run before
// `npm run build`.
//
// If the app has no next.config.*, one is written. If it has one, the file is
// moved aside and replaced with a wrapper that merges basePath over whatever
// the app exported — this preserves arbitrary user config without parsing JS.
//
// **A tree that was already wrapped is rewrapped, never skipped.** A rebuild of
// an uploaded app — and every move — copies the live release as its source,
// wrapper included, and that wrapper carries the path it was built for as a
// literal. Returning early here is how a renamed Next app stayed built for its
// first slug forever. So the user's config, already aside, is left where it is
// and only the wrapper is written again with today's path.
//
// At basePath "" — an app on a hostname of its own, served at / — there is
// nothing to merge, so the tree is put back the way its author left it: the
// user's config moved back over the wrapper, or the generated standalone
// config removed.
func injectNextBasePath(srcDir, basePath string) error {
	for _, ext := range []string{".js", ".mjs", ".cjs", ".ts"} {
		userName := userConfigMarker + ext
		if !fileExists(filepath.Join(srcDir, userName)) {
			continue
		}
		wrapperPath := filepath.Join(srcDir, "next.config"+ext)
		if basePath == "" {
			if err := os.Rename(filepath.Join(srcDir, userName), wrapperPath); err != nil {
				return fmt.Errorf("restoring %s: %w", "next.config"+ext, err)
			}
			return nil
		}
		return os.WriteFile(wrapperPath, []byte(generatedHeader+wrapperFor(srcDir, userName, ext, basePath)), 0644)
	}

	existing := ""
	for _, name := range []string{"next.config.js", "next.config.mjs", "next.config.cjs", "next.config.ts"} {
		if fileExists(filepath.Join(srcDir, name)) {
			existing = name
			break
		}
	}

	// A standalone config this function wrote on an earlier pass is ours, not
	// the app's: rewrite it rather than wrapping it as though the author had.
	if existing == "next.config.js" && isGeneratedConfig(srcDir, existing) {
		existing = ""
		if basePath == "" {
			return os.Remove(filepath.Join(srcDir, "next.config.js"))
		}
	}

	if basePath == "" {
		return nil
	}

	if existing == "" {
		return os.WriteFile(
			filepath.Join(srcDir, "next.config.js"),
			[]byte(generatedHeader+standaloneConfig(basePath)),
			0644,
		)
	}

	ext := filepath.Ext(existing)
	userName := userConfigMarker + ext
	if err := os.Rename(filepath.Join(srcDir, existing), filepath.Join(srcDir, userName)); err != nil {
		return fmt.Errorf("moving %s aside: %w", existing, err)
	}

	return os.WriteFile(filepath.Join(srcDir, existing), []byte(generatedHeader+wrapperFor(srcDir, userName, ext, basePath)), 0644)
}

func wrapperFor(srcDir, userName, ext, basePath string) string {
	if isESM(srcDir, ext) {
		return esmWrapper(userName, basePath)
	}
	return commonJSWrapper(userName, basePath)
}

// isGeneratedConfig reports whether a next.config was written by this file.
func isGeneratedConfig(srcDir, name string) bool {
	data, err := readInRelease(srcDir, name)
	return err == nil && strings.HasPrefix(string(data), generatedHeader)
}

// isESM reports whether a config with the given extension is loaded as an ES
// module: .mjs and .ts always are, .js only when package.json says so.
func isESM(srcDir, ext string) bool {
	switch ext {
	case ".mjs", ".ts":
		return true
	case ".cjs":
		return false
	}

	data, err := readInRelease(srcDir, "package.json")
	if err != nil {
		return false
	}
	var pkg struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}
	return pkg.Type == "module"
}

func standaloneConfig(basePath string) string {
	return fmt.Sprintf("module.exports = { basePath: %q }\n", basePath)
}

// mergeBody is the half of the wrapper that does not depend on the module
// system, kept in one place so the CommonJS and ESM forms cannot drift.
//
// Next.js accepts three shapes of config: an object, a (phase, ctx) function,
// and a function returning a promise. The third is why `merge` exists rather
// than a bare spread — spreading a Promise yields `{}`, so an app exporting
// `async () => ({ … })` would deploy with basePath and *none of its own
// settings*, with nothing logged anywhere. The promise is only introduced when
// the app's own config already returned one; a synchronous config stays
// synchronous.
//
// `output` is warned about rather than overridden: the app is started with
// `next start`, which ignores a standalone build, and `export` cannot build
// route handlers at all — so the setting is at best dead weight and at worst a
// build failure whose message names none of this.
const mergeBody = `const withBasePath = (cfg) => {
  cfg = cfg || {}
  if (cfg.output) {
    console.warn(
      'Toft: next.config sets output: "' + cfg.output + '", which does not apply here. ' +
        'The app is started with "next start" (a standalone build is never used, and "export" ' +
        'cannot build route handlers). Remove the setting.'
    )
  }
  return { ...cfg, basePath }
}

const merge = (resolved) =>
  resolved && typeof resolved.then === 'function' ? resolved.then(withBasePath) : withBasePath(resolved)
`

func commonJSWrapper(userName, basePath string) string {
	return fmt.Sprintf(`const basePath = %q
let userConfig = require('./%s')
userConfig = userConfig && userConfig.default ? userConfig.default : userConfig

%s
module.exports = (phase, ctx) => merge(typeof userConfig === 'function' ? userConfig(phase, ctx) : userConfig)
`, basePath, userName, mergeBody)
}

func esmWrapper(userName, basePath string) string {
	// The extension is omitted for .ts so Next's own loader resolves it; for
	// .mjs it must be explicit.
	spec := "./" + userName
	if filepath.Ext(userName) == ".ts" {
		spec = "./" + userConfigMarker
	}

	return fmt.Sprintf(`import userConfig from '%s'

const basePath = %q

%s
export default (phase, ctx) => merge(typeof userConfig === 'function' ? userConfig(phase, ctx) : userConfig)
`, spec, basePath, mergeBody)
}
